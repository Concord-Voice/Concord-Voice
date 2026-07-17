package opsmetrics

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	activityQualificationThreshold = time.Minute
	activityQualificationRetention = 30 * 24 * time.Hour
	maxPendingQualifications       = 10_000
)

// ActivityQualification is one application-private qualified-activity marker
// waiting to be reduced into aggregate activity gauges.
type ActivityQualification struct {
	UserID      uuid.UUID
	QualifiedAt time.Time
}

type userActivityState struct {
	day            time.Time
	connectedSince time.Time
	accumulated    time.Duration
	connected      bool
}

type pendingActivityQualification struct {
	qualifiedAt   time.Time
	firstQueuedAt time.Time
}

// ActivityTracker accumulates distinct-user connected time without retaining
// an interval ledger. Identity-bearing state never leaves the trusted collector.
type ActivityTracker struct {
	mu      sync.Mutex
	now     func() time.Time
	states  map[uuid.UUID]*userActivityState
	pending map[uuid.UUID]pendingActivityQualification
}

// NewActivityTracker creates a tracker driven by the system clock.
func NewActivityTracker() *ActivityTracker {
	return newActivityTracker(time.Now)
}

func newActivityTracker(now func() time.Time) *ActivityTracker {
	return &ActivityTracker{
		now:     now,
		states:  make(map[uuid.UUID]*userActivityState),
		pending: make(map[uuid.UUID]pendingActivityQualification),
	}
}

// UserConnected starts a user-level interval. Duplicate transitions are inert.
func (tracker *ActivityTracker) UserConnected(userID uuid.UUID) {
	if tracker == nil || userID == uuid.Nil {
		return
	}
	now := tracker.now().UTC()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	state, ok := tracker.states[userID]
	if !ok {
		state = &userActivityState{day: utcDay(now)}
		tracker.states[userID] = state
	}
	if state.connected {
		return
	}
	if state.day.Before(utcDay(now)) {
		state.day = utcDay(now)
		state.accumulated = 0
	}
	state.connected = true
	state.connectedSince = now
}

// UserDisconnected closes a user-level interval. Duplicate transitions are inert.
func (tracker *ActivityTracker) UserDisconnected(userID uuid.UUID) {
	if tracker == nil || userID == uuid.Nil {
		return
	}
	now := tracker.now().UTC()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	state, ok := tracker.states[userID]
	if !ok || !state.connected {
		return
	}
	tracker.advanceLocked(userID, state, now, false)
	state.connected = false
	state.connectedSince = time.Time{}
}

// PendingQualifications advances open intervals and returns a stable copy of
// unacknowledged activity markers. Calling it again is retry-safe.
func (tracker *ActivityTracker) PendingQualifications(at time.Time) []ActivityQualification {
	if tracker == nil {
		return nil
	}
	at = at.UTC()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	retentionCutoff := at.Add(-activityQualificationRetention)
	expired := make(map[uuid.UUID]struct{})
	for userID, pending := range tracker.pending {
		if !pending.firstQueuedAt.After(retentionCutoff) {
			delete(tracker.pending, userID)
			expired[userID] = struct{}{}
		}
	}

	for userID, state := range tracker.states {
		if state.connected {
			_, suppressRequeue := expired[userID]
			tracker.advanceLocked(userID, state, at, suppressRequeue)
			continue
		}
		if !at.Before(state.day.Add(24 * time.Hour)) {
			delete(tracker.states, userID)
		}
	}

	result := make([]ActivityQualification, 0, len(tracker.pending))
	for userID, pending := range tracker.pending {
		result = append(result, ActivityQualification{UserID: userID, QualifiedAt: pending.qualifiedAt})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UserID == result[j].UserID {
			return result[i].QualifiedAt.Before(result[j].QualifiedAt)
		}
		return result[i].UserID.String() < result[j].UserID.String()
	})
	return result
}

// AcknowledgeQualifications removes only the exact pending values that were
// persisted. A stale acknowledgement cannot erase newer observed activity.
func (tracker *ActivityTracker) AcknowledgeQualifications(qualifications []ActivityQualification) {
	if tracker == nil || len(qualifications) == 0 {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	for _, qualification := range qualifications {
		pending, ok := tracker.pending[qualification.UserID]
		if ok && pending.qualifiedAt.Equal(qualification.QualifiedAt) {
			delete(tracker.pending, qualification.UserID)
		}
	}
}

func (tracker *ActivityTracker) advanceLocked(
	userID uuid.UUID,
	state *userActivityState,
	at time.Time,
	suppressRequeue bool,
) {
	if !state.connected || !at.After(state.connectedSince) {
		return
	}

	cursor := state.connectedSince
	for cursor.Before(at) {
		day := utcDay(cursor)
		if !state.day.Equal(day) {
			state.day = day
			state.accumulated = 0
		}

		dayEnd := day.Add(24 * time.Hour)
		segmentEnd := at
		if dayEnd.Before(segmentEnd) {
			segmentEnd = dayEnd
		}
		segmentDuration := segmentEnd.Sub(cursor)
		tracker.recordQualifiedActivityLocked(
			userID, state, segmentEnd, dayEnd, segmentDuration, suppressRequeue,
		)
		state.accumulated = min(state.accumulated+segmentDuration, activityQualificationThreshold)
		cursor = segmentEnd
	}
	state.connectedSince = at
}

func (tracker *ActivityTracker) recordQualifiedActivityLocked(
	userID uuid.UUID,
	state *userActivityState,
	segmentEnd time.Time,
	dayEnd time.Time,
	segmentDuration time.Duration,
	suppressRequeue bool,
) {
	if suppressRequeue || state.accumulated+segmentDuration < activityQualificationThreshold {
		return
	}
	qualifiedAt := segmentEnd
	if qualifiedAt.Equal(dayEnd) {
		qualifiedAt = dayEnd.Add(-time.Microsecond)
	}
	pending, ok := tracker.pending[userID]
	if !ok && len(tracker.pending) >= maxPendingQualifications {
		return
	}
	if !ok {
		tracker.pending[userID] = pendingActivityQualification{
			qualifiedAt:   qualifiedAt,
			firstQueuedAt: segmentEnd,
		}
		return
	}
	if qualifiedAt.After(pending.qualifiedAt) {
		pending.qualifiedAt = qualifiedAt
		tracker.pending[userID] = pending
	}
}

func utcDay(at time.Time) time.Time {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}
