package opsmetrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeAccountRepository struct {
	current       accountCurrentCounts
	active        accountActiveCounts
	currentErr    error
	activeErr     error
	qualification error
	recorded      []ActivityQualification
	// overdue and overdueErr back BillingErasureOverdue (#2033 PR-2): the count of
	// billing_erasure_outbox rows older than 24 hours, read independently of the
	// other counts so one failing query omits only its own sample.
	overdue    int64
	overdueErr error
	overdueAt  []time.Time
}

func (repository *fakeAccountRepository) CurrentCounts(context.Context, time.Time) (accountCurrentCounts, error) {
	return repository.current, repository.currentErr
}

func (repository *fakeAccountRepository) ActiveCounts(context.Context, time.Time) (accountActiveCounts, error) {
	return repository.active, repository.activeErr
}

func (repository *fakeAccountRepository) BillingErasureOverdue(_ context.Context, at time.Time) (int64, error) {
	repository.overdueAt = append(repository.overdueAt, at)
	return repository.overdue, repository.overdueErr
}

func (repository *fakeAccountRepository) RecordQualifications(_ context.Context, qualifications []ActivityQualification) error {
	repository.recorded = append([]ActivityQualification(nil), qualifications...)
	return repository.qualification
}

func TestAccountProviderReturnsOnlyFixedAggregateMetrics(t *testing.T) {
	repository := &fakeAccountRepository{
		current: accountCurrentCounts{registered: 120, pending: 4, activeSessions: 33},
		active:  accountActiveCounts{hours24: 18, days7: 70, days15: 91, days30: 108},
		overdue: 4,
	}
	provider := newAccountProvider(repository, nil)
	at := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	metrics, err := provider.AccountMetrics(context.Background(), at)
	require.NoError(t, err)
	require.Equal(t, map[MetricKey]float64{
		MetricRegisteredUsersCurrent:      120,
		MetricPendingRegistrationsCurrent: 4,
		MetricActiveSessionsCurrent:       33,
		MetricActiveUsers24H:              18,
		MetricActiveUsers7D:               70,
		MetricActiveUsers15D:              91,
		MetricActiveUsers30D:              108,
		MetricBillingErasureOutboxPending: 4,
	}, metrics)
	require.Equal(t, []time.Time{at}, repository.overdueAt, "the overdue count is evaluated at the collection instant")
	for key := range metrics {
		definition, ok := Definition(key)
		require.True(t, ok)
		require.Equal(t, SourceControl, definition.Source)
	}
}

func TestAccountProviderRetriesQualificationBeforeActiveCounts(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()
	tracker.UserConnected(userID)
	at := clock.now.Add(time.Minute)
	persistErr := errors.New("activity marker unavailable")
	repository := &fakeAccountRepository{
		current:       accountCurrentCounts{registered: 1},
		active:        accountActiveCounts{hours24: 1, days7: 1, days15: 1, days30: 1},
		qualification: persistErr,
	}
	provider := newAccountProvider(repository, tracker)

	metrics, err := provider.AccountMetrics(context.Background(), at)
	require.ErrorIs(t, err, persistErr)
	require.Equal(t, float64(1), metrics[MetricRegisteredUsersCurrent])
	require.NotContains(t, metrics, MetricActiveUsers24H)
	require.Equal(t, []ActivityQualification{{UserID: userID, QualifiedAt: at}}, repository.recorded)

	repository.qualification = nil
	succeededAt := at.Add(time.Second)
	metrics, err = provider.AccountMetrics(context.Background(), succeededAt)
	require.NoError(t, err)
	require.Equal(t, float64(1), metrics[MetricActiveUsers24H])
	clock.now = succeededAt
	tracker.UserDisconnected(userID)
	require.Empty(t, tracker.PendingQualifications(at.Add(2*time.Second)))
}

func TestAccountProviderFlushesQualificationsWithoutCollectingCounts(t *testing.T) {
	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	userID := uuid.New()
	tracker.UserConnected(userID)
	at := clock.now.Add(time.Minute)
	repository := &fakeAccountRepository{
		currentErr: errors.New("current counts unavailable"),
		activeErr:  errors.New("active counts unavailable"),
	}

	require.NoError(t, newAccountProvider(repository, tracker).FlushQualifications(context.Background(), at))
	require.Equal(t, []ActivityQualification{{UserID: userID, QualifiedAt: at}}, repository.recorded)
	require.Empty(t, tracker.PendingQualifications(at))
}

