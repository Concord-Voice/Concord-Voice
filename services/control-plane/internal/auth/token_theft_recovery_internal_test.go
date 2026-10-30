package auth

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueTokenTheftRevocation_PersistsFallbackAfterEnqueueContextExpires(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	require.NoError(t, redisClient.ConfigSet(t.Context(), "appendonly", "yes").Err())
	userID := dbtest.CreateUser(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	queuedInDB, err := queueTokenTheftRevocation(ctx, db, redisClient, userID.String())
	require.NoError(t, err)
	assert.False(t, queuedInDB)
	blocked, err := redisClient.Exists(context.Background(), middleware.TokenTheftBlockedKey(userID.String())).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, blocked)
}

func TestQueueTokenTheftRevocation_RejectsFallbackWithoutAOFReceipt(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	userID := dbtest.CreateUser(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Force the Redis fallback path after PostgreSQL rejects enqueue.

	queuedInDB, err := queueTokenTheftRevocation(ctx, db, redisClient, userID.String())
	assert.False(t, queuedInDB)
	require.ErrorContains(t, err, "sync token theft recovery state to Redis AOF")
	assert.True(t, mini.Exists(middleware.TokenTheftBlockedKey(userID.String())))
}

func TestQueueTokenTheftRevocation_RedisStallStillCommitsPendingRow(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { defer func() { _ = conn.Close() }(); <-done }()
		}
	}()
	redisClient := redis.NewClient(&redis.Options{Addr: listener.Addr().String(), MaxRetries: -1})
	t.Cleanup(func() { _ = redisClient.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), tokenTheftEnqueueTimeout)
	defer cancel()

	queuedInDB, err := queueTokenTheftRevocation(ctx, db, redisClient, userID.String())
	assert.True(t, queuedInDB, "a stalled Redis handshake must leave time for the SQL record")
	require.Error(t, err)
	var pending bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM token_theft_pending_revocations WHERE user_id = $1)`, userID).Scan(&pending))
	assert.True(t, pending)
}

func TestRefreshMintsRejectPendingTheftInsideUserLock(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "unblocked control"
		if pending {
			name = "durable pending theft"
		}
		t.Run(name, func(t *testing.T) {
			// The production constructor supplies the same entitlement cache that
			// both mint paths consult before opening their transactions.
			rig := newFamilyTestRig(t)
			require.NotNil(t, rig.h.entCache)
			access, _, sourceID, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
			require.NoError(t, err)
			sourceFamily, _ := assertFamilyAccess(t, rig, access)
			var expiry time.Time
			require.NoError(t, rig.db.QueryRow(`SELECT expires_at FROM refresh_tokens WHERE id=$1`, sourceID).Scan(&expiry))
			graceAccess, graceSecret, revokedID, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
			require.NoError(t, err)
			graceFamily, _ := assertFamilyAccess(t, rig, graceAccess)
			require.NotEqual(t, sourceFamily, graceFamily)
			c, rec := familyTestContext()
			c.Request.Header.Set("X-Refresh-Token", graceSecret)
			rig.h.Refresh(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rotated := familyResponse(t, rec)
			rotatedFamily, _ := assertFamilyAccess(t, rig, rotated.Access)
			require.Equal(t, graceFamily, rotatedFamily)

			// Only the durable SQL guard can refuse these otherwise valid inputs:
			// no process-local hold or Redis deny marker is installed.
			require.False(t, middleware.LocalTokenTheftPending(rig.user))
			blocked, err := rig.h.redis.Exists(t.Context(), middleware.TokenTheftBlockedKey(rig.user)).Result()
			require.NoError(t, err)
			require.Zero(t, blocked)
			if pending {
				require.NoError(t, EnqueueTokenTheftRevocation(t.Context(), rig.db, rig.user))
			}
			// Include every column, so revocation timestamps, immutable SID expiry,
			// family owners, and inserted successors are all covered by equality.
			snapshot := func() string {
				var state string
				require.NoError(t, rig.db.QueryRow(`SELECT jsonb_build_object(
					'refresh', (SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM refresh_tokens r WHERE user_id=$1),
					'sids', (SELECT jsonb_agg(to_jsonb(s) ORDER BY sid) FROM login_family_sids s WHERE user_id=$1),
					'families', (SELECT jsonb_agg(to_jsonb(f) ORDER BY family_id) FROM login_families f WHERE user_id=$1),
					'owners', (SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM login_family_revocations o WHERE user_id=$1)
				)::text`, rig.user).Scan(&state))
				return state
			}
			before := snapshot()
			c, rec = familyTestContext()
			rig.h.rotateAndRespond(c, models.RefreshToken{ID: sourceID, UserID: rig.user, ExpiresAt: expiry}, "")
			if pending {
				require.Equal(t, http.StatusUnauthorized, rec.Code)
				require.JSONEq(t, `{"error":"`+errMsgInvalidRefreshToken+`"}`, rec.Body.String())
				require.Empty(t, rec.Header().Values("Set-Cookie"))
				require.JSONEq(t, before, snapshot())
			} else {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				pair := familyResponse(t, rec)
				family, _ := assertFamilyAccess(t, rig, pair.Access)
				require.Equal(t, sourceFamily, family)
			}
			c, rec = familyTestContext()
			rig.h.handleGracePeriodRefresh(c, rig.user, revokedID, time.Now())
			if pending {
				require.Equal(t, http.StatusUnauthorized, rec.Code)
				require.JSONEq(t, `{"error":"`+errMsgInvalidRefreshToken+`"}`, rec.Body.String())
				require.Empty(t, rec.Header().Values("Set-Cookie"))
				require.JSONEq(t, before, snapshot())
			} else {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				pair := familyResponse(t, rec)
				family, _ := assertFamilyAccess(t, rig, pair.Access)
				require.Equal(t, graceFamily, family)
			}
			var active, total int
			require.NoError(t, rig.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE revoked_at IS NULL), COUNT(*) FROM refresh_tokens WHERE user_id=$1`, rig.user).Scan(&active, &total))
			assert.Equal(t, 2, active)
			if pending {
				assert.Equal(t, 3, total, "both denied mints must leave the original source and grace chain intact")
			} else {
				assert.Equal(t, 5, total, "both unblocked controls must actually mint a successor")
			}
		})
	}
}

