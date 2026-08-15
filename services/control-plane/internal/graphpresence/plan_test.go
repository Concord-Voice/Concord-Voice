package graphpresence

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
)

func TestNilPlanIsBenignTerminal(t *testing.T) {
	var p *Plan
	assert.False(t, p.HasWork(), "nil plan must not report work")
	assert.False(t, p.Degraded(), "nil plan must not report degraded")
}

func TestEmptyPlanHasNoWork(t *testing.T) {
	p := &Plan{subject: presencecapture.Subject{Family: presencecapture.FamilyBlock}}
	assert.False(t, p.HasWork(), "plan with no legs and no viewers must not report work")
}

func TestPlanWithCapturedAudienceHasWork(t *testing.T) {
	sender, viewer := uuid.New(), uuid.New()
	p := &Plan{active: []activeLeg{{
		senderID: sender,
		scope:    presence.Scope{Category: presence.CategoryServerVoice},
		captured: map[uuid.UUID]bool{viewer: true},
	}}}
	assert.True(t, p.HasWork(), "plan with a captured audience must report work")
}

// A degraded plan carries only the conservative viewer superset, and that
// superset alone counts as work — it is what Complete will disconnect.
func TestDegradedPlanReportsWorkAndCause(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	p := &Plan{
		degraded: true,
		cause:    causeAudienceRead,
		viewers:  map[uuid.UUID]bool{a: true, b: true},
	}
	assert.True(t, p.Degraded(), "degraded plan must report Degraded")
	assert.True(t, p.HasWork(), "degraded plan with viewers must report work")
	assert.NotEqual(t, causeNone, p.cause,
		"degraded plan must carry a non-zero fixed-enum cause")
}

// capturedAudience is the union Complete disconnects on an unresolved commit.
func TestCapturedAudienceUnionsLegsAndViewers(t *testing.T) {
	s, v1, v2 := uuid.New(), uuid.New(), uuid.New()
	p := &Plan{
		active:  []activeLeg{{senderID: s, captured: map[uuid.UUID]bool{v1: true}}},
		viewers: map[uuid.UUID]bool{v2: true},
	}
	assert.Equal(t, map[uuid.UUID]bool{v1: true, v2: true}, p.capturedAudience(),
		"capturedAudience must be the union of every leg and the viewer set")
}

func TestBoundsAreConstantsNotConfig(t *testing.T) {
	assert.Equal(t, 8, maxFocalSenders, "maxFocalSenders must be 8")
	assert.Equal(t, 5000, maxCapturedViewers, "maxCapturedViewers must be 5000")
}
