// Package dm internal tests for the DM voice call ring state machinery (#1209).
// Internal package (package dm, not dm_test) because these tests exercise the
// unexported newPendingCall constructor; the external HTTP handler tests
// continue to live in handlers_test.go as package dm_test.
package dm

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingCall_Lifecycle(t *testing.T) {
	t.Run("creates pending call with ringing user set", func(t *testing.T) {
		convID := uuid.New()
		caller := uuid.New()
		callees := []uuid.UUID{uuid.New(), uuid.New()}
		ring := newPendingCall(convID, caller, callees, 1*time.Second)
		require.NotNil(t, ring)
		assert.Equal(t, caller, ring.CallerUserID)
		assert.Equal(t, convID, ring.ConversationID)
		assert.Len(t, ring.RingingUserIDs, 2)
		assert.Empty(t, ring.DeclinedUserIDs)
		assert.Empty(t, ring.AcceptedUserIDs)
		assert.NotEqual(t, uuid.Nil, ring.RingID)
		assert.WithinDuration(t, time.Now(), ring.RingStartedAt, time.Second)
	})

	t.Run("MarkDeclined moves from ringing to declined", func(t *testing.T) {
		convID := uuid.New()
		caller := uuid.New()
		callee := uuid.New()
		ring := newPendingCall(convID, caller, []uuid.UUID{callee}, 1*time.Second)
		ring.MarkDeclined(callee)
		assert.Empty(t, ring.RingingUserIDs)
		assert.Contains(t, ring.DeclinedUserIDs, callee)
		assert.Empty(t, ring.AcceptedUserIDs)
	})

	t.Run("MarkAccepted moves from ringing to accepted", func(t *testing.T) {
		convID := uuid.New()
		caller := uuid.New()
		callee := uuid.New()
		ring := newPendingCall(convID, caller, []uuid.UUID{callee}, 1*time.Second)
		ring.MarkAccepted(callee)
		assert.Empty(t, ring.RingingUserIDs)
		assert.Empty(t, ring.DeclinedUserIDs)
		assert.Contains(t, ring.AcceptedUserIDs, callee)
	})

	t.Run("IsFullyDeclined true when ringing empty and accepted empty", func(t *testing.T) {
		convID := uuid.New()
		caller := uuid.New()
		callee := uuid.New()
		ring := newPendingCall(convID, caller, []uuid.UUID{callee}, 1*time.Second)
		assert.False(t, ring.IsFullyDeclined(), "starts false with someone still ringing")
		ring.MarkDeclined(callee)
		assert.True(t, ring.IsFullyDeclined(), "true after the only callee declines")
	})

	t.Run("IsFullyDeclined false when someone has accepted (even if others declined)", func(t *testing.T) {
		convID := uuid.New()
		caller := uuid.New()
		acceptor := uuid.New()
		decliner := uuid.New()
		ring := newPendingCall(convID, caller, []uuid.UUID{acceptor, decliner}, 1*time.Second)
		ring.MarkAccepted(acceptor)
		ring.MarkDeclined(decliner)
		assert.False(t, ring.IsFullyDeclined(), "accepted-by-someone trumps decline-by-others")
	})

	t.Run("multiple callees: per-decliner state tracked correctly", func(t *testing.T) {
		convID := uuid.New()
		caller := uuid.New()
		c1, c2, c3 := uuid.New(), uuid.New(), uuid.New()
		ring := newPendingCall(convID, caller, []uuid.UUID{c1, c2, c3}, 1*time.Second)

		ring.MarkDeclined(c1)
		assert.Len(t, ring.RingingUserIDs, 2)
		assert.Len(t, ring.DeclinedUserIDs, 1)
		assert.False(t, ring.IsFullyDeclined())

		ring.MarkDeclined(c2)
		assert.Len(t, ring.RingingUserIDs, 1)
		assert.Len(t, ring.DeclinedUserIDs, 2)
		assert.False(t, ring.IsFullyDeclined())

		ring.MarkDeclined(c3)
		assert.Empty(t, ring.RingingUserIDs)
		assert.Len(t, ring.DeclinedUserIDs, 3)
		assert.True(t, ring.IsFullyDeclined(), "all callees declined, none accepted")
	})
}

func TestPendingCall_TimerLifecycle(t *testing.T) {
	t.Run("StartTimer fires the callback after the duration", func(t *testing.T) {
		ring := newPendingCall(uuid.New(), uuid.New(), []uuid.UUID{uuid.New()}, 100*time.Millisecond)
		fired := make(chan struct{}, 1)
		ring.StartTimer(50*time.Millisecond, func() { fired <- struct{}{} })
		select {
		case <-fired:
			// expected
		case <-time.After(200 * time.Millisecond):
			t.Fatal("timer did not fire within 200ms (configured: 50ms)")
		}
	})

	t.Run("StopTimer prevents the callback from firing", func(t *testing.T) {
		ring := newPendingCall(uuid.New(), uuid.New(), []uuid.UUID{uuid.New()}, 100*time.Millisecond)
		fired := make(chan struct{}, 1)
		ring.StartTimer(50*time.Millisecond, func() { fired <- struct{}{} })
		ring.StopTimer()
		select {
		case <-fired:
			t.Fatal("timer fired despite StopTimer call")
		case <-time.After(150 * time.Millisecond):
			// expected — no fire
		}
	})

	t.Run("StopTimer is safe when no timer was started", func(t *testing.T) {
		ring := newPendingCall(uuid.New(), uuid.New(), []uuid.UUID{uuid.New()}, 100*time.Millisecond)
		// Should not panic
		assert.NotPanics(t, func() { ring.StopTimer() })
	})

	t.Run("StartTimer twice replaces the first timer", func(t *testing.T) {
		ring := newPendingCall(uuid.New(), uuid.New(), []uuid.UUID{uuid.New()}, 100*time.Millisecond)
		firstFired := make(chan struct{}, 1)
		secondFired := make(chan struct{}, 1)
		ring.StartTimer(50*time.Millisecond, func() { firstFired <- struct{}{} })
		ring.StartTimer(50*time.Millisecond, func() { secondFired <- struct{}{} })
		select {
		case <-firstFired:
			t.Fatal("first timer should have been replaced by second StartTimer")
		case <-secondFired:
			// expected
		case <-time.After(200 * time.Millisecond):
			t.Fatal("neither timer fired within 200ms")
		}
	})
}

func TestDMVoiceInvitedData_IsGroup(t *testing.T) {
	caller := map[string]interface{}{"user_id": uuid.New().String(), "username": "alice"}
	ring := &PendingCall{RingID: uuid.New(), RingStartedAt: time.Now()}
	convID := uuid.New()

	groupData := dmVoiceInvitedData(convID, true, caller, ring, 45)
	assert.Equal(t, true, groupData["is_group"])
	assert.Equal(t, convID.String(), groupData["conversation_id"])
	assert.Equal(t, caller, groupData["caller"])
	assert.Equal(t, ring.RingID.String(), groupData["ring_id"])
	assert.Equal(t, 45, groupData["ring_timeout_seconds"])

	oneToOne := dmVoiceInvitedData(convID, false, caller, ring, 45)
	assert.Equal(t, false, oneToOne["is_group"])
}
