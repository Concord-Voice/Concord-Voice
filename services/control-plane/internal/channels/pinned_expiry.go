package channels

import (
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
)

// SetPinnedExpirer wires the terminal that deletes a channel's already-due
// pinned messages when its policy stops keeping pins (#3459 §8). Without it a
// toggle-on still commits, and the sweeper deletes those rows within one
// pass instead of before the PATCH answers, which is the silent degradation
// the router's requirePinnedExpiryWired exists to refuse at boot.
func (h *Handler) SetPinnedExpirer(e *expiration.ContextExpirer) { h.pinnedExpirer = e }

// HasPinnedExpiryRunner reports whether SetPinnedExpirer was called with a
// non-nil expirer.
func (h *Handler) HasPinnedExpiryRunner() bool { return h != nil && h.pinnedExpirer != nil }

// expireDuePinnedMessages runs the pinned-expiry terminal after COMMIT, after
// the expiration broadcast, and before the response (§8 steps 3). It is
// synchronous so "right away" means before the 200; the expirer detaches the
// run from the request's cancellation and bounds it itself, so a client that
// hangs up cannot cut a deletion short. Its failures are logged inside the
// expirer under a fixed class, and change nothing about the response: the
// policy has committed and the sweeper deletes what this leaves.
func (h *Handler) expireDuePinnedMessages(ctx context.Context, channelID, serverID string, transition expiration.Transition) {
	if len(transition.DuePinnedIDs) == 0 {
		return
	}
	h.pinnedExpirer.ExpireChannelPins(ctx, channelID, serverID, transition)
}
