package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// This file is in package subscriptions (not subscriptions_test) because the
// sweeper's resolveTier field is unexported: the only way to make the
// post-flip tier read fail is to replace it.

type resolveRecorder struct {
	mu    sync.Mutex
	calls int
}

func (r *resolveRecorder) OnTierChange(context.Context, uuid.UUID, string, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func (r *resolveRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

var errResolveBoom = errors.New("resolve boom")

func lapsedCodeRow(t *testing.T, db *sql.DB, user uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.QueryRow(`
		INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
		VALUES ($1, 'premium', 'active', 'code', $2)
		RETURNING id`, user, time.Now().Add(-time.Hour)).Scan(&id))
	return id
}

func rowStatus(t *testing.T, db *sql.DB, id uuid.UUID) string {
	t.Helper()
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&status))
	return status
}

// TestExpireOne_FailedTierReadRollsBackTheFlip: since migration 000179 a user
// can hold a live Stripe row beside the expiring code row, so a failed
// post-flip read must not be reported as free. expireOne returns the error,
// the flip rolls back (the row stays live for the next sweep) and nobody is
// notified. SweepExpired logs the per-row error and counts the row as not
// swept, like a failed notify.
func TestExpireOne_FailedTierReadRollsBackTheFlip(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := testdb.CreateUser(t, db)
	rowID := lapsedCodeRow(t, db, user)

	rec := &resolveRecorder{}
	s := NewExpirySweeperWith(db, logger.New("test"), rec)
	var resolved int
	s.resolveTier = func(context.Context, entitlements.Querier, string) (string, error) {
		resolved++
		return "", errResolveBoom
	}

	err := s.expireOne(ctx, expiredSubscription{id: rowID, userID: user, oldTier: "premium"})
	require.ErrorIs(t, err, errResolveBoom)
	assert.Contains(t, err.Error(), "resolve tier after expiry")
	assert.Equal(t, 1, resolved, "the injected resolver ran: the failure is the read's, not the flip's")
	assert.Equal(t, "active", rowStatus(t, db, rowID), "the failed read rolls the flip back; the row stays live")
	assert.Zero(t, rec.count(), "no tier change may be reported when the post-flip tier is unknown")

	n, err := s.SweepExpired(ctx)
	require.NoError(t, err, "a per-row failure is logged and skipped, not fatal to the sweep")
	assert.Equal(t, 0, n, "the row whose tier read failed is not counted as swept")
	assert.Equal(t, "active", rowStatus(t, db, rowID))
	assert.Zero(t, rec.count())

	// The next sweep with a working resolver converges the row.
	s.resolveTier = entitlements.ResolveTierChecked
	n, err = s.SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "expired", rowStatus(t, db, rowID))
	assert.Equal(t, 1, rec.count())
}
