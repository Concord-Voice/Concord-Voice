package voicepresence

import (
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scopeFor(channelID uuid.UUID) presence.Scope {
	return presence.Scope{
		Category:    presence.CategoryServerVoice,
		RoomID:      channelID,
		LifecycleID: channelID,
		EventAt:     time.Unix(1_700_000_000, 0).UTC(),
	}
}

func TestPlan_HasWork_EmptyPlanIsBenign(t *testing.T) {
	var plan *Plan
	assert.False(t, plan.HasWork())
	assert.False(t, (&Plan{}).HasWork())
	assert.False(t, (&Plan{Senders: []SenderCapture{{
		SenderID: uuid.New(), OldAudience: map[uuid.UUID]bool{},
	}}}).HasWork(), "a sender with an empty captured audience is no work")
}

func TestPlan_HasWork_CandidateBearingSenderWithEmptyAudienceIsWork(t *testing.T) {
	assert.True(t, (&Plan{Senders: []SenderCapture{{
		SenderID:   uuid.New(),
		Candidates: map[uuid.UUID]bool{uuid.New(): true},
	}}}).HasWork(), "candidate-bearing sender must refresh even when captured audience is empty")
}

func TestPlan_HasWork_CapturedViewerIsWork(t *testing.T) {
	plan := &Plan{Senders: []SenderCapture{{
		SenderID:    uuid.New(),
		Candidates:  map[uuid.UUID]bool{uuid.New(): true},
		OldAudience: map[uuid.UUID]bool{uuid.New(): true},
	}}}
	assert.True(t, plan.HasWork())
}

func TestPlanBuilder_SameSenderAcrossChannels_UnionsAndOrdersByUUID(t *testing.T) {
	// Deterministic ordering: two senders whose UUIDs sort in a known order.
	low := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	high := uuid.MustParse("ffffffff-0000-4000-8000-000000000002")
	channelA := uuid.New()
	candidateA, candidateB := uuid.New(), uuid.New()

	builder := newPlanBuilder("server-id", nil)
	builder.add(high, channelA.String(), scopeFor(channelA), map[uuid.UUID]bool{candidateA: true})
	builder.add(low, channelA.String(), scopeFor(channelA), map[uuid.UUID]bool{candidateA: true})
	// Defensive union: buildServerVoice rejects an ambiguous multi-row sender,
	// so this cannot happen in practice — but the plan must not lose candidates.
	builder.add(low, channelA.String(), scopeFor(channelA), map[uuid.UUID]bool{candidateB: true})

	plan := builder.build()

	require.Len(t, plan.Senders, 2)
	assert.Equal(t, low, plan.Senders[0].SenderID, "senders are UUID-ordered for determinism")
	assert.Equal(t, high, plan.Senders[1].SenderID)
	assert.Equal(t,
		map[uuid.UUID]bool{candidateA: true, candidateB: true},
		plan.Senders[0].Candidates,
	)
	assert.Empty(t, plan.Senders[0].OldAudience,
		"phase 1 leaves OldAudience empty; phase 2 fills it")
}

func TestPlan_CapturedAudience_UnionsEverySender(t *testing.T) {
	viewerA, viewerB := uuid.New(), uuid.New()
	plan := &Plan{Senders: []SenderCapture{
		{SenderID: uuid.New(), OldAudience: map[uuid.UUID]bool{viewerA: true}},
		{SenderID: uuid.New(), OldAudience: map[uuid.UUID]bool{viewerB: true}},
	}}

	assert.Equal(t, map[uuid.UUID]bool{viewerA: true, viewerB: true}, plan.CapturedAudience())
}
