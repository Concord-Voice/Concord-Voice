package users

import (
	"context"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
)

// KeyResetSessionDisconnectorForTest is the test-only destructive-reset
// session termination seam.
type KeyResetSessionDisconnectorForTest interface {
	DisconnectUser(uuid.UUID)
}

// SetKeyResetSessionDisconnectorForTest installs a deterministic session
// termination test double.
func SetKeyResetSessionDisconnectorForTest(h *Handler, disconnector KeyResetSessionDisconnectorForTest) {
	h.sessionDisconnector = disconnector
}

// ReclaimErasedMediaForTest exposes the post-commit object-storage reclamation
// so a test can drive it with an already-cancelled parent context.
//
// Exercised directly rather than through DeleteAccount because the window this
// guards is unreachable from outside: cancelling BEFORE the call fails
// BeginTx (no erasure, nothing to reclaim), and cancelling DURING it means
// racing a commit. The unit is what carries the detachment, so the unit is what
// the test drives.
func ReclaimErasedMediaForTest(
	ctx context.Context, s *AccountService, tier1, tier2 []media.BlobRef,
) {
	s.reclaimErasedMedia(ctx, erasedMedia{tier1: tier1, tier2: tier2})
}
