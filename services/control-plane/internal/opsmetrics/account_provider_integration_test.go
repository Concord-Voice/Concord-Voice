package opsmetrics

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresAccountRepositoryIntegrationCountsClosedAccountState(t *testing.T) {
	db := setupAccountProviderIntegrationDB(t)
	repository := &postgresAccountRepository{db: db}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	for _, lastActiveAt := range []time.Time{
		now.Add(-time.Hour),
		now.Add(-24 * time.Hour),
		now.Add(-7 * 24 * time.Hour),
		now.Add(-15 * 24 * time.Hour),
		now.Add(-30 * 24 * time.Hour),
		now.Add(-30*24*time.Hour - time.Microsecond),
	} {
		_, err := db.Exec(`INSERT INTO users (id, ops_last_active_at) VALUES ($1, $2)`, uuid.New(), lastActiveAt)
		require.NoError(t, err)
	}

	for _, expiresAt := range []time.Time{now.Add(time.Hour), now, now.Add(-time.Hour)} {
		_, err := db.Exec(`INSERT INTO pending_registrations (id, expires_at) VALUES ($1, $2)`, uuid.New(), expiresAt)
		require.NoError(t, err)
	}
	revokedAt := now.Add(-time.Minute)
	for _, session := range []struct {
		expiresAt time.Time
		revokedAt *time.Time
	}{
		{expiresAt: now.Add(time.Hour)},
		{expiresAt: now},
		{expiresAt: now.Add(-time.Hour)},
		{expiresAt: now.Add(time.Hour), revokedAt: &revokedAt},
	} {
		_, err := db.Exec(
			`INSERT INTO refresh_tokens (id, expires_at, revoked_at) VALUES ($1, $2, $3)`,
			uuid.New(), session.expiresAt, session.revokedAt,
		)
		require.NoError(t, err)
	}

	current, err := repository.CurrentCounts(context.Background(), now)
	require.NoError(t, err)
	assert.Equal(t, accountCurrentCounts{registered: 6, pending: 1, activeSessions: 1}, current)

	active, err := repository.ActiveCounts(context.Background(), now)
	require.NoError(t, err)
	assert.Equal(t, accountActiveCounts{hours24: 2, days7: 3, days15: 4, days30: 5}, active)
}

// #2033 PR-2 (refinement R-c): the gauge counts erasure obligations that have been
// waiting more than 24 hours, so any non-zero value is the alert spec section 10 describes.
func TestPostgresAccountRepositoryIntegrationCountsOverdueBillingErasures(t *testing.T) {
	db := setupAccountProviderIntegrationDB(t)
	repository := &postgresAccountRepository{db: db}
	ctx := context.Background()

	empty, err := repository.BillingErasureOverdue(ctx, time.Now().UTC())
	require.NoError(t, err)
	assert.Zero(t, empty, "an empty outbox is zero, not an error")

	for name, age := range map[string]string{
		"cus_new":      "0 seconds",
		"cus_23h":      "23 hours",
		"cus_25h":      "25 hours",
		"cus_48h":      "48 hours",
		"cus_30d_late": "30 days",
	} {
		_, err := db.Exec(
			`INSERT INTO billing_erasure_outbox (stripe_customer_id, created_at) VALUES ($1, NOW() - $2::interval)`,
			name, age)
		require.NoError(t, err)
	}

	overdue, err := repository.BillingErasureOverdue(ctx, time.Now().UTC())
	require.NoError(t, err)
	assert.EqualValues(t, 3, overdue, "only rows created more than 24 hours ago are counted")

	provider := newAccountProvider(repository, nil)
	_, err = db.Exec(`INSERT INTO users (id) VALUES ($1)`, uuid.New())
	require.NoError(t, err)
	metrics, err := provider.AccountMetrics(ctx, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, float64(3), metrics[MetricBillingErasureOutboxPending])
}