func TestQueueTokenTheftRevocation_ConcurrentEventsKeepNewestFallback(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	require.NoError(t, redisClient.ConfigSet(t.Context(), "appendonly", "yes").Err())
	userID := dbtest.CreateUser(t, db)
	const barrierKey int64 = 0x544846424152
	_, err := db.Exec(`
		CREATE FUNCTION token_theft_order_barrier() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(92669393125714);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER token_theft_order_barrier BEFORE INSERT OR UPDATE ON token_theft_pending_revocations
		FOR EACH ROW EXECUTE FUNCTION token_theft_order_barrier()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS token_theft_order_barrier ON token_theft_pending_revocations`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS token_theft_order_barrier()`)
	})
	barrierConn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, barrierConn.Close()) }()
	_, err = barrierConn.ExecContext(t.Context(), `SELECT pg_advisory_lock($1)`, barrierKey)
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			_, _ = barrierConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, barrierKey)
		}
	}()

	firstDone := make(chan error, 1)
	go func() {
		_, queueErr := queueTokenTheftRevocation(t.Context(), db, redisClient, userID.String())
		firstDone <- queueErr
	}()
	var firstOperation string
	require.Eventually(t, func() bool {
		firstOperation, err = redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, userID.String()).Result()
		return err == nil && firstOperation != ""
	}, 5*time.Second, 20*time.Millisecond)

	secondDone := make(chan error, 1)
	go func() {
		_, queueErr := queueTokenTheftRevocation(t.Context(), db, redisClient, userID.String())
		secondDone <- queueErr
	}()
	select {
	case queueErr := <-secondDone:
		t.Fatalf("second theft event overtook the first: %v", queueErr)
	case <-time.After(100 * time.Millisecond):
	}
	currentOperation, err := redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, firstOperation, currentOperation)
	_, err = barrierConn.ExecContext(t.Context(), `SELECT pg_advisory_unlock($1)`, barrierKey)
	require.NoError(t, err)
	released = true
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)

	var databaseOperation string
	require.NoError(t, db.QueryRow(`SELECT operation_id FROM token_theft_pending_revocations WHERE user_id = $1`, userID).Scan(&databaseOperation))
	currentOperation, err = redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, userID.String()).Result()
	require.NoError(t, err)
	assert.NotEqual(t, firstOperation, currentOperation)
	assert.Equal(t, databaseOperation, currentOperation)
	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, userID.String()))
	remaining, err := redisClient.HLen(t.Context(), tokenTheftFallbackHashKey).Result()
	require.NoError(t, err)
	assert.Zero(t, remaining, "the completed event must leave no stale fallback to replay")
}

func TestPromoteTokenTheftFallback_DoesNotResetCompletedSweep(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	completedOperation, fallbackOperation := uuid.NewString(), uuid.NewString()
	_, err := db.Exec(`INSERT INTO token_theft_pending_revocations (user_id, operation_id, completed_at)
		VALUES ($1, $2, NOW())`, userID, completedOperation)
	require.NoError(t, err)
	require.NoError(t, redisClient.HSet(t.Context(), tokenTheftFallbackHashKey, userID.String(), fallbackOperation).Err())
	require.NoError(t, redisClient.Set(t.Context(), middleware.TokenTheftBlockedKey(userID.String()), fallbackOperation, 0).Err())

	added, err := promoteTokenTheftFallback(t.Context(), db, redisClient, userID.String(), &fallbackOperation)
	require.NoError(t, err)
	assert.True(t, added)
	var operation string
	var completedAt sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT operation_id, completed_at FROM token_theft_pending_revocations WHERE user_id = $1`, userID).Scan(&operation, &completedAt))
	assert.Equal(t, completedOperation, operation)
	assert.True(t, completedAt.Valid)

	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, userID.String()))
	added, err = promoteTokenTheftFallback(t.Context(), db, redisClient, userID.String(), &fallbackOperation)
	require.NoError(t, err)
	assert.True(t, added)
	require.NoError(t, db.QueryRow(`SELECT operation_id FROM token_theft_pending_revocations WHERE user_id = $1`, userID).Scan(&operation))
	assert.Equal(t, fallbackOperation, operation)
}

