package expiration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyFragment_EmbeddedByteForByteInEveryApplyStatement pins R2: the
// backfill batch, its eligibility probe, and the start reconcile must embed
// the ONE shared fragment. A statement that carries its own copy can drift,
// and a batch that writes a shape the probe does not recognise leaves
// backfill_pending set forever.
func TestApplyFragment_EmbeddedByteForByteInEveryApplyStatement(t *testing.T) {
	for _, s := range []scope{channelScope, conversationScope} {
		t.Run(s.name, func(t *testing.T) {
			for name, statement := range map[string]string{
				"applyBatchSQL":     s.applyBatchSQL,
				"applyEligibleSQL":  s.applyEligibleSQL,
				"reconcileApplySQL": s.reconcileApplySQL,
			} {
				assert.Equal(t, 1, strings.Count(statement, applyEligible), "%s must embed applyEligible exactly once", name)
			}
			for name, statement := range map[string]string{
				"applyBatchSQL":     s.applyBatchSQL,
				"reconcileApplySQL": s.reconcileApplySQL,
			} {
				assert.Equal(t, 1, strings.Count(statement, applyAssignments), "%s must embed applyAssignments exactly once", name)
			}
		})
	}
}

// TestApplyFragment_AssignmentsWriteTheShapeEligibilityCompares is the other
// half of R2: the assignments and the predicate are built from the same two
// target expressions, so a written row is exactly a row the predicate drops.
func TestApplyFragment_AssignmentsWriteTheShapeEligibilityCompares(t *testing.T) {
	assert.Contains(t, applyAssignments, "expires_at = "+applyTargetExpiresAt)
	assert.Contains(t, applyAssignments, "held_expires_at = "+applyTargetHeld)
	assert.Contains(t, applyEligible, "(expires_at, held_expires_at) IS DISTINCT FROM\n        ("+applyTargetExpiresAt+", "+applyTargetHeld+")")
}

// TestPinnedStatements_CarryPinnedIndexPredicate pins Q-SHAPE: every
// pinned-only statement that names held_expires_at also carries
// `pinned_at IS NOT NULL`, so it rides the partial pinned index instead of
// scanning the context's messages.
func TestPinnedStatements_CarryPinnedIndexPredicate(t *testing.T) {
	for _, s := range []scope{channelScope, conversationScope} {
		t.Run(s.name, func(t *testing.T) {
			for name, statement := range map[string]string{
				"reconcileApplySQL": s.reconcileApplySQL,
				"holdPinnedSQL":     s.holdPinnedSQL,
				"releaseHeldSQL":    s.releaseHeldSQL,
				"dropHeldSQL":       s.dropHeldSQL,
				"duePinnedSQL":      s.duePinnedSQL,
			} {
				assert.Contains(t, statement, "pinned_at IS NOT NULL", "%s is pinned-only and must carry the pinned index predicate", name)
			}
		})
	}
}

// TestMovePredicates_NeverMoveAStampAtOrBeforeTheCutoff pins C-NORES at the
// text level: every move out of expires_at is strictly after the cutoff. The
// integration boundary test proves the same thing against a row stamped at
// exactly the cutoff.
func TestMovePredicates_NeverMoveAStampAtOrBeforeTheCutoff(t *testing.T) {
	for _, s := range []scope{channelScope, conversationScope} {
		assert.Contains(t, s.holdPinnedSQL, "expires_at > $2", s.name)
		assert.NotContains(t, s.holdPinnedSQL, ">=", s.name)
	}
	assert.Contains(t, applyEligible, "(expires_at IS NULL OR expires_at > $2)")
	assert.NotContains(t, applyEligible, ">=")
	for name, holds := range map[string]string{"channel": channelPinHolds, "conversation": conversationPinHolds} {
		assert.Contains(t, holds, "expires_at > statement_timestamp()", name)
		assert.NotContains(t, holds, ">=", name)
	}
}

// TestPinFragments_ReadOneStatementInstant pins X6: a pin's move reads the
// statement's own fixed instant in both CASE arms, never a clock that can
// advance between the arms and straddle the stamp.
func TestPinFragments_ReadOneStatementInstant(t *testing.T) {
	for name, fragment := range map[string]string{
		"channel":      ChannelPinHoldAssignments,
		"conversation": ConversationPinHoldAssignments,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, 2, strings.Count(fragment, "statement_timestamp()"), "both CASE arms test the same instant")
			assert.NotContains(t, fragment, "clock_timestamp()")
			assert.NotContains(t, fragment, "now()")
			assert.NotContains(t, fragment, "NOW()")
			assert.True(t, strings.HasPrefix(fragment, "expires_at = "), "fragment is a SET-list tail")
			assert.Contains(t, fragment, "held_expires_at = ")
		})
	}
	assert.Equal(t, "expires_at = COALESCE(expires_at, held_expires_at),\n    held_expires_at = NULL", UnpinReleaseAssignments)
}

func TestPolicy_KeepsPins(t *testing.T) {
	window := 3600
	for _, tc := range []struct {
		name   string
		policy Policy
		want   bool
	}{
		{"no window keeps nothing", Policy{IncludePinned: false}, false},
		{"window excluding pins keeps them", Policy{WindowSeconds: &window, IncludePinned: false}, true},
		{"window including pins keeps nothing", Policy{WindowSeconds: &window, IncludePinned: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, tc.policy.KeepsPins()) })
	}
}

func TestPolicy_IncludePinnedAlwaysOnTheWire(t *testing.T) {
	body, err := json.Marshal(Policy{})
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &fields))
	raw, ok := fields["include_pinned"]
	require.True(t, ok, "include_pinned must be present even when false")
	assert.Equal(t, "false", string(raw))
}

func TestIncludeUpdate_WritesOnlyOnSet(t *testing.T) {
	window := 3600
	for _, tc := range []struct {
		name        string
		request     Request
		wantInclude any
		wantSet     bool
	}{
		{"set omitted", Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"}, nil, true},
		{"set true", Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(true)}, true, true},
		{"set false", Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply", IncludePinned: ptr(false)}, false, true},
		{"clear", Request{Mode: "clear", Retroactive: "leave_pending"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			include, isSet := includeUpdate(tc.request)
			assert.Equal(t, tc.wantInclude, include)
			assert.Equal(t, tc.wantSet, isSet)
		})
	}
}
