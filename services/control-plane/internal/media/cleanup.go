package media

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ObjectDeleter is the subset of ObjectStore needed for media cleanup.
type ObjectDeleter interface {
	DeleteObject(ctx context.Context, key string) error
}

// RowQuerier is implemented by both *sql.DB and *sql.Tx.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DBTX is the database surface CleanupObject needs. Keeping the cleanup on the
// caller's transaction lets profile updates serialize their metadata erasure.
type DBTX interface {
	RowQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ProfileSlotAvatar and ProfileSlotBanner identify the two canonical profile
// endpoints backed by immutable physical objects.
const (
	ProfileSlotAvatar = "avatar"
	ProfileSlotBanner = "banner"
)

// TerminalizeProfileSlot records every physical generation that a profile slot
// can still name before removing the metadata and upload intents. Callers hold
// the user row lock, which serializes this with intent creation and publication.
func TerminalizeProfileSlot(ctx context.Context, tx *sql.Tx, userID, profileSlot string) error {
	if profileSlot != ProfileSlotAvatar && profileSlot != ProfileSlotBanner {
		return fmt.Errorf("terminalize profile slot: invalid slot %q", profileSlot)
	}
	legacyKey := "avatars/" + userID
	if profileSlot == ProfileSlotBanner {
		legacyKey = "banners/" + userID
	}
	if err := ensureProfileSlotUsesLegacyBackend(ctx, tx, userID, profileSlot, legacyKey); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tier1_erasure_delete_obligations (storage_key)
		SELECT storage_key
		FROM tier1_profile_upload_intents
		WHERE user_id = $1 AND profile_slot = $2
		UNION
		SELECT storage_key
		FROM media_files
		WHERE uploader_id = $1
		  AND media_tier = 1
		  AND deleted_at IS NULL
		  AND (profile_slot = $2 OR storage_key = $3)
		UNION
		SELECT $3
		ON CONFLICT (storage_key) DO NOTHING`, userID, profileSlot, legacyKey); err != nil {
		return fmt.Errorf("terminalize profile slot: record deletion obligations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tier1_profile_upload_intents WHERE user_id = $1 AND profile_slot = $2`,
		userID, profileSlot,
	); err != nil {
		return fmt.Errorf("terminalize profile slot: delete upload intents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE media_files
		SET deleted_at = NOW()
		WHERE uploader_id = $1
		  AND media_tier = 1
		  AND deleted_at IS NULL
		  AND (profile_slot = $2 OR storage_key = $3)`, userID, profileSlot, legacyKey); err != nil {
		return fmt.Errorf("terminalize profile slot: retire media metadata: %w", err)
	}
	return nil
}

// ensureProfileSlotUsesLegacyBackend preserves CleanupObject's placement
// invariant before a permanent tombstone makes a profile key unservable. The
// caller holds the user lock; this check refuses rather than recording an
// erasure against a bucket the legacy reclaimer cannot safely delete from.
func ensureProfileSlotUsesLegacyBackend(ctx context.Context, tx *sql.Tx, userID, profileSlot, legacyKey string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT storage_backend
		FROM media_files
		WHERE uploader_id = $1
		  AND media_tier = 1
		  AND deleted_at IS NULL
		  AND (profile_slot = $2 OR storage_key = $3)`, userID, profileSlot, legacyKey)
	if err != nil {
		return fmt.Errorf("terminalize profile slot: check storage backend: %w", err)
	}
	for rows.Next() {
		var backend *string
		if err := rows.Scan(&backend); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return errors.Join(
					fmt.Errorf("terminalize profile slot: scan storage backend: %w", err),
					fmt.Errorf("terminalize profile slot: close storage backend rows: %w", closeErr),
				)
			}
			return fmt.Errorf("terminalize profile slot: scan storage backend: %w", err)
		}
		if !isLegacyBackend(backend) {
			if closeErr := rows.Close(); closeErr != nil {
				return errors.Join(
					errors.New("terminalize profile slot: refused non-legacy backend"),
					fmt.Errorf("terminalize profile slot: close storage backend rows: %w", closeErr),
				)
			}
			return errors.New("terminalize profile slot: refused non-legacy backend")
		}
	}
	if err := rows.Err(); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return errors.Join(
				fmt.Errorf("terminalize profile slot: read storage backend rows: %w", err),
				fmt.Errorf("terminalize profile slot: close storage backend rows: %w", closeErr),
			)
		}
		return fmt.Errorf("terminalize profile slot: read storage backend rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("terminalize profile slot: close storage backend rows: %w", err)
	}
	return nil
}

// liveMediaBackendQuery reads the placement of the LIVE row for a storage key.
// At most one row can match: migration 000042's unique index on storage_key is
// partial on `deleted_at IS NULL`.
const liveMediaBackendQuery = `SELECT storage_backend FROM media_files
	         WHERE storage_key = $1 AND deleted_at IS NULL`

