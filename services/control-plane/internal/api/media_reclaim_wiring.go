package api

import (
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// newErasedMediaReclaimer builds the account-erasure object-storage discharge
// (#2759 follow-on). It lives here, as a named function, rather than as an
// inline closure in NewRouter, for ONE reason: as a closure the leg routing was
// untestable, and swapping its two lines was invisible to the entire repo.
//
// That mutant is not cosmetic. It routes TIER-1 keys into EnqueueBlobDeletes,
// whose contract explicitly forbids them -- the purge worker deletes
// unconditionally, without the live-key guard reapSweptBlob applies, and tier-1
// keys are deterministic per subject. So the swapped version deletes an object a
// LIVE row still points at, which is precisely the failure the two-rail split
// exists to prevent. Deleting the wiring altogether was equally invisible,
// because a nil reclaimer is a deliberate degrade with no boot guard.
//
// enqueue is taken as a func rather than the *purge.Reaper so this package gains
// no purge import and the test double stays a one-liner.
func newErasedMediaReclaimer(
	store media.ObjectDeleter,
	enqueue func([]media.BlobRef),
	log *logger.Logger,
) users.ErasedMediaReclaimer {
	return func(ctx context.Context, tier1, tier2 []media.BlobRef) {
		// Tier 1 NEVER goes through enqueue; tier 2 NEVER goes through
		// ReclaimErasedTier1. See the doc comment above for what swapping costs.
		media.ReclaimErasedTier1(ctx, store, log, tier1)
		enqueue(tier2)
	}
}
