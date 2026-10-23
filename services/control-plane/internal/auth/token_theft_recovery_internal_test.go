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
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	require.NoError(t, EnqueueTokenTheftRevocation(t.Context(), db, userID.String()))
	h := &Handler{db: db, redis: redisClient, log: logger.New("test")}
	sourceID, revokedID, successorID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := db.Exec(`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`, sourceID, userID, HashRefreshToken("pending-theft-source"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, revoked_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour', NOW())`, revokedID, userID, HashRefreshToken("pending-theft-revoked"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, predecessor_id)
		VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour', $4)`, successorID, userID, HashRefreshToken("pending-theft-successor"), revokedID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.rotateAndRespond(c, models.RefreshToken{
		ID: sourceID, UserID: userID.String(), ExpiresAt: time.Now().Add(time.Hour),
	}, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	c, rec = newLoginContext()
	h.handleGracePeriodRefresh(c, userID.String(), revokedID, time.Now())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	var active, total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&active))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1`, userID).Scan(&total))
	assert.Equal(t, 2, active, "neither mint path may revoke a source while theft is pending")
	assert.Equal(t, 3, total, "neither mint path may create a successor after theft enqueue")
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
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	_, err := db.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id) VALUES ($1)`, userID,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), userID, HashRefreshToken("legacy-pending-token"),
	)
	require.NoError(t, err)

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
			db, cleanup := dbtest.SetupTestDB(t)
			t.Cleanup(cleanup)
			redisClient := setupAuthAttemptRedis(t)
			userID := dbtest.CreateUser(t, db)
			_, err := db.Exec(
				`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
				uuid.New(), userID, HashRefreshToken("fallback-token-"+test.name),
			)
			require.NoError(t, err)
			require.NoError(t, test.stage(t.Context(), redisClient, userID.String()))

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

type tokenTheftRetryDisconnector struct{}

func (tokenTheftRetryDisconnector) DisconnectUser(uuid.UUID) {}

func TestHandleTokenTheft_RetriesWhenBothPersistenceStoresFail(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	_, err := db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), userID, HashRefreshToken("token-theft-retry-token"),
	)
	require.NoError(t, err)
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

	h := &Handler{db: db, redis: redisClient, log: logger.New("test"), hub: tokenTheftRetryDisconnector{}}
	c, rec := newLoginContext()
	require.True(t, h.handleTokenTheft(c, models.RefreshToken{UserID: userID.String()}))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotContains(t, rec.Body.String(), "Session terminated")
	require.ErrorIs(t, middleware.CheckTokenTheftBlocked(t.Context(), redisClient, db, userID.String()), middleware.ErrTokenTheftRevocationPending)
	assert.True(t, middleware.LocalTokenTheftPending(userID.String()))
	_, err = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
	require.NoError(t, err)
	require.NoError(t, redisClient.Del(context.Background(), forceFailureKey).Err())

	assert.Eventually(t, func() bool {
		var active int
		err := db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&active)
		return err == nil && active == 0
	}, 5*time.Second, 25*time.Millisecond)
	assert.Eventually(t, func() bool { return !middleware.LocalTokenTheftPending(userID.String()) }, 5*time.Second, 25*time.Millisecond)
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