// cleanupSoftDeleteQuery soft-deletes the live row for a storage key.
//
// PAIR-KEYED on (storage_key, storage_backend) rather than the key alone. The
// backend predicate is a compare-and-swap on the placement CleanupObject
// validated a moment earlier: if the row's placement changed underneath us the
// statement matches nothing rather than recording an erasure against a backend
// nobody deleted from. `IS NOT DISTINCT FROM` (not `=`) because NULL is the
// permanent, overwhelmingly common value and `= NULL` is never true.
const cleanupSoftDeleteQuery = `UPDATE media_files SET deleted_at = NOW()
	         WHERE storage_key = $1
	           AND storage_backend IS NOT DISTINCT FROM $2::text
	           AND deleted_at IS NULL`

// CleanupObject removes a media object from storage and soft-deletes its metadata row.
// It returns cleanup failures so transactional callers can roll back, while
// post-commit callers can retain best-effort behavior. A failed object deletion
// leaves metadata live as the only available retry signal.
//
// PLACEMENT (ADR-0038 / #2759 unit B2). This is the TIER-1 cleanup path: its
// only callers hand it profile-media keys (avatars/, server-banners/,
// server-icons/, dm-icons/), and ADR-0038 keeps ALL profile media on MinIO
// unconditionally and forever. So `store` — the process-wide legacy client — is
// the correct target for every key that legitimately reaches here, and this
// function deliberately grows NO vendor delete path.
//
// What it does grow is the refusal. It reads the row's storage_backend first
// and, if that names anything other than the legacy backend, deletes nothing
// and records nothing. Two reasons, and the second is the one that matters:
//
//   - Deleting from `store` would hit the wrong bucket, and an S3 DELETE of an
//     absent key SUCCEEDS — so the failure would be silent.
//   - Unlike the tier-2 paths, there is NO retry behind this one. The straggler
//     sweep is bounded to `media_tier = 2` (see internal/purge/reaper.go), so a
//     tier-1 row soft-deleted without its object actually being gone is never
//     revisited by anything. Recording that erasure is therefore permanent, and
//     for a third-party-resident object it is an erasure record that does not
//     match reality. Refusing loudly is the only honest answer available here.
//
// A non-legacy value arriving here is an ADR-0038 violation upstream, not a
// case to be handled. Returning an error keeps its metadata live and lets the
// caller surface the failure; no vendor delete path is added here.
func CleanupObject(ctx context.Context, db DBTX, store ObjectDeleter, storageKey string) error {
	backend, found, err := liveMediaBackend(ctx, db, storageKey)
	if err != nil {
		// Placement unknown. Do not delete (we would be guessing at a bucket)
		// and do not soft-delete (that would record an erasure nothing
		// performed). Nothing changes, so the caller's next attempt retries.
		return fmt.Errorf("read media storage backend: %w", err)
	}
	if found && !isLegacyBackend(backend) {
		return errors.New("media cleanup refused non-legacy backend")
	}

	if store != nil {
		if err := store.DeleteObject(ctx, storageKey); err != nil {
			return fmt.Errorf("delete media object: %w", err)
		}
	}

	if !found {
		// No live row: nothing to soft-delete. The object delete above still
		// ran, which is the pre-existing behaviour — a caller may legitimately
		// hand us a key whose row was already retired, and skipping the delete
		// would leak the object with nothing to sweep it.
		return nil
	}
	result, err := db.ExecContext(ctx, cleanupSoftDeleteQuery, storageKey, backend)
	if err != nil {
		return fmt.Errorf("soft-delete media metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read media metadata cleanup result: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("soft-delete media metadata: affected %d rows, want 1", affected)
	}
	return nil
}

// liveMediaBackend reads the storage_backend of the live row for storageKey.
// found is false when no live row exists, which is not an error.
func liveMediaBackend(ctx context.Context, db RowQuerier, storageKey string) (backend *string, found bool, err error) {
	err = db.QueryRowContext(ctx, liveMediaBackendQuery, storageKey).Scan(&backend)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return backend, true, nil
}

// --- account-erasure reclamation (#2759 follow-on) -------------------------

// ErasableTier1Keys returns the tier-1 storage keys whose SUBJECT is this user,
// and therefore the only tier-1 keys that become unreferenced when the user is
// erased.
//
// THE OMISSION IS THE FEATURE. tier1StorageKey mints five shapes and only these
// two are subject-scoped to a user. The other three -- `server-icons/<serverID>`,
// `server-banners/<serverID>`, `dm-icons/<conversationID>` -- are scoped to a
// SERVER or a CONVERSATION that outlives the person who uploaded them, and
// media_files.uploader_id does not record ownership of those: setting a server
// icon needs PermManageServer (a moderator, not necessarily the owner), and
// insertTier1Record's `ON CONFLICT ... DO UPDATE SET uploader_id` REBINDS the
// row to whoever changed the icon last. So an erasure driven by uploader_id
// alone would delete the icon of a live server whose owner is untouched, and
// because proxyTier1Media serves these by key without ever reading media_files,
// nothing else would notice until the icon was gone.
//
// A future subject-scoped tier-1 purpose belongs here; a shared one never does.
func ErasableTier1Keys(userID string) []string {
	return []string{
		tier1StorageKey(purposeAvatar, userID, "", ""),
		tier1StorageKey(purposeBanner, userID, "", ""),
	}
}