func TestPromoteTokenTheftFallback_DoesNotReviveStaleSnapshot(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	operationID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO token_theft_pending_revocations (user_id, operation_id)
		VALUES ($1, $2)`, userID, operationID)
	require.NoError(t, err)
	require.NoError(t, redisClient.HSet(t.Context(), tokenTheftFallbackHashKey, userID.String(), operationID).Err())
	require.NoError(t, redisClient.Set(t.Context(), middleware.TokenTheftBlockedKey(userID.String()), operationID, 0).Err())
	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, userID.String()))
	newTokenID := uuid.NewString()
	_, err = db.Exec(`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`, newTokenID, userID, HashRefreshToken("post-recovery-session"))
	require.NoError(t, err)

	added, err := promoteTokenTheftFallback(t.Context(), db, redisClient, userID.String(), &operationID)
	require.NoError(t, err)
	assert.False(t, added)
	var pending int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID).Scan(&pending))
	assert.Zero(t, pending)
	var active bool
	require.NoError(t, db.QueryRow(`SELECT revoked_at IS NULL FROM refresh_tokens WHERE id = $1`, newTokenID).Scan(&active))
	assert.True(t, active, "a stale fallback must not revoke sessions minted after recovery")
}

func TestProcessTokenTheftRevocation_RetiresFallbackPresentBeforeSweep(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	olderOperation := uuid.NewString()
	require.NoError(t, redisClient.HSet(t.Context(), tokenTheftFallbackHashKey, userID.String(), olderOperation).Err())
	require.NoError(t, redisClient.Set(t.Context(), middleware.TokenTheftBlockedKey(userID.String()), olderOperation, 0).Err())
	require.NoError(t, EnqueueTokenTheftRevocation(t.Context(), db, userID.String()))
	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, userID.String()))

	_, err := redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, userID.String()).Result()
	assert.ErrorIs(t, err, redis.Nil)
	newTokenID := uuid.NewString()
	_, err = db.Exec(`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`, newTokenID, userID, HashRefreshToken("after-newer-sweep"))
	require.NoError(t, err)
	require.NoError(t, RecoverPendingTokenTheftRevocations(t.Context(), db, redisClient, logger.New("test")))
	var active bool
	require.NoError(t, db.QueryRow(`SELECT revoked_at IS NULL FROM refresh_tokens WHERE id = $1`, newTokenID).Scan(&active))
	assert.True(t, active)
}

func TestFinalizeTokenTheftRevocation_PreservesFallbackPublishedAfterSweep(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	oldID, completedID, newerID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := db.Exec(`INSERT INTO token_theft_pending_revocations
		(user_id, operation_id, superseded_operation_id, completed_at) VALUES ($1, $2, $3, NOW())`, userID, completedID, oldID)
	require.NoError(t, err)
	require.NoError(t, redisClient.HSet(t.Context(), tokenTheftFallbackHashKey, userID.String(), newerID).Err())
	require.NoError(t, redisClient.Set(t.Context(), middleware.TokenTheftBlockedKey(userID.String()), newerID, 0).Err())
	require.NoError(t, finalizeTokenTheftRevocation(t.Context(), db, redisClient, userID.String(), sql.NullString{String: completedID, Valid: true}))
	current, err := redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, newerID, current)
}

func TestRetryTokenTheftRecoveryOnce_ResumesCompletedRowWithoutNewSweep(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	operationID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO token_theft_pending_revocations (user_id, operation_id, completed_at)
		VALUES ($1, $2, NOW())`, userID, operationID)
	require.NoError(t, err)
	newTokenID := uuid.NewString()
	_, err = db.Exec(`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`, newTokenID, userID, HashRefreshToken("after-completed-sweep"))
	require.NoError(t, err)
	h := &Handler{db: db, redis: redisClient}
	queued, err := h.retryTokenTheftRecoveryOnce(t.Context(), userID.String(), false)
	require.NoError(t, err)
	assert.True(t, queued)
	var active bool
	require.NoError(t, db.QueryRow(`SELECT revoked_at IS NULL FROM refresh_tokens WHERE id = $1`, newTokenID).Scan(&active))
	assert.True(t, active)
}

