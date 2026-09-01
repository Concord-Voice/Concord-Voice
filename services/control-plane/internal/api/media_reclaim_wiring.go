package api

import (
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
)

// newDurableErasedMediaReclaimer wakes Tier-1 retry processing and enqueues
// Tier-2 objects only. Tier-1 refs are intentionally not direct-delete input.
func newDurableErasedMediaReclaimer(
	wake func(),
	enqueue func([]media.BlobRef),
) users.ErasedMediaReclaimer {
	return func(_ context.Context, _ []media.BlobRef, tier2 []media.BlobRef) {
		if wake != nil {
			wake()
		}
		enqueue(tier2)
	}
}
