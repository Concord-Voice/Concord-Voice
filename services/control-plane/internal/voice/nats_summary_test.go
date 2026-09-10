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

// TestCompletedCallSummaryFromRoomEmpty_SkewedProducerKeepsTheHistoryRow pins the
// #3205 two-clock repair. endedAt reaches this function already CLAMPED to NATS
// receipt time while StartedAt is still the producer's raw clock, so on a
// forward-skewed producer the pair inverts and InsertCompletedCallEvent refuses
// the row outright -- silently, because persistDMRoomEmptySummary logs the
// refusal and returns. The whole point of the clamp is to keep working under a
// wrong producer clock, so losing call history to one is a regression the clamp
// itself introduced.
//
// Falsification: revert the shift+floor in completedCallSummaryFromRoomEmpty and
// the inversion assertion fails. Keep ONLY the floor (startedAt = endedAt, the
// sibling fallbacks' shape) and the inversion assertion passes but the duration
// assertion fails at 0s -- which is why both are here.
func TestCompletedCallSummaryFromRoomEmpty_SkewedProducerKeepsTheHistoryRow(t *testing.T) {
	callID := uuid.New()
	callerID := uuid.New()
	calleeID := uuid.New()

	// A media host ten minutes fast reports a three-minute call. Receipt time --
	// the consuming replica's clock, and therefore the clamped endedAt -- is the
	// real instant the call ended.
	receivedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	summary, supplied, err := completedCallSummaryFromRoomEmpty(voiceRoomEmptyEvent{
		CallID:             callID.String(),
		CallerUserID:       callerID.String(),
		ParticipantUserIDs: []string{callerID.String(), calleeID.String()},
		StartedAt:          "2026-07-14T12:07:00Z", // producer clock: real 11:57
		Timestamp:          "2026-07-14T12:10:00Z", // producer clock: real 12:00
	}, receivedAt)
	require.NoError(t, err)
	require.True(t, supplied)

	assert.False(t, summary.EndedAt.Before(summary.StartedAt),
		"EndedAt before StartedAt is refused by InsertCompletedCallEvent and the "+
			"refusal is only logged, so the row is lost with no error surfaced")
	assert.Equal(t, 3*time.Minute, summary.EndedAt.Sub(summary.StartedAt),
		"the producer's own two stamps are mutually consistent, so the measured "+
			"duration must survive the clamp rather than collapsing to zero")
	assert.Equal(t, receivedAt, summary.EndedAt, "EndedAt stays the clamped value")
}

// TestCompletedCallSummaryFromRoomEmpty_UnskewedProducerIsUntouched guards the
// other direction: the repair above must not fire when there was no clamp. A
// producer running behind the consumer is passed through verbatim, so shifting
// StartedAt there would invent a duration nobody measured.
func TestCompletedCallSummaryFromRoomEmpty_UnskewedProducerIsUntouched(t *testing.T) {
	callID := uuid.New()
	callerID := uuid.New()
	calleeID := uuid.New()

	for name, receivedAt := range map[string]time.Time{
		"producer exactly on time": time.Date(2026, 7, 14, 12, 1, 30, 0, time.UTC),
		"producer running behind":  time.Date(2026, 7, 14, 12, 9, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			summary, supplied, err := completedCallSummaryFromRoomEmpty(voiceRoomEmptyEvent{
				CallID:             callID.String(),
				CallerUserID:       callerID.String(),
				ParticipantUserIDs: []string{callerID.String(), calleeID.String()},
				StartedAt:          "2026-07-14T12:00:00Z",
				Timestamp:          "2026-07-14T12:01:30Z",
			}, receivedAt)
			require.NoError(t, err)
			require.True(t, supplied)
			assert.Equal(t, time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC), summary.StartedAt,
				"StartedAt must be verbatim when the clamp did not move the end")
		})
	}
}
