package voice

import (
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompletedCallSummaryFromRoomEmpty(t *testing.T) {
	callID := uuid.New()
	callerID := uuid.New()
	calleeID := uuid.New()
	startedAt := "2026-07-14T12:00:00Z"
	endedAt := "2026-07-14T12:01:30Z"

	summary, supplied, err := completedCallSummaryFromRoomEmpty(voiceRoomEmptyEvent{
		CallID:             callID.String(),
		CallerUserID:       callerID.String(),
		ParticipantUserIDs: []string{callerID.String(), calleeID.String(), callerID.String()},
		StartedAt:          startedAt,
		Timestamp:          endedAt,
	}, time.Date(2026, 7, 14, 12, 1, 30, 0, time.UTC))
	require.NoError(t, err)
	require.True(t, supplied)
	assert.Equal(t, callID, summary.CallID)
	assert.Equal(t, callerID, summary.CallerUserID)
	assert.ElementsMatch(t, []uuid.UUID{callerID, calleeID}, summary.ParticipantUserIDs)
	assert.True(t, sort.SliceIsSorted(summary.ParticipantUserIDs, func(left, right int) bool {
		return summary.ParticipantUserIDs[left].String() < summary.ParticipantUserIDs[right].String()
	}))
	assert.Equal(t, time.Minute+30*time.Second, summary.EndedAt.Sub(summary.StartedAt))
}

func TestCompletedCallSummaryFromRoomEmpty_LegacyAndMalformed(t *testing.T) {
	_, supplied, err := completedCallSummaryFromRoomEmpty(voiceRoomEmptyEvent{
		ChannelID: "legacy-conversation",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}, time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, supplied)

	_, supplied, err = completedCallSummaryFromRoomEmpty(voiceRoomEmptyEvent{
		CallID:       "not-a-uuid",
		CallerUserID: uuid.New().String(),
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	}, time.Now().UTC())
	require.Error(t, err)
	assert.True(t, supplied)
}

func TestPrivateVoiceRejectedParticipantIDs(t *testing.T) {
	accepted := uuid.New()
	rejectedA := uuid.New()
	rejectedB := uuid.New()

	got := privateVoiceRejectedParticipantIDs(
		[]uuid.UUID{rejectedB, accepted, rejectedA, rejectedB},
		[]uuid.UUID{accepted},
	)
	require.Len(t, got, 2)
	assert.ElementsMatch(t, []uuid.UUID{rejectedA, rejectedB}, got)
	assert.True(t, sort.SliceIsSorted(got, func(left, right int) bool {
		return got[left].String() < got[right].String()
	}))
}
