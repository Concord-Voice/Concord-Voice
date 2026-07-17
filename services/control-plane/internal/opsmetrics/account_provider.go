package opsmetrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

const accountCurrentCountsSQL = `
SELECT
    (SELECT COUNT(*) FROM users),
    (SELECT COUNT(*) FROM pending_registrations WHERE expires_at > $1),
    (SELECT COUNT(*) FROM refresh_tokens WHERE revoked_at IS NULL AND expires_at > $1)
`

const accountActiveCountsSQL = `
WITH expired_activity AS (
    UPDATE users
    SET ops_last_active_at = NULL
    WHERE ops_last_active_at < $4
)
SELECT
    COUNT(*) FILTER (WHERE ops_last_active_at >= $1),
    COUNT(*) FILTER (WHERE ops_last_active_at >= $2),
    COUNT(*) FILTER (WHERE ops_last_active_at >= $3),
    COUNT(*) FILTER (WHERE ops_last_active_at >= $4)
FROM users
WHERE ops_last_active_at >= $4
`

const recordActivityQualificationsSQL = `
WITH qualified AS (
    SELECT *
    FROM unnest($1::uuid[], $2::timestamptz[])
        AS qualification_rows(user_id, qualified_at)
)
UPDATE users AS u
SET ops_last_active_at = qualified.qualified_at
FROM qualified
WHERE u.id = qualified.user_id
  AND (
      u.ops_last_active_at IS NULL
      OR u.ops_last_active_at < qualified.qualified_at
  )
`

type accountCurrentCounts struct {
	registered     int64
	pending        int64
	activeSessions int64
}

type accountActiveCounts struct {
	hours24 int64
	days7   int64
	days15  int64
	days30  int64
}

type accountRepository interface {
	CurrentCounts(context.Context, time.Time) (accountCurrentCounts, error)
	ActiveCounts(context.Context, time.Time) (accountActiveCounts, error)
	RecordQualifications(context.Context, []ActivityQualification) error
}

type postgresAccountRepository struct {
	db *sql.DB
}

// AccountProvider reduces application account state to fixed aggregate gauges.
type AccountProvider struct {
	repository accountRepository
	tracker    *ActivityTracker
}

// NewAccountProvider creates the trusted-side PostgreSQL account reducer.
func NewAccountProvider(db *sql.DB, tracker *ActivityTracker) *AccountProvider {
	return newAccountProvider(&postgresAccountRepository{db: db}, tracker)
}

func newAccountProvider(repository accountRepository, tracker *ActivityTracker) *AccountProvider {
	return &AccountProvider{repository: repository, tracker: tracker}
}

// AccountMetrics returns only fixed aggregate keys. Query failures omit their
// affected samples while preserving independently collectable values.
func (provider *AccountProvider) AccountMetrics(ctx context.Context, at time.Time) (map[MetricKey]float64, error) {
	metrics := make(map[MetricKey]float64, 7)
	if provider == nil || provider.repository == nil {
		return metrics, errors.New("account metrics provider is unavailable")
	}
	at = at.UTC()
	var collectedErrors []error

	current, err := provider.repository.CurrentCounts(ctx, at)
	if err != nil {
		collectedErrors = append(collectedErrors, fmt.Errorf("collect current account counts: %w", err))
	} else {
		metrics[MetricRegisteredUsersCurrent] = float64(current.registered)
		metrics[MetricPendingRegistrationsCurrent] = float64(current.pending)
		metrics[MetricActiveSessionsCurrent] = float64(current.activeSessions)
	}

	activeReady := true
	if err := provider.FlushQualifications(ctx, at); err != nil {
		collectedErrors = append(collectedErrors, err)
		activeReady = false
	}

	if activeReady {
		active, err := provider.repository.ActiveCounts(ctx, at)
		if err != nil {
			collectedErrors = append(collectedErrors, fmt.Errorf("collect active account counts: %w", err))
		} else {
			metrics[MetricActiveUsers24H] = float64(active.hours24)
			metrics[MetricActiveUsers7D] = float64(active.days7)
			metrics[MetricActiveUsers15D] = float64(active.days15)
			metrics[MetricActiveUsers30D] = float64(active.days30)
		}
	}

	return metrics, errors.Join(collectedErrors...)
}

// FlushQualifications persists the latest observed activity for users who
// qualified on their current UTC day. Exact acknowledgement keeps retries safe.
func (provider *AccountProvider) FlushQualifications(ctx context.Context, at time.Time) error {
	if provider == nil || provider.repository == nil {
		return errors.New("account metrics provider is unavailable")
	}
	if provider.tracker == nil {
		return nil
	}
	qualifications := provider.tracker.PendingQualifications(at.UTC())
	if len(qualifications) == 0 {
		return nil
	}
	if err := provider.repository.RecordQualifications(ctx, qualifications); err != nil {
		return fmt.Errorf("record activity qualifications: %w", err)
	}
	provider.tracker.AcknowledgeQualifications(qualifications)
	return nil
}

func (repository *postgresAccountRepository) CurrentCounts(ctx context.Context, at time.Time) (accountCurrentCounts, error) {
	var counts accountCurrentCounts
	err := repository.db.QueryRowContext(ctx, accountCurrentCountsSQL, at.UTC()).Scan(
		&counts.registered,
		&counts.pending,
		&counts.activeSessions,
	)
	return counts, err
}

func (repository *postgresAccountRepository) ActiveCounts(ctx context.Context, at time.Time) (accountActiveCounts, error) {
	var counts accountActiveCounts
	err := repository.db.QueryRowContext(
		ctx,
		accountActiveCountsSQL,
		at.Add(-24*time.Hour),
		at.Add(-7*24*time.Hour),
		at.Add(-15*24*time.Hour),
		at.Add(-30*24*time.Hour),
	).Scan(&counts.hours24, &counts.days7, &counts.days15, &counts.days30)
	return counts, err
}

func (repository *postgresAccountRepository) RecordQualifications(ctx context.Context, qualifications []ActivityQualification) error {
	if len(qualifications) == 0 {
		return nil
	}
	userIDs := make([]string, 0, len(qualifications))
	qualifiedAt := make([]time.Time, 0, len(qualifications))
	for _, qualification := range qualifications {
		userIDs = append(userIDs, qualification.UserID.String())
		qualifiedAt = append(qualifiedAt, qualification.QualifiedAt.UTC())
	}
	_, err := repository.db.ExecContext(
		ctx,
		recordActivityQualificationsSQL,
		pq.Array(userIDs),
		pq.Array(qualifiedAt),
	)
	return err
}
