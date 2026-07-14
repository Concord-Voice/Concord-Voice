package voice

import (
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
	})
	require.NoError(t, err)
	require.True(t, supplied)
	assert.Equal(t, callID, summary.CallID)
	assert.Equal(t, callerID, summary.CallerUserID)
	assert.Equal(t, []uuid.UUID{callerID, calleeID}, summary.ParticipantUserIDs)
	assert.Equal(t, time.Minute+30*time.Second, summary.EndedAt.Sub(summary.StartedAt))
}

func TestCompletedCallSummaryFromRoomEmpty_LegacyAndMalformed(t *testing.T) {
	_, supplied, err := completedCallSummaryFromRoomEmpty(voiceRoomEmptyEvent{
		ChannelID: "legacy-conversation",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	assert.False(t, supplied)

	_, supplied, err = completedCallSummaryFromRoomEmpty(voiceRoomEmptyEvent{
		CallID:       "not-a-uuid",
		CallerUserID: uuid.New().String(),
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	require.Error(t, err)
	assert.True(t, supplied)
}