func TestAccountProviderBoundsQualificationsAcrossPersistenceFailures(t *testing.T) {
	const pendingLimit = 10_000

	clock := &mutableActivityClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	tracker := newActivityTracker(clock.Now)
	for range pendingLimit + 1 {
		tracker.UserConnected(uuid.New())
	}
	persistErr := errors.New("activity marker unavailable")
	repository := &fakeAccountRepository{qualification: persistErr}
	provider := newAccountProvider(repository, tracker)
	at := clock.now.Add(time.Minute)

	_, err := provider.AccountMetrics(context.Background(), at)
	require.ErrorIs(t, err, persistErr)
	require.Len(t, repository.recorded, pendingLimit)
	require.Len(t, tracker.PendingQualifications(at.Add(time.Second)), pendingLimit)

	_, err = provider.AccountMetrics(context.Background(), at.Add(2*time.Second))
	require.ErrorIs(t, err, persistErr)
	require.Len(t, repository.recorded, pendingLimit)
	require.Len(t, tracker.PendingQualifications(at.Add(3*time.Second)), pendingLimit)
}

func TestAccountProviderDegradesQueriesIndependently(t *testing.T) {
	currentErr := errors.New("current counts failed")
	activeErr := errors.New("active counts failed")
	at := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	t.Run("current counts", func(t *testing.T) {
		repository := &fakeAccountRepository{
			currentErr: currentErr,
			active:     accountActiveCounts{hours24: 2, days7: 3, days15: 4, days30: 5},
		}
		metrics, err := newAccountProvider(repository, nil).AccountMetrics(context.Background(), at)
		require.ErrorIs(t, err, currentErr)
		require.NotContains(t, metrics, MetricRegisteredUsersCurrent)
		require.Equal(t, float64(2), metrics[MetricActiveUsers24H])
	})

	t.Run("active counts", func(t *testing.T) {
		repository := &fakeAccountRepository{
			current:   accountCurrentCounts{registered: 6, pending: 1, activeSessions: 2},
			activeErr: activeErr,
		}
		metrics, err := newAccountProvider(repository, nil).AccountMetrics(context.Background(), at)
		require.ErrorIs(t, err, activeErr)
		require.Equal(t, float64(6), metrics[MetricRegisteredUsersCurrent])
		require.NotContains(t, metrics, MetricActiveUsers24H)
	})
}

func TestAccountProviderBillingErasureOverdueDegradesIndependently(t *testing.T) {
	overdueErr := errors.New("billing outbox count failed")
	at := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	t.Run("only the billing gauge is omitted", func(t *testing.T) {
		repository := &fakeAccountRepository{
			current:    accountCurrentCounts{registered: 6, pending: 1, activeSessions: 2},
			active:     accountActiveCounts{hours24: 2, days7: 3, days15: 4, days30: 5},
			overdueErr: overdueErr,
		}
		metrics, err := newAccountProvider(repository, nil).AccountMetrics(context.Background(), at)
		require.ErrorIs(t, err, overdueErr)
		require.NotContains(t, metrics, MetricBillingErasureOutboxPending)
		require.Equal(t, float64(6), metrics[MetricRegisteredUsersCurrent])
		require.Equal(t, float64(2), metrics[MetricActiveUsers24H])
	})

	t.Run("a failing account count does not hide the billing gauge", func(t *testing.T) {
		repository := &fakeAccountRepository{
			currentErr: errors.New("current counts failed"),
			activeErr:  errors.New("active counts failed"),
			overdue:    7,
		}
		metrics, err := newAccountProvider(repository, nil).AccountMetrics(context.Background(), at)
		require.Error(t, err)
		require.Equal(t, float64(7), metrics[MetricBillingErasureOutboxPending])
	})

	t.Run("zero overdue is reported as zero, not omitted", func(t *testing.T) {
		metrics, err := newAccountProvider(&fakeAccountRepository{}, nil).AccountMetrics(context.Background(), at)
		require.NoError(t, err)
		require.Contains(t, metrics, MetricBillingErasureOutboxPending)
		require.Zero(t, metrics[MetricBillingErasureOutboxPending])
	})
}

func TestAccountMetricSQLUsesClosedPredicates(t *testing.T) {
	require.Contains(t, accountCurrentCountsSQL, "COUNT(*) FROM users")
	require.Contains(t, accountCurrentCountsSQL, "expires_at > $1")
	require.Contains(t, accountCurrentCountsSQL, "revoked_at IS NULL")
	require.Contains(t, accountActiveCountsSQL, "ops_last_active_at >= $1")
	require.Contains(t, accountActiveCountsSQL, "ops_last_active_at >= $4")
	require.Contains(t, accountActiveCountsSQL, "FROM users\nWHERE ops_last_active_at >= $4")
	require.NotContains(t, accountCurrentCountsSQL, "SELECT id")
	require.NotContains(t, accountActiveCountsSQL, "GROUP BY")
	require.Contains(t, recordActivityQualificationsSQL, "unnest($1::uuid[], $2::timestamptz[])")
	require.Contains(t, recordActivityQualificationsSQL, "u.ops_last_active_at < qualified.qualified_at")
	require.NotContains(t, recordActivityQualificationsSQL, "date_trunc")
}
