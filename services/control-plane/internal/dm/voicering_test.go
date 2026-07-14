// Package dm internal tests for the DM voice call ring state machinery (#1209).
// Internal package (package dm, not dm_test) because these tests exercise the
// unexported newPendingCall constructor; the external HTTP handler tests
// continue to live in handlers_test.go as package dm_test.
package dm

import (
	"errors"
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

func TestPendingCall_TransitionsSerializeDeclineAndAccept(t *testing.T) {
	convID := uuid.New()
	caller := uuid.New()
	decliner := uuid.New()
	acceptor := uuid.New()
	ring := newPendingCall(convID, caller, []uuid.UUID{decliner, acceptor}, time.Second)
	pendingDMCalls.Store(convID, ring)
	t.Cleanup(func() {
		ring.finalizeTerminal()
		pendingDMCalls.Delete(convID)
	})

	declineEnqueued := make(chan struct{})
	releaseDecline := make(chan struct{})
	declineResult := make(chan declineTransition, 1)
	go func() {
		declineResult <- ring.tryDecline(decliner, func() {
			close(declineEnqueued)
			<-releaseDecline
		})
	}()
	<-declineEnqueued

	type acceptTransitionResult struct {
		accepted bool
		err      error
	}
	acceptResult := make(chan acceptTransitionResult, 1)
	go func() {
		accepted, err := ring.tryAccept(acceptor, nil)
		acceptResult <- acceptTransitionResult{accepted: accepted, err: err}
	}()
	select {
	case <-acceptResult:
		t.Fatal("accept overtook the in-flight decline notification")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseDecline)
	assert.Equal(t, declineTransitionPending, <-declineResult)
	result := <-acceptResult
	require.NoError(t, result.err)
	require.True(t, result.accepted)
	assert.True(t, ring.terminalOwned)
	assert.True(t, ring.isCurrentLockedForTest())

	ring.finalizeTerminal()
	_, loaded := pendingDMCalls.Load(convID)
	assert.False(t, loaded)
}

func TestPendingCall_AcceptCallbackFailureRollsBackTransition(t *testing.T) {
	convID := uuid.New()
	caller := uuid.New()
	acceptor := uuid.New()
	ring := newPendingCall(convID, caller, []uuid.UUID{acceptor}, time.Second)
	pendingDMCalls.Store(convID, ring)
	t.Cleanup(func() { pendingDMCalls.Delete(convID) })

	wantErr := errors.New("lease unavailable")
	accepted, err := ring.tryAccept(acceptor, func() error { return wantErr })
	require.ErrorIs(t, err, wantErr)
	assert.False(t, accepted)
	assert.False(t, ring.terminalOwned)
	assert.Contains(t, ring.RingingUserIDs, acceptor)
	assert.NotContains(t, ring.AcceptedUserIDs, acceptor)
	assert.True(t, ring.isCurrentLockedForTest())

	accepted, err = ring.tryAccept(acceptor, nil)
	require.NoError(t, err)
	assert.True(t, accepted)
}

func TestPendingCall_InitializationPrecedesTerminalTransition(t *testing.T) {
	convID := uuid.New()
	ring := newPendingCall(convID, uuid.New(), []uuid.UUID{uuid.New()}, time.Second)
	t.Cleanup(func() {
		ring.finalizeTerminal()
		pendingDMCalls.Delete(convID)
	})

	initializing := make(chan struct{})
	releaseInitialization := make(chan struct{})
	claimResult := make(chan bool, 1)
	go func() {
		_, loaded := loadOrStoreInitializedPendingCall(ring, func() {
			close(initializing)
			<-releaseInitialization
		})
		claimResult <- loaded
	}()
	<-initializing

	terminalResult := make(chan bool, 1)
	go func() { terminalResult <- ring.tryTerminate() }()
	select {
	case <-terminalResult:
		t.Fatal("terminal transition overtook ring initialization")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseInitialization)
	assert.False(t, <-claimResult)
	require.True(t, <-terminalResult)
	ring.finalizeTerminal()
	_, loaded := pendingDMCalls.Load(convID)
	assert.False(t, loaded)
}

func (p *PendingCall) isCurrentLockedForTest() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isCurrentLocked()
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

func TestAcceptedDMCallCorrelationExpiresAndReplacementSurvivesOldTimer(t *testing.T) {
	convID := uuid.New()
	first := newPendingCall(convID, uuid.New(), []uuid.UUID{uuid.New()}, time.Second)
	rememberAcceptedDMCall(first, 20*time.Millisecond)

	record, ok := lookupAcceptedDMCall(convID, first.RingID)
	require.True(t, ok)
	assert.Equal(t, first.CallerUserID, record.CallerUserID)

	replacement := newPendingCall(convID, uuid.New(), []uuid.UUID{uuid.New()}, time.Second)
	time.Sleep(5 * time.Millisecond)
	rememberAcceptedDMCall(replacement, time.Second)
	t.Cleanup(func() { forgetAcceptedDMCall(convID, replacement.RingID) })

	time.Sleep(30 * time.Millisecond)
	_, oldFound := lookupAcceptedDMCall(convID, first.RingID)
	assert.False(t, oldFound, "replaced ring cannot be resolved")
	current, currentFound := lookupAcceptedDMCall(convID, replacement.RingID)
	require.True(t, currentFound, "old timer must not delete the replacement")
	assert.Equal(t, replacement.CallerUserID, current.CallerUserID)

	forgetAcceptedDMCall(convID, replacement.RingID)
	_, foundAfterForget := lookupAcceptedDMCall(convID, replacement.RingID)
	assert.False(t, foundAfterForget)
}

func TestAcceptedDMCallCorrelationExpiresWithoutMediaJoin(t *testing.T) {
	ring := newPendingCall(uuid.New(), uuid.New(), []uuid.UUID{uuid.New()}, time.Second)
	rememberAcceptedDMCall(ring, 10*time.Millisecond)
	t.Cleanup(func() { forgetAcceptedDMCall(ring.ConversationID, ring.RingID) })

	require.Eventually(t, func() bool {
		_, ok := lookupAcceptedDMCall(ring.ConversationID, ring.RingID)
		return !ok
	}, 200*time.Millisecond, 5*time.Millisecond)
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

func TestSanitizeLogValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain", input: "conversation-123", want: "conversation-123"},
		{name: "empty", input: "", want: ""},
		{name: "crlf", input: "id\r\nforged-entry", want: "idforged-entry"},
		{name: "c0_and_del", input: "a\t\x00\x01\x1f\x7fb", want: "ab"},
		{name: "unicode", input: "café-🎉-Ω", want: "café-🎉-Ω"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, sanitizeLogValue(test.input))
		})
	}
}