func TestFinalizeTokenTheftRevocation_KeepsRowWithoutAOFReceipt(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	userID := dbtest.CreateUser(t, db)
	operationID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO token_theft_pending_revocations (user_id, operation_id, completed_at)
		VALUES ($1, $2, NOW())`, userID, operationID)
	require.NoError(t, err)
	require.ErrorContains(t, finalizeTokenTheftRevocation(t.Context(), db, redisClient, userID.String(), sql.NullString{String: operationID, Valid: true}), "sync token theft recovery cleanup to Redis AOF")
	var pending bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM token_theft_pending_revocations WHERE user_id = $1)`, userID).Scan(&pending))
	assert.True(t, pending)
}

func TestProcessTokenTheftRevocation_KeepsCompletedRowWithoutLegacyCleanupAOFReceipt(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	userID := dbtest.CreateUser(t, db)
	_, err := db.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, operation_id, completed_at) VALUES ($1, $2, NOW())`,
		userID, uuid.New(),
	)
	require.NoError(t, err)
	require.NoError(t, redisClient.SAdd(t.Context(), tokenTheftLegacyFallbackSetKey, userID.String()).Err())

	err = ProcessTokenTheftRevocation(t.Context(), db, redisClient, userID.String())
	require.ErrorContains(t, err, "sync token theft recovery cleanup to Redis AOF")
	var pending bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM token_theft_pending_revocations WHERE user_id = $1)`, userID).Scan(&pending))
	assert.True(t, pending, "the SQL row must remain until legacy retirement is durable")
}

