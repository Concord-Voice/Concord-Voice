// Internal tests for the call_event payload constructors (#1209).
//
// Package `dm` rather than `dm_test` because these helpers are unexported.
// The corresponding HTTP handler tests live in handlers_test.go as
// package dm_test.

package dm

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallEventMissed(t *testing.T) {
	convID := uuid.New()
	caller := uuid.New()
	startedAt := time.Now().Add(-30 * time.Second)
	ring := &PendingCall{
		RingID:         uuid.New(),
		CallerUserID:   caller,
		ConversationID: convID,
		RingStartedAt:  startedAt,
	}

	payload := callEventMissed(ring)

	assert.Equal(t, ring.RingID, payload.RingID)
	assert.Equal(t, caller, payload.CallerUserID)
	assert.Equal(t, CallEventMissed, payload.Status)
	assert.Equal(t, 0, payload.DurationSeconds)
	assert.Equal(t, startedAt, payload.StartedAt)
	// EndedAt should be roughly now.
	assert.WithinDuration(t, time.Now(), payload.EndedAt, 5*time.Second)
	// Participants = [caller] only (no callees responded).
	require.Len(t, payload.ParticipantUserIDs, 1)
	assert.Equal(t, caller, payload.ParticipantUserIDs[0])
}

func TestCallEventCanceled(t *testing.T) {
	convID := uuid.New()
	caller := uuid.New()
	startedAt := time.Now().Add(-10 * time.Second)
	ring := &PendingCall{
		RingID:         uuid.New(),
		CallerUserID:   caller,
		ConversationID: convID,
		RingStartedAt:  startedAt,
	}

	payload := callEventCanceled(ring)

	assert.Equal(t, ring.RingID, payload.RingID)
	assert.Equal(t, caller, payload.CallerUserID)
	assert.Equal(t, CallEventCanceled, payload.Status)
	assert.Equal(t, 0, payload.DurationSeconds)
	assert.Equal(t, startedAt, payload.StartedAt)
	assert.WithinDuration(t, time.Now(), payload.EndedAt, 5*time.Second)
	// Participants = [caller] only — call canceled before any callee responded.
	require.Len(t, payload.ParticipantUserIDs, 1)
	assert.Equal(t, caller, payload.ParticipantUserIDs[0])
}

func TestCallEventDeclined(t *testing.T) {
	t.Run("includes caller + declined callees", func(t *testing.T) {
		convID := uuid.New()
		caller := uuid.New()
		decliner1 := uuid.New()
		decliner2 := uuid.New()
		startedAt := time.Now().Add(-20 * time.Second)
		ring := &PendingCall{
			RingID:          uuid.New(),
			CallerUserID:    caller,
			ConversationID:  convID,
			RingStartedAt:   startedAt,
			DeclinedUserIDs: map[uuid.UUID]struct{}{decliner1: {}, decliner2: {}},
			mu:              sync.Mutex{},
		}

		payload := callEventDeclined(ring)

		assert.Equal(t, ring.RingID, payload.RingID)
		assert.Equal(t, caller, payload.CallerUserID)
		assert.Equal(t, CallEventDeclined, payload.Status)
		assert.Equal(t, 0, payload.DurationSeconds)
		assert.Equal(t, startedAt, payload.StartedAt)
		assert.WithinDuration(t, time.Now(), payload.EndedAt, 5*time.Second)
		// Participants = caller + 2 decliners. Iteration order over the map
		// is non-deterministic; use ElementsMatch.
		assert.ElementsMatch(t, []uuid.UUID{caller, decliner1, decliner2}, payload.ParticipantUserIDs)
	})

	t.Run("with no decliners (defensive — should not happen in production)", func(t *testing.T) {
		caller := uuid.New()
		ring := &PendingCall{
			RingID:          uuid.New(),
			CallerUserID:    caller,
			RingStartedAt:   time.Now(),
			DeclinedUserIDs: map[uuid.UUID]struct{}{},
			mu:              sync.Mutex{},
		}

		payload := callEventDeclined(ring)

		require.Len(t, payload.ParticipantUserIDs, 1)
		assert.Equal(t, caller, payload.ParticipantUserIDs[0])
	})

	t.Run("holds the ring lock during read (no data race with concurrent MarkDeclined)", func(_ *testing.T) {
		// Smoke-test concurrent safety: run callEventDeclined while other
		// goroutines call ring.MarkDeclined. With -race the test runner will
		// flag any unsynchronized access — this is a regression guard
		// against accidentally removing the ring.mu.Lock() in
		// callEventDeclined. We collect the callee IDs into a slice up
		// front so the test goroutine doesn't itself race-iterate the map.
		caller := uuid.New()
		callees := []uuid.UUID{uuid.New(), uuid.New()}
		ring := newPendingCall(uuid.New(), caller, callees, time.Second)

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = callEventDeclined(ring)
			}()
		}
		for _, u := range callees {
			wg.Add(1)
			go func(uid uuid.UUID) {
				defer wg.Done()
				ring.MarkDeclined(uid)
			}(u)
		}
		wg.Wait()
	})
}

// TestCallEventDeclined_GroupListsAllDecliners locks the N-generic decliner
// list for groups with MORE than two decliners (#1219 B4). The existing
// TestCallEventDeclined covers exactly two decliners; this proves the builder
// scales to an arbitrary group size (caller + 3 decliners = 4 participants).
func TestCallEventDeclined_GroupListsAllDecliners(t *testing.T) {
	caller := uuid.New()
	d1, d2, d3 := uuid.New(), uuid.New(), uuid.New()
	ring := &PendingCall{
		RingID:          uuid.New(),
		CallerUserID:    caller,
		RingStartedAt:   time.Now(),
		DeclinedUserIDs: map[uuid.UUID]struct{}{d1: {}, d2: {}, d3: {}},
		mu:              sync.Mutex{},
	}

	p := callEventDeclined(ring)

	assert.Equal(t, CallEventDeclined, p.Status)
	require.Len(t, p.ParticipantUserIDs, 4, "caller + 3 decliners")
	assert.ElementsMatch(t, []uuid.UUID{caller, d1, d2, d3}, p.ParticipantUserIDs)
}
