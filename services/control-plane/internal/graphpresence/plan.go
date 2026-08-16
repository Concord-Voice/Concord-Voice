// Package graphpresence reconciles Rich Presence with graph-destroying writes
// (#2446 friendship/FoF). It implements presencecapture.GraphPresenceCapture
// and is the only package importing both internal/presence and the consumer
// contract.
//
// It does NOT import internal/rbac, and — unlike voicepresence — it applies NO
// channel-visibility filter at all. An earlier draft of this comment claimed one
// was taken as a locally-declared structural interface on the
// voicepresence.ChannelVisibilityFilterer precedent; no such interface or field
// exists here (PR #2738 review).
//
// The consequence is worth stating rather than leaving implicit: a leg's
// captured set is the CANDIDATE superset that presence.CaptureServerVoiceCandidates
// returns, not the pre-mutation *authorized* audience. That is safe for
// delivery, because ActivityService unions recheckViewers into the clear set
// and a superset only over-clears. It is NOT safe to hand to a disconnect,
// which is why Abandon is now bounded (see presencecapture.CauseProvesNoCommit
// and CaptureInTx's accepted-edge gate).
//
// What that candidate set actually contains: the SENDER'S SERVER MEMBERS,
// intersected with friends ∪ FoF at the default TierFriends and taken whole at
// TierServers (presence.serverVoiceCandidates). It is therefore always a SUBSET
// of the server's membership. An earlier version of this comment wrote it as
// "friends ∪ FoF ∪ server peers", which is a union where the code performs an
// intersection — it implies a non-member friend is captured, and a test written
// against that reading cannot pass because such a viewer is never in the set at
// all. It is "superset" only with respect to the missing channel-visibility
// filter, which is the sense that matters above.
package graphpresence

import (
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
)

// Bounds are consts, never configuration. A deployment that needs a different
// value has a design problem, and a config surface would invite raising the
// bound instead of fixing the derivation.
const (
	// maxFocalSenders bounds the senders whose own audience is reconciled.
	// Every family in #2446 yields 1-2; the headroom is for #2447. Exceeding it
	// means the focal-set derivation is wrong — a bug, not load — so it fails
	// CLOSED regardless of declared posture, including at BlockUser.
	maxFocalSenders = 8

	// maxCapturedViewers bounds the exact-delta viewer set, which is unbounded
	// in practice: at TierServers a popular sender's audience is every member
	// of the server. Above it, escalate to a full rich-presence disconnect for
	// the focal senders rather than attempting an exact delta.
	maxCapturedViewers = 5000
)

// degradeCause is a FIXED enum. It is never a wrapped database error and never
// a query-derived string, so the log line it produces cannot become a data leak
// ([internal]rules/observability.md).
type degradeCause uint8

const (
	causeNone degradeCause = iota
	causeAudienceRead
	causeActiveScopeRead
	causeBoundExceeded
)

func (c degradeCause) String() string {
	switch c {
	case causeAudienceRead:
		return "audience_read"
	case causeActiveScopeRead:
		return "active_scope_read"
	case causeBoundExceeded:
		return "bound_exceeded"
	default:
		return "none"
	}
}

// activeLeg is one focal sender's pre-mutation state for one active category.
type activeLeg struct {
	senderID uuid.UUID
	scope    presence.Scope
	captured map[uuid.UUID]bool
}

// Plan is the opaque capture handed back to consumer packages as
// presencecapture.Plan.
//
// It must never hold usernames, emails, avatar URLs, friendship rows, or any
// edge list, and exposes no exported audience accessor — an exported accessor
// is a social-graph read handed to whoever holds the plan.
type Plan struct {
	subject presencecapture.Subject

	// Exact path.
	active  []activeLeg
	viewers map[uuid.UUID]bool

	// Degraded path.
	degraded bool
	cause    degradeCause
}

var _ presencecapture.Plan = (*Plan)(nil)

// HasWork reports whether anything was captured.
func (p *Plan) HasWork() bool {
	if p == nil {
		return false
	}
	if len(p.viewers) > 0 {
		return true
	}
	for i := range p.active {
		if len(p.active[i].captured) > 0 {
			return true
		}
	}
	return false
}

// Degraded reports that this plan carries the conservative principal
// superset instead of an exact delta. TWO paths set it, not one: a capture
// read that failed under FailConservativeDegrade, and a capture that
// exceeded maxCapturedViewers. The second is a bound, not a failure — an
// earlier doc named only the first, which made the bound path read like a
// contract violation (PR #2738 review, CodeRabbit).
func (p *Plan) Degraded() bool {
	return p != nil && p.degraded
}

// capturedAudience is the union of every leg's captured audience and the
// peripheral viewer set — the exact set Abandon and an unresolved commit
// disconnect. Unexported by design.
func (p *Plan) capturedAudience() map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool)
	if p == nil {
		return out
	}
	for id := range p.viewers {
		out[id] = true
	}
	for i := range p.active {
		for id := range p.active[i].captured {
			out[id] = true
		}
	}
	return out
}
