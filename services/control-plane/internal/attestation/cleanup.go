package attestation

import (
	"context"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// retentionPruner is the subset of *Repository that Cleanup needs.
// *Repository satisfies it structurally; tests may supply a fake.
type retentionPruner interface {
	PruneRetention(ctx context.Context, now time.Time) error
}

// Cleanup drives periodic retention enforcement against the attestation registry.
// It ticks on the given interval and delegates to PruneRetention on each tick.
type Cleanup struct {
	repo retentionPruner
	log  *logger.Logger
}

// NewCleanup wires a Cleanup against the given dependencies.
// repo is typically a *Repository; tests may pass any retentionPruner.
func NewCleanup(repo retentionPruner, log *logger.Logger) *Cleanup {
	return &Cleanup{repo: repo, log: log}
}

// Run executes PruneRetention on the given interval until ctx is done.
// It is designed to be called in its own goroutine.
func (c *Cleanup) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := c.repo.PruneRetention(ctx, now); err != nil {
				c.log.With(
					"event", "attestation.retention_cleanup_failed",
					"error", err.Error(),
				).Error("attestation retention cleanup failed")
			} else {
				c.log.With("event", "attestation.retention_cleanup_succeeded").
					Info("attestation retention cleanup ran")
			}
		}
	}
}
