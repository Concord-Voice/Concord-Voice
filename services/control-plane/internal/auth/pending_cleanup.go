package auth

import (
	"context"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// StartPendingCleanupWorker runs a goroutine that periodically deletes
// expired pending_registrations rows. Stops cleanly when ctx is cancelled.
func StartPendingCleanupWorker(
	ctx context.Context,
	repo *PendingRepo,
	log *logger.Logger,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		if n, err := repo.DeleteExpired(ctx); err != nil {
			log.Warn("pending cleanup: initial sweep failed", "error", err)
		} else if n > 0 {
			log.Info("pending cleanup: startup sweep", "deleted", n)
		}
		for {
			select {
			case <-ctx.Done():
				log.Info("pending cleanup worker stopped")
				return
			case <-ticker.C:
				if n, err := repo.DeleteExpired(ctx); err != nil {
					log.Warn("pending cleanup: sweep failed", "error", err)
				} else if n > 0 {
					log.Info("pending cleanup: deleted expired rows", "deleted", n)
				}
			}
		}
	}()
}
