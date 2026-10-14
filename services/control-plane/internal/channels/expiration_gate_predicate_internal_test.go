package channels

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
)

func gatePredicateBool(v bool) *bool { return &v }

func gatePredicateInt(v int) *int { return &v }

// expirationGateFires over every include_pinned shape against a prior window
// that the new one does not shorten, so the include arm is the only one that
// can fire (#3459 §7, AC7).
func TestExpirationGateFires_IncludePinnedArm(t *testing.T) {
	prior := sql.NullInt64{Int64: 86400, Valid: true}
	for _, tc := range []struct {
		name          string
		priorInclude  bool
		include       *bool
		wantFiresGate bool
	}{
		{"false to true fires", false, gatePredicateBool(true), true},
		{"true to true does not", true, gatePredicateBool(true), false},
		{"true to false does not", true, gatePredicateBool(false), false},
		{"false to false does not", false, gatePredicateBool(false), false},
		{"omitted over false does not", false, nil, false},
		{"omitted over true does not", true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locked := lockedExpirationChannel{channelType: "text", window: prior, includePinned: tc.priorInclude}
			request := expiration.Request{Mode: "set", WindowSeconds: gatePredicateInt(86400), Retroactive: "new_only", IncludePinned: tc.include}
			assert.Equal(t, tc.wantFiresGate, expirationGateFires(locked, request))
		})
	}
}

// The shortening arm still fires on its own, whatever include_pinned says.
func TestExpirationGateFires_ShorteningArmUnchanged(t *testing.T) {
	locked := lockedExpirationChannel{window: sql.NullInt64{Int64: 604800, Valid: true}, includePinned: true}
	request := expiration.Request{Mode: "set", WindowSeconds: gatePredicateInt(3600), Retroactive: "new_only", IncludePinned: gatePredicateBool(false)}
	assert.True(t, expirationGateFires(locked, request))
	request.WindowSeconds = gatePredicateInt(2592000)
	assert.False(t, expirationGateFires(locked, request))
}

// X5: mayFire is widened explicitly. Validate makes the include-only operand
// unreachable through the route today (set requires a window), so this pure
// table is the only place its removal can be observed.
//
// Mutant killed: mayFire reduced to `request.WindowSeconds != nil` (the
// include-only row reports false).
func TestExpirationGateMayFire(t *testing.T) {
	assert.True(t, expirationGateMayFire(expiration.Request{WindowSeconds: gatePredicateInt(3600)}))
	assert.True(t, expirationGateMayFire(expiration.Request{IncludePinned: gatePredicateBool(true)}), "include-only")
	assert.True(t, expirationGateMayFire(expiration.Request{IncludePinned: gatePredicateBool(false)}), "include-only, false")
	assert.False(t, expirationGateMayFire(expiration.Request{Mode: "clear", Retroactive: "leave_pending"}))
}