func TestRetryTokenTheftRecovery_ReleasesLocalDenyAfterRedisFallback(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	require.NoError(t, redisClient.ConfigSet(t.Context(), "appendonly", "yes").Err())
	userID := dbtest.CreateUser(t, db)
	require.NoError(t, installTokenTheftRetryEnqueueFailure(db))
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS token_theft_retry_enqueue_failure()`)
	})
	release := middleware.HoldLocalTokenTheftPending(userID.String())
	t.Cleanup(release)
	h := &Handler{db: db, redis: redisClient, log: logger.New("test")}
	h.retryTokenTheftRecovery(userID.String(), true, release)
	assert.Eventually(t, func() bool { return !middleware.LocalTokenTheftPending(userID.String()) }, 5*time.Second, 25*time.Millisecond)
	assert.True(t, redisClient.HExists(t.Context(), tokenTheftFallbackHashKey, userID.String()).Val())
}

func TestRetryTokenTheftRecovery_QueuesDistinctEventsForOneUser(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	require.NoError(t, redisClient.ConfigSet(t.Context(), "appendonly", "yes").Err())
	userID := dbtest.CreateUser(t, db)
	require.NoError(t, installTokenTheftRetryEnqueueFailure(db))
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS token_theft_retry_enqueue_failure()`)
	})
	const forceFailureKey = "token_theft_recovery_test:force_failure"
	require.NoError(t, redisClient.Set(t.Context(), forceFailureKey, "1", 0).Err())
	originalScript := persistTokenTheftRecoveryStateScript
	persistTokenTheftRecoveryStateScript = redis.NewScript(`
if redis.call("GET", "token_theft_recovery_test:force_failure") == "1" then
  return redis.error_reply("forced token theft recovery state failure")
end
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("SET", KEYS[2], ARGV[2])
return 1
`)
	t.Cleanup(func() { persistTokenTheftRecoveryStateScript = originalScript })
	h := &Handler{db: db, redis: redisClient, log: logger.New("test")}
	releaseFirst := middleware.HoldLocalTokenTheftPending(userID.String())
	releaseSecond := middleware.HoldLocalTokenTheftPending(userID.String())
	t.Cleanup(releaseFirst)
	t.Cleanup(releaseSecond)
	h.retryTokenTheftRecovery(userID.String(), true, releaseFirst)
	h.retryTokenTheftRecovery(userID.String(), true, releaseSecond)
	h.theftRetryMu.Lock()
	queued := len(h.theftRetries[userID.String()])
	h.theftRetryMu.Unlock()
	assert.Equal(t, 2, queued)
	require.NoError(t, redisClient.Del(t.Context(), forceFailureKey).Err())
	_, err := db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		h.theftRetryMu.Lock()
		_, running := h.theftRetries[userID.String()]
		h.theftRetryMu.Unlock()
		return !running && !middleware.LocalTokenTheftPending(userID.String())
	}, 10*time.Second, 25*time.Millisecond)
}

func TestProcessTokenTheftRevocation_NoPendingRecordIsNoop(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)

	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, uuid.NewString()))
}

