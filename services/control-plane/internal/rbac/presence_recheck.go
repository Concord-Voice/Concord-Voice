package rbac

import (
	"context"
	"database/sql"
	"errors"
)

// PresenceRecheckPlan is the opaque pre-mutation Rich Presence capture produced
// inside an authority transaction and consumed after it commits. internal/rbac
// never inspects it.
type PresenceRecheckPlan interface {
	// HasWork reports whether any active sender was captured. An empty plan is
	// the benign terminal: no dispatch, no disconnect, HTTP 200.
	HasWork() bool
}

// PresenceRecheck reconciles active Server Voice Rich Presence against an
// RBAC/SBAC visibility mutation (#2445). It is declared here at the consumer
// (the VoiceEnforcer precedent) so internal/rbac gains ZERO presence imports.
// That is load-bearing twice over: it keeps the implementation swappable for
// #2635's shared enforcement outbox, AND it keeps the internal/presence test
// binary compiling — internal/presence/policy_integration_test.go imports
// internal/rbac, so an rbac→presence import would cycle the test binary.
//
// Capture is deliberately TWO-PHASE. pg_advisory_xact_lock is held from the
// moment it executes until COMMIT, so anything called between the lock and the
// write runs UNDER the lock regardless of which connection it uses. Splitting
// the phases is what keeps advisory-lock hold time O(#affected channels)
// instead of O(#senders) — and a channel admits up to 1000 participants, so on
// a busy channel the collapsed form would hold a lock that serializes EVERY
// RBAC mutation on the server for ~1000 sequential round trips. Do not merge
// the two methods.
type PresenceRecheck interface {
	// PrepareCapture is phase 1 — PRE-TRANSACTION. It enumerates the active
	// Server Voice senders in scope and resolves each sender's candidate half
	// (settings, tier, friends/FoF, server membership, base presence). Those
	// inputs are mutated by NO hooked RBAC write, so resolving them outside the
	// transaction is sound and it is what keeps the per-sender round trips off
	// the advisory lock entirely.
	//
	// channelIDs nil means "every active voice channel in the server"; the
	// implementation enumerates them. onlyUserID non-nil bounds the phase-2
	// visibility-filter input to that one affected user (the assign/unassign
	// and temp-SBAC scope) — sound because only that user's permission inputs
	// changed. Candidate SETS are never pruned by mutation shape.
	//
	// A non-nil error MUST be returned to the caller BEFORE BeginTx: the
	// permission write never happens, nothing changed, nothing disclosed,
	// retryable (§8 class 1).
	PrepareCapture(
		ctx context.Context, serverID string, channelIDs []string, onlyUserID *string,
	) (PresenceRecheckPlan, error)
	// CaptureVisibility is phase 2 — IN-TRANSACTION, after the advisory lock
	// and BEFORE the write. It runs exactly one FilterVisibleUserIDsForChannelTx
	// per affected channel over the union of that channel's senders' candidate
	// sets and intersects the result into the plan, yielding the exact
	// pre-write authorized audience. A non-nil error MUST roll the transaction
	// back (§8 class 2).
	CaptureVisibility(ctx context.Context, tx *sql.Tx, plan PresenceRecheckPlan) error
	// Execute dispatches the plan post-commit, fire-and-forget on the
	// executor's own lifecycle context. It never blocks the request.
	Execute(plan PresenceRecheckPlan)
	// Abandon is the fail-closed terminal: a viewer-scoped disconnect of the
	// captured audience. Used for an ambiguous commit and for an unreachable
	// dispatch. A disconnect discloses nothing; reconnect rebuilds from
	// committed state.
	Abandon(plan PresenceRecheckPlan, cause string)
}

// SetPresenceRecheck wires the post-mutation Rich Presence reconciliation.
// Called once at router construction, before the handler serves traffic. A nil
// recheck leaves every helper a no-op — the pre-#2445 behavior — which is why
// router.go fatal-exits when the activity service exists and this does not.
func (h *Handler) SetPresenceRecheck(p PresenceRecheck) {
	h.presenceRecheck = p
}

// ErrPresenceCaptureLimited marks a capture refused because its channel
// fan-out exceeded the executor's bound. It is declared HERE, at the consumer,
// for the same reason the PresenceRecheck interface is: internal/rbac must gain
// zero presence imports, so it cannot reference the implementation's own
// sentinel. voicepresence wraps this one (it already imports internal/rbac),
// which lets withAuthorityCapture classify the failure with errors.Is.
//
// It exists for LOGS, not for responses — see the note at its use site in
// authority_tx.go on why the wire response stays byte-identical.
var ErrPresenceCaptureLimited = errors.New("rich-presence capture fan-out bound exceeded")

// HasPresenceRecheck reports whether SetPresenceRecheck actually ran with a
// non-nil executor. It exists so api.requirePresenceRecheckWired can assert on
// wiring rather than on the executor value the caller holds: NewExecutor never
// returns nil, so a check on that value is a tautology and would still boot
// cleanly with the SetPresenceRecheck line deleted — the one fail-open path the
// guard exists to catch (#2445 review).
func (h *Handler) HasPresenceRecheck() bool {
	return h.presenceRecheck != nil
}

func (h *Handler) preparePresenceCapture(
	ctx context.Context,
	serverID string,
	channelIDs []string,
	onlyUserID *string,
) (PresenceRecheckPlan, error) {
	if h.presenceRecheck == nil {
		return nil, nil
	}
	return h.presenceRecheck.PrepareCapture(ctx, serverID, channelIDs, onlyUserID)
}

func (h *Handler) capturePresenceVisibility(
	ctx context.Context,
	tx *sql.Tx,
	plan PresenceRecheckPlan,
) error {
	if h.presenceRecheck == nil || plan == nil {
		return nil
	}
	return h.presenceRecheck.CaptureVisibility(ctx, tx, plan)
}

func (h *Handler) presenceExecute(plan PresenceRecheckPlan) {
	if h.presenceRecheck == nil || plan == nil || !plan.HasWork() {
		return
	}
	h.presenceRecheck.Execute(plan)
}

func (h *Handler) presenceAbandon(plan PresenceRecheckPlan, cause string) {
	if h.presenceRecheck == nil || plan == nil || !plan.HasWork() {
		return
	}
	h.presenceRecheck.Abandon(plan, cause)
}
