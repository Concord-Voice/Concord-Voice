package messages

import (
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// SetBeforeSoftLockConfirmHookForTest installs the delete-rate soft-lock's
// ordering seam (#3455) for the external test package: f runs after the
// unlocked population read and before the transaction that re-reads it under
// lock. This file compiles only into test binaries, so production code has no
// way to set the hook.
func (h *Handler) SetBeforeSoftLockConfirmHookForTest(f func()) { h.beforeSoftLockConfirmHook = f }

// ConfirmOutsidePopulationForTest runs the soft-lock confirmation, carrying no
// factor, for an actor neither rule covers: the server read as not enforcing
// and the own rule does not apply. No route builds that gate, because such an
// actor is never charged, so this is the only way to pin that a caller which
// confirms outside the population is refused rather than waved through (#3455).
func (h *Handler) ConfirmOutsidePopulationForTest(ctx context.Context, userID, serverID string) (bool, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := h.confirmSoftLockTx(ctx, tx, softLockGate{userID: userID, serverID: serverID, purpose: stepup.PurposeMessageDelete})
	return res.outcome.Confirmed(), err
}
