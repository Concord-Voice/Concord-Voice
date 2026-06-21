package entitlements_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq" // register postgres driver for the unreachable-DSN error case
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertUser creates a minimal users row so the subscriptions FK is satisfied.
func insertUser(t *testing.T, ts *testhelpers.TestServer) string {
	t.Helper()
	id := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, $4, true, true)`,
		id, id+"@test.local", "u"+id[:8], testhelpers.TestAuthHash,
	)
	require.NoError(t, err)
	return id
}

func insertSub(t *testing.T, ts *testhelpers.TestServer, userID, tier, status string, periodEnd *time.Time) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
		 VALUES ($1, $2, $3, 'code', $4)`,
		userID, tier, status, periodEnd,
	)
	require.NoError(t, err)
}

func TestCache_SatisfiesTierResolver(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	var r entitlements.TierResolver = entitlements.NewCache(ts.Redis, ts.DB)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "active", nil)
	assert.Equal(t, "premium", r.GetTier(context.Background(), uid))
}

func TestResolveTier_ActivePremium(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "active", nil)
	assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTier(context.Background(), ts.DB, uid))
}

func TestResolveTier_NoRowFreeDefault(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTier(context.Background(), ts.DB, uid))
}

func TestResolveTier_CanceledStatusFree(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "canceled", nil)
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTier(context.Background(), ts.DB, uid))
}

func TestResolveTier_ExpiredPeriodFree(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	past := time.Now().Add(-1 * time.Hour)
	insertSub(t, ts, uid, "premium", "active", &past)
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTier(context.Background(), ts.DB, uid))
}

func TestResolveTier_DBErrorFailsClosedToFree(t *testing.T) {
	// Unreachable DSN: QueryRowContext errors -> ResolveTier fails closed to free.
	// nosemgrep: go.secrets.pg.pg-hardcoded-secret.pg-hardcoded-secret -- unreachable-host test fixture, not a real credential (mirrors age/integration_test.go)
	db, err := sql.Open("postgres", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTier(context.Background(), db, uuid.New().String()))
}
