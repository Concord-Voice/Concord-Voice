package media

import (
	"context"
	"database/sql"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// ObjectDeleter is the subset of ObjectStore needed for media cleanup.
type ObjectDeleter interface {
	DeleteObject(ctx context.Context, key string) error
}

// CleanupObject removes a media object from storage and soft-deletes its metadata row.
// Best-effort: logs warnings but does not return errors — the caller's operation should
// not fail because of a cleanup issue.
// The DB soft-delete runs regardless of whether store is configured (store may be nil).
func CleanupObject(ctx context.Context, db *sql.DB, store ObjectDeleter, log *logger.Logger, storageKey string) {
	if store != nil {
		if err := store.DeleteObject(ctx, storageKey); err != nil {
			log.Warn("Failed to delete media object from storage", "error", err, "key", storageKey)
		}
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE media_files SET deleted_at = NOW() WHERE storage_key = $1 AND deleted_at IS NULL`,
		storageKey,
	); err != nil {
		log.Warn("Failed to soft-delete media metadata", "error", err, "key", storageKey)
	}
}
