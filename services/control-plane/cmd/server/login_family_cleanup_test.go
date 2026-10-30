package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loginFamilyCleanupStores(t *testing.T) (*sql.DB, *redis.Client) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL must explicitly select an isolated database")
	}
	db, _ := testdb.SetupTestDB(t)
	rdb := redistest.Client(t)
	return db, rdb
}

// The callbacks inspect the actual stores. Moving the new phase ahead of any
// existing phase would leave that phase's fixture visible here.
func TestLoginFamilyCleanupRunsLastWithOwnBudget(t *testing.T) {
	db, rdb := loginFamilyCleanupStores(t)
	user := testdb.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, revoked_at) VALUES
 ($1, 'cleanup-expired', NOW() - INTERVAL '1 day', NULL),
 ($1, 'cleanup-revoked', NOW() + INTERVAL '1 day', NOW() - INTERVAL '91 days'),
 ($1, 'cleanup-live', NOW() + INTERVAL '1 day', NULL)`, user)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO step_up_tokens (token_hash, user_id, factor, purpose, expires_at)
 VALUES (decode(repeat('ab',32),'hex'), $1, 'password', 'cleanup-test', NOW() - INTERVAL '1 day')`, user)
	require.NoError(t, err)
	key := "presence:" + uuid.NewString()
	require.NoError(t, rdb.Set(context.Background(), key, "online", 0).Err())
	t.Cleanup(func() { assert.NoError(t, rdb.Del(context.Background(), key).Err()) })
	var logs bytes.Buffer
	var ownershipCtx context.Context
	ownershipCalls, sweepCalls := 0, 0
	before := time.Now()
	ownership := func(ctx context.Context) {
		ownershipCalls++
		ownershipCtx = ctx
		var refresh, stepUp int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM refresh_tokens`).Scan(&refresh))
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM step_up_tokens`).Scan(&stepUp))
		assert.Equal(t, 1, refresh, "both old refresh-token phases must precede ownership cleanup")
		assert.Zero(t, stepUp, "step-up sweep must precede ownership cleanup")
		exists, queryErr := rdb.Exists(ctx, key).Result()
		require.NoError(t, queryErr)
		assert.Zero(t, exists, "presence cleanup must precede ownership cleanup")
	}
	sweep := func(ctx context.Context, gotDB *sql.DB, limit int) (loginfamily.GCResult, error) {
		sweepCalls++
		assert.Equal(t, 1, ownershipCalls, "family GC must be the final phase")
		assert.Same(t, db, gotDB)
		assert.NotSame(t, ownershipCtx, ctx, "family GC must not spend the earlier tasks' context")
		assert.Equal(t, 100, limit)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		taskDeadline, ok := ownershipCtx.Deadline()
		require.True(t, ok)
		assert.NotEqual(t, taskDeadline, deadline, "family GC budget must be its own, not the 30 s task budget")
		assert.False(t, deadline.After(time.Now().Add(loginFamilySweepBudget)), "family GC budget must stay bounded")
		assert.Greater(t, time.Until(deadline), loginFamilySweepBudget-5*time.Second)
		assert.True(t, deadline.After(before), "family GC deadline starts after the earlier tasks")
		// Exercise the real adapter as well as inspecting its invocation contract.
		return loginfamily.SweepRetiredEvidence(ctx, gotDB, limit)
	}
	runCleanupWithLoginFamilySweep(context.Background(), db, rdb, logger.NewWithWriter(&logs), ownership, sweep)
	assert.Equal(t, 1, sweepCalls)
	assert.Contains(t, logs.String(), "incomplete=false")
	assert.Contains(t, logs.String(), "failed=false")
}