func TestProcessTokenTheftRevocation_CompletesLegacyPendingRecord(t *testing.T) {
	rig := newFamilyTestRig(t)
	db, redisClient := rig.db, rig.h.redis
	userID := uuid.MustParse(rig.user)
	access, _, sid, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
	require.NoError(t, err)
	assertFamilyAccess(t, rig, access)
	_, err = db.Exec(`INSERT INTO token_theft_pending_revocations (user_id) VALUES ($1)`, userID)
	require.NoError(t, err)
	var legacyOperation sql.NullString
	require.NoError(t, db.QueryRow(`SELECT operation_id FROM token_theft_pending_revocations WHERE user_id=$1`, userID).Scan(&legacyOperation))
	require.False(t, legacyOperation.Valid)

	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, userID.String()))

	var active, pending int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
	).Scan(&active))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
	).Scan(&pending))
	assert.Zero(t, active)
	assert.Zero(t, pending)
	blocked, err := redisClient.Exists(t.Context(), middleware.TokenTheftBlockedKey(userID.String())).Result()
	require.NoError(t, err)
	assert.Zero(t, blocked)
	var terminal, revoked, theftOwner bool
	require.NoError(t, db.QueryRow(`SELECT f.terminal_at IS NOT NULL, r.revoked_at IS NOT NULL,
		o.reason='theft' AND o.scope='user' AND o.sealed_at IS NOT NULL
		AND EXISTS (SELECT 1 FROM login_family_revocation_targets t WHERE t.operation_id=o.operation_id AND t.family_id=f.family_id AND t.user_id=f.user_id)
		FROM login_family_sids s JOIN login_families f USING (family_id,user_id)
		JOIN refresh_tokens r ON r.id=s.sid AND r.user_id=s.user_id
		JOIN login_family_revocations o ON o.operation_id=f.auth_operation_id AND o.user_id=f.user_id
		WHERE s.sid=$1`, sid).Scan(&terminal, &revoked, &theftOwner))
	assert.True(t, terminal)
	assert.True(t, revoked)
	assert.True(t, theftOwner)

	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, rig.user))
	var generation int64
	require.NoError(t, db.QueryRow(`SELECT token_theft_generation FROM users WHERE id=$1`, userID).Scan(&generation))
	assert.EqualValues(t, 1, generation)
}

