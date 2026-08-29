package media

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ObjectDeleter is the subset of ObjectStore needed for media cleanup.
type ObjectDeleter interface {
	DeleteObject(ctx context.Context, key string) error
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
// Best-effort: logs warnings but does not return errors — the caller's operation should
// not fail because of a cleanup issue.
// The DB soft-delete runs regardless of whether store is configured (store may be nil).
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
// case to be handled: the right response is a loud ERROR an operator can act on
// (see the follow-on note in the unit report about a durable retry queue, which
// is deliberately NOT built here).
func CleanupObject(ctx context.Context, db *sql.DB, store ObjectDeleter, log *logger.Logger, storageKey string) {
	backend, found, err := liveMediaBackend(ctx, db, storageKey)
	if err != nil {
		// Placement unknown. Do not delete (we would be guessing at a bucket)
		// and do not soft-delete (that would record an erasure nothing
		// performed). Nothing changes, so the caller's next attempt retries.
		log.Error("Failed to read the storage backend for a media object; skipping cleanup",
			"error", err, "key", storageKey)
		return
	}
	if found && !isLegacyBackend(backend) {
		log.Error("Refusing to clean up media held by a non-legacy storage backend: profile media is MinIO-resident by ADR-0038, and this path has no retry behind it",
			"key", storageKey, "storage_backend", describeBackend(backend))
		return
	}

	if store != nil {
		if err := store.DeleteObject(ctx, storageKey); err != nil {
			log.Warn("Failed to delete media object from storage", "error", err, "key", storageKey)
		}
	}

	if !found {
		// No live row: nothing to soft-delete. The object delete above still
		// ran, which is the pre-existing behaviour — a caller may legitimately
		// hand us a key whose row was already retired, and skipping the delete
		// would leak the object with nothing to sweep it.
		return
	}
	if _, err := db.ExecContext(ctx, cleanupSoftDeleteQuery, storageKey, backend); err != nil {
		log.Warn("Failed to soft-delete media metadata", "error", err, "key", storageKey)
	}
}

// liveMediaBackend reads the storage_backend of the live row for storageKey.
// found is false when no live row exists, which is not an error.
func liveMediaBackend(ctx context.Context, db *sql.DB, storageKey string) (backend *string, found bool, err error) {
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

// ReclaimErasedTier1 deletes the profile-media objects an erased account's own
// keys named.
//
// It does NOT route through CleanupObject, and the reason is timing rather than
// preference. This runs POST-COMMIT, after the ON DELETE CASCADE has already
// hard-deleted the rows, so CleanupObject's liveMediaBackend read would find
// nothing every single time -- a guaranteed-empty query whose answer the caller
// has already captured under the user-row lock. The placement check that read
// exists to perform is done here instead, against the captured value, which is
// the only place it can still be performed truthfully.
//
// Post-commit and not pre-: deleting the object first would leave a transaction
// free to roll back onto an account whose avatar is already gone.
func ReclaimErasedTier1(ctx context.Context, store ObjectDeleter, log *logger.Logger, refs []BlobRef) {
	for _, ref := range refs {
		if !isLegacyBackend(ref.Backend) {
			// ADR-0038 pins ALL profile media to the legacy backend
			// permanently, so this is an upstream violation rather than a case
			// to handle. Deleting from `store` anyway would hit the wrong
			// bucket and SUCCEED (an S3 DELETE of an absent key returns
			// success), recording an erasure that did not happen with no retry
			// behind it -- the straggler sweep is bounded to media_tier = 2.
			log.Error("account erasure: refusing to reclaim profile media held by a non-legacy backend",
				"storage_key", ref.Key, "storage_backend", ref.BackendLabel())
			continue
		}
		if store == nil {
			// No object storage in THIS process -- which is emphatically not
			// "nothing was ever written". An earlier revision said that, and it
			// was backwards: every ref here came from a LIVE media_files row
			// (erasedMediaQuery), and a row exists only because some process
			// successfully wrote the object. So the bytes are there, this
			// replica cannot reach them, and nothing else will -- the straggler
			// sweep and the orphan reaper are both media_tier = 2. That is a
			// configuration fault causing permanent plaintext retention on a
			// GDPR path, hence Error rather than Warn.
			log.Error("account erasure: object storage is not configured; profile media NOT reclaimed and unrecoverable",
				"storage_key", ref.Key)
			continue
		}
		if err := store.DeleteObject(ctx, ref.Key); err != nil {
			// ERROR, not Warn, and the severity is the finding: this is the one
			// branch in this file with an IRREVERSIBLE consequence. The other
			// three refusals above change nothing and leave the caller free to
			// retry; this one has already lost the row, so no sweep will ever
			// revisit the key -- the straggler sweep and the orphan reaper are
			// both tier-2, because a row-less tier-1 object is indistinguishable
			// from a live server icon. The bytes are plaintext and they stay.
			//
			// Review suggested also logging the erased user id, so an operator
			// could reconcile the residue against users.avatar_url later. It is
			// already there: every key this function can receive is
			// `avatars/<userID>` or `banners/<userID>` by construction
			// (ErasableTier1Keys), so the id IS the key's suffix. Threading the
			// subject through the reclaimer signature would duplicate it.
			log.Error("account erasure: failed to delete profile media object; PLAINTEXT bytes remain and nothing will retry",
				"error", err, "storage_key", ref.Key)
		}
	}
}