func TestLoginFamilyCleanupReportsCommittedCountsAndIncompleteHonestly(t *testing.T) {
	db, rdb := loginFamilyCleanupStores(t)
	for _, tc := range []struct {
		name       string
		incomplete bool
		err        error
		wantLevel  string
	}{
		{"complete", false, nil, "INFO"},
		{"retained work", true, nil, "WARN"},
		{"failure after committed progress", false, errors.New("private-database-detail"), "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			calls := 0
			sweep := func(context.Context, *sql.DB, int) (loginfamily.GCResult, error) {
				calls++
				return loginfamily.GCResult{DeletedSIDs: 2, DeletedFamilies: 3, DeletedOperations: 5,
					DeletedConnectionClosures: 7, DeletedRoomClosures: 11, Incomplete: tc.incomplete}, tc.err
			}
			runCleanupWithLoginFamilySweep(context.Background(), db, rdb, logger.NewWithWriter(&logs), nil, sweep)
			assert.Equal(t, 1, calls)
			var aggregate string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "Cleanup: login family evidence sweep finished") {
					aggregate = line
				}
			}
			require.NotEmpty(t, aggregate)
			for _, field := range []string{"deleted_sids=2", "deleted_families=3", "deleted_operations=5", "deleted_connection_closures=7", "deleted_room_closures=11"} {
				assert.Contains(t, aggregate, field)
			}
			assert.Contains(t, aggregate, "level="+tc.wantLevel)
			if tc.incomplete || tc.err != nil {
				assert.Contains(t, aggregate, "incomplete=true")
			} else {
				assert.Contains(t, aggregate, "incomplete=false")
			}
			if tc.err != nil {
				assert.Contains(t, aggregate, "failed=true")
			} else {
				assert.Contains(t, aggregate, "failed=false")
			}
			assert.NotContains(t, logs.String(), "private-database-detail")
			assert.NotContains(t, logs.String(), "Cleanup completed")
		})
	}
}

func TestLoginFamilyCleanupCannotRenewExhaustedBudget(t *testing.T) {
	db, rdb := loginFamilyCleanupStores(t)
	for _, tc := range []struct {
		name                  string
		expireBeforeOwnership bool
	}{
		{"parent already exhausted", true}, {"ownership exhausts remaining budget", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.expireBeforeOwnership {
				cancel()
			}
			var logs bytes.Buffer
			var ownershipCtx context.Context
			ownership := func(got context.Context) { ownershipCtx = got; cancel() }
			sweep := func(got context.Context, gotDB *sql.DB, limit int) (loginfamily.GCResult, error) {
				require.NotNil(t, ownershipCtx)
				// Its own budget never outlives a canceled parent.
				assert.ErrorIs(t, got.Err(), context.Canceled)
				return loginfamily.SweepRetiredEvidence(got, gotDB, limit)
			}
			runCleanupWithLoginFamilySweep(ctx, db, rdb, logger.NewWithWriter(&logs), ownership, sweep)
			assert.Contains(t, logs.String(), "incomplete=true")
			assert.Contains(t, logs.String(), "failed=true")
			assert.NotContains(t, logs.String(), "Cleanup completed")
		})
	}
}

func TestLoginFamilyCleanupProductionCallerDoesNotWorkPastDeadline(t *testing.T) {
	db, rdb := loginFamilyCleanupStores(t)
	user := testdb.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
 VALUES ($1, 'cleanup-canceled-retained', NOW() - INTERVAL '1 day')`, user)
	require.NoError(t, err)
	key := "presence:" + uuid.NewString()
	require.NoError(t, rdb.Set(context.Background(), key, "online", 0).Err())
	t.Cleanup(func() { assert.NoError(t, rdb.Del(context.Background(), key).Err()) })
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var logs bytes.Buffer
	runCleanup(ctx, db, rdb, logger.NewWithWriter(&logs), nil)
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE token_hash = 'cleanup-canceled-retained'`).Scan(&remaining))
	assert.Equal(t, 1, remaining, "expired parent must prevent SQL cleanup mutations")
	exists, err := rdb.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, exists, "expired parent must prevent Redis cleanup mutations")
	assert.Contains(t, logs.String(), "Cleanup: login family evidence sweep finished")
	assert.Contains(t, logs.String(), "incomplete=true")
	assert.Contains(t, logs.String(), "failure_class=deadline_exceeded")
	assert.NotContains(t, logs.String(), "Cleanup completed")
}

func TestLoginFamilyCleanupPreservesShorterParentDeadline(t *testing.T) {
	db, rdb := loginFamilyCleanupStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var logs bytes.Buffer
	calls := 0
	runCleanupWithLoginFamilySweep(ctx, db, rdb, logger.NewWithWriter(&logs), nil,
		func(got context.Context, _ *sql.DB, _ int) (loginfamily.GCResult, error) {
			calls++
			actual, ok := got.Deadline()
			assert.True(t, ok)
			assert.Equal(t, deadline, actual, "cleanup cannot replenish its parent's remaining budget")
			cancel()
			// A late successful return cannot claim completion after its budget expired.
			return loginfamily.GCResult{DeletedSIDs: 13}, nil
		})
	assert.Equal(t, 1, calls)
	assert.Contains(t, logs.String(), "deleted_sids=13")
	assert.Contains(t, logs.String(), "incomplete=true")
	assert.Contains(t, logs.String(), "failed=true")
	assert.Contains(t, logs.String(), "failure_class=canceled")
}