func TestFinalizeTokenTheftRevocation_PreservesIncompleteRecord(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	operationID := uuid.NewString()
	_, err := db.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, operation_id) VALUES ($1, $2)`, userID, operationID,
	)
	require.NoError(t, err)

	require.NoError(t, finalizeTokenTheftRevocation(t.Context(), db, redisClient, userID.String(), sql.NullString{String: operationID, Valid: true}))

	var pending int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}

func TestRecoverPendingTokenTheftRevocations_PromotesRedisFallbacks(t *testing.T) {
	tests := []struct {
		name  string
		stage func(context.Context, *redis.Client, string) error
	}{
		{
			name: "operation-scoped fallback",
			stage: func(ctx context.Context, redisClient *redis.Client, userID string) error {
				operationID := uuid.NewString()
				if err := redisClient.HSet(ctx, tokenTheftFallbackHashKey, userID, operationID).Err(); err != nil {
					return err
				}
				return redisClient.Set(ctx, middleware.TokenTheftBlockedKey(userID), operationID, 0).Err()
			},
		},
		{
			name: "legacy fallback",
			stage: func(ctx context.Context, redisClient *redis.Client, userID string) error {
				if err := redisClient.SAdd(ctx, tokenTheftLegacyFallbackSetKey, userID).Err(); err != nil {
					return err
				}
				return redisClient.Set(ctx, middleware.TokenTheftBlockedKey(userID), tokenTheftLegacyStateValue, 0).Err()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			db, redisClient := rig.db, rig.h.redis
			userID := uuid.MustParse(rig.user)
			access, _, sid, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
			require.NoError(t, err)
			assertFamilyAccess(t, rig, access)
			require.NoError(t, test.stage(t.Context(), redisClient, userID.String()))
			marker, err := redisClient.Get(t.Context(), middleware.TokenTheftBlockedKey(rig.user)).Result()
			require.NoError(t, err)
			if test.name == "legacy fallback" {
				require.Equal(t, tokenTheftLegacyStateValue, marker)
				legacyMember, err := redisClient.SIsMember(t.Context(), tokenTheftLegacyFallbackSetKey, rig.user).Result()
				require.NoError(t, err)
				require.True(t, legacyMember)
			} else {
				operationID, err := redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, rig.user).Result()
				require.NoError(t, err)
				require.Equal(t, operationID, marker)
				require.NotEmpty(t, operationID)
			}

			require.NoError(t, RecoverPendingTokenTheftRevocations(t.Context(), db, redisClient, logger.New("test")))

			var active, pending int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
			).Scan(&active))
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
			).Scan(&pending))
			assert.Zero(t, active)
			assert.Zero(t, pending)
			blocked, err := redisClient.Exists(t.Context(), middleware.TokenTheftBlockedKey(userID.String())).Result()
			require.NoError(t, err)
			assert.Zero(t, blocked)
			var terminal, revoked, theftOwner bool
			require.NoError(t, db.QueryRow(`SELECT f.terminal_at IS NOT NULL, r.revoked_at IS NOT NULL,
				o.reason='theft' AND o.scope='user' AND o.sealed_at IS NOT NULL
				AND EXISTS (SELECT 1 FROM login_family_revocation_targets t WHERE t.operation_id=o.operation_id AND t.family_id=f.family_id AND t.user_id=f.user_id)
				FROM login_family_sids s JOIN login_families f USING (family_id,user_id)
				JOIN refresh_tokens r ON r.id=s.sid AND r.user_id=s.user_id
				JOIN login_family_revocations o ON o.operation_id=f.auth_operation_id AND o.user_id=f.user_id
				WHERE s.sid=$1`, sid).Scan(&terminal, &revoked, &theftOwner))
			assert.True(t, terminal)
			assert.True(t, revoked)
			assert.True(t, theftOwner)
			require.NoError(t, RecoverPendingTokenTheftRevocations(t.Context(), db, redisClient, logger.New("test")))
			var generation int64
			require.NoError(t, db.QueryRow(`SELECT token_theft_generation FROM users WHERE id=$1`, userID).Scan(&generation))
			assert.EqualValues(t, 1, generation)
			fallback, err := redisClient.HExists(t.Context(), tokenTheftFallbackHashKey, rig.user).Result()
			require.NoError(t, err)
			assert.False(t, fallback)
			legacy, err := redisClient.SIsMember(t.Context(), tokenTheftLegacyFallbackSetKey, rig.user).Result()
			require.NoError(t, err)
			assert.False(t, legacy)
		})
	}
}

func TestRecoverPendingTokenTheftRevocations_WithoutRedisKeepsPendingRecord(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	require.NoError(t, EnqueueTokenTheftRevocation(t.Context(), db, userID.String()))

	err := RecoverPendingTokenTheftRevocations(t.Context(), db, nil, logger.New("test"))
	require.ErrorContains(t, err, "token theft block store unavailable")

	var pending int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}

func TestRecoverPendingTokenTheftRevocations_KeepsFallbackWhenPromotionFails(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	operationID := uuid.NewString()
	require.NoError(t, redisClient.HSet(t.Context(), tokenTheftFallbackHashKey, userID.String(), operationID).Err())
	require.NoError(t, redisClient.Set(t.Context(), middleware.TokenTheftBlockedKey(userID.String()), operationID, 0).Err())
	require.NoError(t, installTokenTheftRetryEnqueueFailure(db))
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS token_theft_retry_enqueue_failure()`)
	})

	err := RecoverPendingTokenTheftRevocations(t.Context(), db, redisClient, logger.New("test"))
	require.ErrorContains(t, err, "promote token theft fallback")

	storedOperationID, err := redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, operationID, storedOperationID)
}

func TestRetryTokenTheftRecoveryOnce_JoinsQueueAndProcessingFailures(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	h := &Handler{db: db}

	_, err := h.retryTokenTheftRecoveryOnce(t.Context(), userID.String(), true)
	require.Error(t, err)
	assert.ErrorContains(t, err, "persist token theft recovery state")
	assert.ErrorContains(t, err, "revoke refresh tokens after token theft retry")
}

// tokenTheftRetryDisconnector records the user-wide theft kill.
type tokenTheftRetryDisconnector struct{ users []uuid.UUID }