func TestPostgresAccountRepositoryIntegrationExpiresActivityOutsideThirtyDays(t *testing.T) {
	db := setupAccountProviderIntegrationDB(t)
	repository := &postgresAccountRepository{db: db}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	boundaryUserID := uuid.New()
	expiredUserID := uuid.New()

	_, err := db.Exec(
		`INSERT INTO users (id, ops_last_active_at) VALUES ($1, $2), ($3, $4)`,
		boundaryUserID,
		now.Add(-30*24*time.Hour),
		expiredUserID,
		now.Add(-30*24*time.Hour-time.Microsecond),
	)
	require.NoError(t, err)

	active, err := repository.ActiveCounts(context.Background(), now)
	require.NoError(t, err)
	assert.Equal(t, accountActiveCounts{days30: 1}, active)
	assert.Equal(t, now.Add(-30*24*time.Hour), accountMarker(t, db, boundaryUserID))

	var expiredMarker sql.NullTime
	require.NoError(t, db.QueryRow(
		`SELECT ops_last_active_at FROM users WHERE id = $1`, expiredUserID,
	).Scan(&expiredMarker))
	assert.False(t, expiredMarker.Valid, "markers outside the widest active window must be erased")
}

func TestPostgresAccountRepositoryIntegrationRecordsLatestMonotonicQualifiedActivity(t *testing.T) {
	db := setupAccountProviderIntegrationDB(t)
	repository := &postgresAccountRepository{db: db}
	userID := uuid.New()
	missingUserID := uuid.New()
	_, err := db.Exec(`INSERT INTO users (id) VALUES ($1)`, userID)
	require.NoError(t, err)

	first := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repository.RecordQualifications(context.Background(), []ActivityQualification{
		{UserID: userID, QualifiedAt: first},
		{UserID: missingUserID, QualifiedAt: first},
	}))
	assert.Equal(t, first, accountMarker(t, db, userID))

	require.NoError(t, repository.RecordQualifications(context.Background(), []ActivityQualification{{
		UserID: userID, QualifiedAt: first.Add(6 * time.Hour),
	}}))
	assert.Equal(t, first.Add(6*time.Hour), accountMarker(t, db, userID))

	require.NoError(t, repository.RecordQualifications(context.Background(), []ActivityQualification{{
		UserID: userID, QualifiedAt: first.Add(3 * time.Hour),
	}}))
	assert.Equal(t, first.Add(6*time.Hour), accountMarker(t, db, userID), "older activity must not regress the marker")

	nextDay := first.Add(24 * time.Hour)
	require.NoError(t, repository.RecordQualifications(context.Background(), []ActivityQualification{{
		UserID: userID, QualifiedAt: nextDay,
	}}))
	assert.Equal(t, nextDay, accountMarker(t, db, userID))

	_, err = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, repository.RecordQualifications(context.Background(), []ActivityQualification{{
		UserID: userID, QualifiedAt: nextDay.Add(24 * time.Hour),
	}}))
}

func setupAccountProviderIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	db := setupOpsMetricsIntegrationDB(t)
	_, err := db.Exec(`
		DROP TABLE IF EXISTS billing_erasure_outbox, refresh_tokens, pending_registrations, users CASCADE;
		CREATE TABLE billing_erasure_outbox (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			stripe_customer_id TEXT NOT NULL UNIQUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE users (
			id UUID PRIMARY KEY,
			ops_last_active_at TIMESTAMPTZ
		);
		CREATE TABLE pending_registrations (
			id UUID PRIMARY KEY,
			expires_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE refresh_tokens (
			id UUID PRIMARY KEY,
			expires_at TIMESTAMPTZ NOT NULL,
			revoked_at TIMESTAMPTZ
		);
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.Exec(`DROP TABLE IF EXISTS billing_erasure_outbox, refresh_tokens, pending_registrations, users CASCADE`)
		assert.NoError(t, cleanupErr)
	})
	return db
}

func accountMarker(t *testing.T, db *sql.DB, userID uuid.UUID) time.Time {
	t.Helper()
	var marker time.Time
	require.NoError(t, db.QueryRow(
		`SELECT ops_last_active_at FROM users WHERE id = $1`, userID,
	).Scan(&marker))
	return marker.UTC()
}
