package opsmetrics

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type mutableActivityClock struct {
	now time.Time
}

func (clock *mutableActivityClock) Now() time.Time { return clock.now }

func TestActivityTrackerQualifiesAtSixtySeconds(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	require.Empty(t, tracker.PendingQualifications(clock.now.Add(59*time.Second)))

	pending := tracker.PendingQualifications(clock.now.Add(60 * time.Second))
	require.Equal(t, []ActivityQualification{{
		UserID:      userID,
		QualifiedAt: clock.now.Add(60 * time.Second),
	}}, pending)
}

func TestActivityTrackerAccumulatesDisjointIntervals(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	clock.now = clock.now.Add(30 * time.Second)
	tracker.UserDisconnected(userID)

	clock.now = clock.now.Add(10 * time.Minute)
	tracker.UserConnected(userID)
	require.Empty(t, tracker.PendingQualifications(clock.now.Add(29*time.Second)))

	pending := tracker.PendingQualifications(clock.now.Add(30 * time.Second))
	require.Len(t, pending, 1)
	require.Equal(t, clock.now.Add(30*time.Second), pending[0].QualifiedAt)
}

func TestActivityTrackerIgnoresDuplicateTransitions(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	clock.now = clock.now.Add(30 * time.Second)
	tracker.UserConnected(userID)
	clock.now = clock.now.Add(29 * time.Second)
	tracker.UserDisconnected(userID)
	tracker.UserDisconnected(userID)
	require.Empty(t, tracker.PendingQualifications(clock.now))

	clock.now = clock.now.Add(time.Minute)
	tracker.UserConnected(userID)
	require.Len(t, tracker.PendingQualifications(clock.now.Add(time.Second)), 1)
}

func TestActivityTrackerSplitsConnectedTimeAtUTCMidnight(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 19, 59, 30, 0, time.FixedZone("local", -4*60*60))}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	utcMidnightPlusThirty := time.Date(2026, 7, 16, 0, 0, 30, 0, time.UTC)
	require.Empty(t, tracker.PendingQualifications(utcMidnightPlusThirty))

	pending := tracker.PendingQualifications(time.Date(2026, 7, 16, 0, 1, 0, 0, time.UTC))
	require.Len(t, pending, 1)
	require.Equal(t, time.Date(2026, 7, 16, 0, 1, 0, 0, time.UTC), pending[0].QualifiedAt)
}

func TestActivityTrackerRetriesLatestQualifiedActivityUntilAcknowledged(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	first := tracker.PendingQualifications(clock.now.Add(time.Minute))
	require.Len(t, first, 1)
	secondAt := clock.now.Add(2 * time.Minute)
	second := tracker.PendingQualifications(secondAt)
	require.Equal(t, []ActivityQualification{{UserID: userID, QualifiedAt: secondAt}}, second)

	tracker.AcknowledgeQualifications(first)
	require.Equal(t, second, tracker.PendingQualifications(secondAt))

	tracker.AcknowledgeQualifications(second)
	clock.now = secondAt
	tracker.UserDisconnected(userID)
	require.Empty(t, tracker.PendingQualifications(secondAt))

	nextDay := time.Date(2026, 7, 16, 0, 1, 0, 0, time.UTC)
	clock.now = nextDay.Add(-time.Minute)
	tracker.UserConnected(userID)
	nextDayQualification := tracker.PendingQualifications(nextDay)
	require.Len(t, nextDayQualification, 1)
	require.Equal(t, nextDay, nextDayQualification[0].QualifiedAt)
}

func TestActivityTrackerPreservesLaterSameDayActivityForRollingWindows(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	first := tracker.PendingQualifications(clock.now.Add(time.Minute))
	require.Len(t, first, 1)
	tracker.AcknowledgeQualifications(first)

	later := clock.now.Add(23 * time.Hour)
	require.Equal(t, []ActivityQualification{{UserID: userID, QualifiedAt: later}},
		tracker.PendingQualifications(later))
}

