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