func (d *tokenTheftRetryDisconnector) DisconnectUser(user uuid.UUID) { d.users = append(d.users, user) }
func (*tokenTheftRetryDisconnector) DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func TestHandleTokenTheft_RetriesWhenBothPersistenceStoresFail(t *testing.T) {
	rig := newFamilyTestRig(t)
	db, redisClient := rig.db, rig.h.redis
	userID := uuid.MustParse(rig.user)
	access, _, sid, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
	require.NoError(t, err)
	assertFamilyAccess(t, rig, access)
	require.NoError(t, installTokenTheftRetryEnqueueFailure(db))
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS token_theft_retry_enqueue_failure()`)
	})

	const forceFailureKey = "token_theft_recovery_test:force_failure"
	require.NoError(t, redisClient.Set(context.Background(), forceFailureKey, "1", 0).Err())
	originalScript := persistTokenTheftRecoveryStateScript
	persistTokenTheftRecoveryStateScript = redis.NewScript(`
if redis.call("GET", "token_theft_recovery_test:force_failure") == "1" then
  return redis.error_reply("forced token theft recovery state failure")
end
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("SET", KEYS[2], ARGV[2])
return 1
`)
	t.Cleanup(func() { persistTokenTheftRecoveryStateScript = originalScript })

	h := rig.h
	hub := &tokenTheftRetryDisconnector{}
	h.hub = hub
	c, rec := newLoginContext()
	require.True(t, h.handleTokenTheft(c, models.RefreshToken{UserID: userID.String()}))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	// No family is terminal on this deferred path, so only the immediate
	// user-wide kill can stop the stolen session's open sockets.
	assert.Equal(t, []uuid.UUID{userID}, hub.users, "theft must disconnect every socket before responding")
	assert.NotContains(t, rec.Body.String(), "Session terminated")
	require.ErrorIs(t, middleware.CheckTokenTheftBlocked(t.Context(), redisClient, db, userID.String()), middleware.ErrTokenTheftRevocationPending)
	assert.True(t, middleware.LocalTokenTheftPending(userID.String()))
	_, err = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
	require.NoError(t, err)
	require.NoError(t, redisClient.Del(context.Background(), forceFailureKey).Err())

	assert.Eventually(t, func() bool {
		var active int
		err := db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE id = $1 AND revoked_at IS NULL`, sid).Scan(&active)
		return err == nil && active == 0
	}, 5*time.Second, 25*time.Millisecond)
	assert.Eventually(t, func() bool { return !middleware.LocalTokenTheftPending(userID.String()) }, 5*time.Second, 25*time.Millisecond)
	var terminal, revoked, theftOwner bool
	require.NoError(t, db.QueryRow(`SELECT f.terminal_at IS NOT NULL, r.revoked_at IS NOT NULL,
		o.reason='theft' AND o.scope='user' AND o.sealed_at IS NOT NULL
		AND EXISTS (SELECT 1 FROM login_family_revocation_targets t WHERE t.operation_id=o.operation_id AND t.family_id=f.family_id AND t.user_id=f.user_id)
		FROM login_family_sids s JOIN login_families f USING (family_id,user_id)
		JOIN refresh_tokens r ON r.id=s.sid AND r.user_id=s.user_id
		JOIN login_family_revocations o ON o.operation_id=f.auth_operation_id AND o.user_id=f.user_id
		WHERE s.sid=$1`, sid).Scan(&terminal, &revoked, &theftOwner))
	assert.True(t, terminal)
	assert.True(t, revoked)
	assert.True(t, theftOwner)
}

func installTokenTheftRetryEnqueueFailure(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE OR REPLACE FUNCTION token_theft_retry_enqueue_failure() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'token theft retry enqueue failure';
		END;
		$$ LANGUAGE plpgsql`); err != nil {
		return err
	}
	_, err := db.Exec(`
		CREATE TRIGGER token_theft_retry_enqueue_failure BEFORE INSERT ON token_theft_pending_revocations
		FOR EACH ROW EXECUTE FUNCTION token_theft_retry_enqueue_failure()`)
	return err
}