func TestActivityTrackerStaleAcknowledgementDoesNotRemoveNewerDay(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 23, 58, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	first := tracker.PendingQualifications(clock.now.Add(time.Minute))
	require.Len(t, first, 1)

	secondAt := time.Date(2026, 7, 16, 0, 1, 0, 0, time.UTC)
	second := tracker.PendingQualifications(secondAt)
	require.Len(t, second, 1)
	require.Equal(t, secondAt, second[0].QualifiedAt)

	tracker.AcknowledgeQualifications(first)
	require.Equal(t, second, tracker.PendingQualifications(secondAt))
}

func TestActivityTrackerExpiresUnpersistedQualificationsAfterThirtyDays(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	qualifiedAt := clock.now.Add(time.Minute)
	require.Len(t, tracker.PendingQualifications(qualifiedAt), 1)

	disconnectedAt := qualifiedAt.Add(time.Second)
	clock.now = disconnectedAt
	tracker.UserDisconnected(userID)
	require.Empty(t, tracker.PendingQualifications(qualifiedAt.Add(activityQualificationRetention)))
}

func TestActivityTrackerOmitsDisconnectedQualificationWhenRetryMapIsFull(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	firstPendingUser := uuid.New()
	firstPendingAt := clock.now.Add(-time.Minute)
	tracker.pending[firstPendingUser] = pendingActivityQualification{
		qualifiedAt:   firstPendingAt,
		firstQueuedAt: firstPendingAt,
	}
	for range maxPendingQualifications - 1 {
		tracker.pending[uuid.New()] = pendingActivityQualification{
			qualifiedAt:   clock.now,
			firstQueuedAt: clock.now,
		}
	}

	userID := uuid.New()
	tracker.UserConnected(userID)
	clock.now = clock.now.Add(time.Minute)
	tracker.UserDisconnected(userID)
	require.Len(t, tracker.PendingQualifications(clock.now), maxPendingQualifications)

	tracker.AcknowledgeQualifications([]ActivityQualification{{
		UserID:      firstPendingUser,
		QualifiedAt: firstPendingAt,
	}})
	pending := tracker.PendingQualifications(clock.now.Add(time.Second))
	require.Len(t, pending, maxPendingQualifications-1)
	require.NotContains(t, pending, ActivityQualification{
		UserID:      userID,
		QualifiedAt: clock.now,
	})
}

func TestActivityTrackerExpiryIsNotExtendedByContinuingActivity(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()

	tracker.UserConnected(userID)
	firstQueuedAt := clock.now.Add(time.Minute)
	require.Len(t, tracker.PendingQualifications(firstQueuedAt), 1)

	later := firstQueuedAt.Add(29 * 24 * time.Hour)
	require.Equal(t, []ActivityQualification{{UserID: userID, QualifiedAt: later}},
		tracker.PendingQualifications(later))

	expiredAt := firstQueuedAt.Add(activityQualificationRetention + time.Microsecond)
	require.Empty(t, tracker.PendingQualifications(expiredAt))

	freshAt := expiredAt.Add(time.Second)
	require.Equal(t, []ActivityQualification{{UserID: userID, QualifiedAt: freshAt}},
		tracker.PendingQualifications(freshAt))
}

func TestActivityTrackerReclaimsExpiredCapacityBeforeAdvancingUsers(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	for range maxPendingQualifications {
		expiredAt := clock.now.Add(-activityQualificationRetention - time.Minute)
		tracker.pending[uuid.New()] = pendingActivityQualification{
			qualifiedAt:   expiredAt,
			firstQueuedAt: expiredAt,
		}
	}
	userID := uuid.New()
	tracker.UserConnected(userID)

	require.Equal(t, []ActivityQualification{{
		UserID:      userID,
		QualifiedAt: clock.now.Add(time.Minute),
	}}, tracker.PendingQualifications(clock.now.Add(time.Minute)))
}
