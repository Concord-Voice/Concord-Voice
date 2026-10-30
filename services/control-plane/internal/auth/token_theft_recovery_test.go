package auth_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartTokenTheftRecoveryWorker_CompletesStartupRecoveryBeforeReturning(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-recovery")
	minter := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	access, _, sid, err := minter.IssueAccessAndRefresh(t.Context(), user.ID)
	require.NoError(t, err)
	require.NotEmpty(t, access)

	require.NoError(t, auth.EnqueueTokenTheftRevocation(context.Background(), ts.DB, user.ID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, auth.StartTokenTheftRecoveryWorker(ctx, ts.DB, ts.Redis, logger.New("test"), time.Hour))

	var pending, active int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&active))
	assert.Zero(t, pending)
	assert.Zero(t, active)

	var revokedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE id = $1 AND user_id=$2`, sid, user.ID,
	).Scan(&revokedAt))
	require.True(t, revokedAt.Valid)
	var terminal, theftOwner bool
	var owner string
	require.NoError(t, ts.DB.QueryRow(`SELECT f.terminal_at IS NOT NULL, f.auth_operation_id,
		o.reason='theft' AND o.scope='user' AND o.sealed_at IS NOT NULL
		AND EXISTS (SELECT 1 FROM login_family_revocation_targets t WHERE t.operation_id=o.operation_id AND t.family_id=f.family_id AND t.user_id=f.user_id)
		FROM login_family_sids s JOIN login_families f USING (family_id,user_id)
		JOIN login_family_revocations o ON o.operation_id=f.auth_operation_id AND o.user_id=f.user_id
		WHERE s.sid=$1 AND s.user_id=$2`, sid, user.ID).Scan(&terminal, &owner, &theftOwner))
	assert.True(t, terminal)
	assert.True(t, theftOwner)
	assert.NotEmpty(t, owner)
	denied := ts.DoRequest(http.MethodGet, pathUsersMe, nil, testhelpers.AuthHeaders(access))
	require.Equal(t, http.StatusUnauthorized, denied.Code, denied.Body.String())
}

func TestProcessTokenTheftRevocation_SkipsCompletedRecord(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-no-stale-replay")
	ctx := context.Background()
	_, err := ts.DB.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, operation_id, completed_at) VALUES ($1, $2, NOW())`, user.ID, uuid.New(),
	)
	require.NoError(t, err)

	_, err = ts.DB.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), user.ID, auth.HashRefreshToken("post-recovery-session"),
	)
	require.NoError(t, err)

	// A retry that only needs Redis cleanup must not revoke this legitimately
	// minted post-recovery session.
	require.NoError(t, auth.ProcessTokenTheftRevocation(ctx, ts.DB, ts.Redis, user.ID))
	var active int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&active))
	assert.Equal(t, 1, active)
	var pending int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	assert.Zero(t, pending)
}

func TestProcessTokenTheftRevocation_DrainsLegacyCompletedRecord(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-legacy-recovery")
	_, err := ts.DB.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, completed_at) VALUES ($1, NOW())`, user.ID,
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Set(context.Background(), middleware.TokenTheftBlockedKey(user.ID), "1", 0).Err())

	require.NoError(t, auth.ProcessTokenTheftRevocation(context.Background(), ts.DB, ts.Redis, user.ID))
	blocked, err := ts.Redis.Exists(context.Background(), middleware.TokenTheftBlockedKey(user.ID)).Result()
	require.NoError(t, err)
	assert.Zero(t, blocked)
	var pending int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	assert.Zero(t, pending)
}

func TestEnqueueTokenTheftRevocation_ResetsCompletedOperation(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-new-operation")
	previousOperationID := uuid.New()
	_, err := ts.DB.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, operation_id, completed_at) VALUES ($1, $2, NOW())`, user.ID, previousOperationID,
	)
	require.NoError(t, err)

	require.NoError(t, auth.EnqueueTokenTheftRevocation(context.Background(), ts.DB, user.ID))
	var operationID uuid.UUID
	var completedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT operation_id, completed_at FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&operationID, &completedAt))
	assert.NotEqual(t, previousOperationID, operationID)
	assert.False(t, completedAt.Valid)
}

func TestStartTokenTheftRecoveryWorker_FailsStartupWhenRecoveryIsIncomplete(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-recovery-failure")
	_, err := ts.DB.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), user.ID, auth.HashRefreshToken("failed-startup-recovery-session"),
	)
	require.NoError(t, err)
	require.NoError(t, auth.EnqueueTokenTheftRevocation(context.Background(), ts.DB, user.ID))

	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, deadRedis.Close()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = auth.StartTokenTheftRecoveryWorker(ctx, ts.DB, deadRedis, logger.New("test"), time.Hour)
	require.Error(t, err)
	assert.ErrorContains(t, err, "startup token theft revocation recovery incomplete")

	var pending int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}

func TestRecoverPendingTokenTheftRevocations_RetiresStateForDeletedUser(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-deleted-user")
	ctx := context.Background()
	operationID := uuid.NewString()
	const fallbackKey = "token_theft_pending_revocations:fallback:v2"
	const legacyFallbackKey = "token_theft_pending_revocations:fallback"

	require.NoError(t, ts.Redis.HSet(ctx, fallbackKey, user.ID, operationID).Err())
	require.NoError(t, ts.Redis.SAdd(ctx, legacyFallbackKey, user.ID).Err())
	require.NoError(t, ts.Redis.Set(ctx, middleware.TokenTheftBlockedKey(user.ID), operationID, 0).Err())
	_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	require.NoError(t, err)

	require.NoError(t, auth.RecoverPendingTokenTheftRevocations(ctx, ts.DB, ts.Redis, logger.New("test")))
	_, err = ts.Redis.HGet(ctx, fallbackKey, user.ID).Result()
	assert.ErrorIs(t, err, redis.Nil)
	legacyPending, err := ts.Redis.SIsMember(ctx, legacyFallbackKey, user.ID).Result()
	require.NoError(t, err)
	assert.False(t, legacyPending)
	blocked, err := ts.Redis.Exists(ctx, middleware.TokenTheftBlockedKey(user.ID)).Result()
	require.NoError(t, err)
	assert.Zero(t, blocked)
}

func TestRecoverPendingTokenTheftRevocations_RetiresSupersededLegacyFallback(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-superseded-legacy")
	ctx := context.Background()
	const legacyFallbackKey = "token_theft_pending_revocations:fallback"
	minter := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	access, _, sid, err := minter.IssueAccessAndRefresh(t.Context(), user.ID)
	require.NoError(t, err)

	require.NoError(t, auth.EnqueueTokenTheftRevocation(ctx, ts.DB, user.ID))
	require.NoError(t, ts.Redis.SAdd(ctx, legacyFallbackKey, user.ID).Err())

	require.NoError(t, auth.RecoverPendingTokenTheftRevocations(ctx, ts.DB, ts.Redis, logger.New("test")))
	var generation int64
	require.NoError(t, ts.DB.QueryRow(`SELECT token_theft_generation FROM users WHERE id = $1`, user.ID).Scan(&generation))
	assert.EqualValues(t, 1, generation, "the committed sweep advances the WebSocket admission fence")
	legacyPending, err := ts.Redis.SIsMember(ctx, legacyFallbackKey, user.ID).Result()
	require.NoError(t, err)
	assert.False(t, legacyPending)

	var terminal, theftOwner bool
	var owner string
	require.NoError(t, ts.DB.QueryRow(`SELECT f.terminal_at IS NOT NULL, f.auth_operation_id,
		o.reason='theft' AND o.scope='user' AND o.sealed_at IS NOT NULL
		AND EXISTS (SELECT 1 FROM login_family_revocation_targets t WHERE t.operation_id=o.operation_id AND t.family_id=f.family_id AND t.user_id=f.user_id)
		FROM login_family_sids s JOIN login_families f USING (family_id,user_id)
		JOIN login_family_revocations o ON o.operation_id=f.auth_operation_id AND o.user_id=f.user_id
		WHERE s.sid=$1 AND s.user_id=$2`, sid, user.ID).Scan(&terminal, &owner, &theftOwner))
	assert.True(t, terminal)
	assert.True(t, theftOwner)
	assert.NotEmpty(t, owner)
	denied := ts.DoRequest(http.MethodGet, pathUsersMe, nil, testhelpers.AuthHeaders(access))
	require.Equal(t, http.StatusUnauthorized, denied.Code, denied.Body.String())

	var oldRevoked sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1 AND user_id=$2`, sid, user.ID).Scan(&oldRevoked))
	require.True(t, oldRevoked.Valid)
	freshAccess, freshRefresh, freshSID, err := minter.IssueAccessAndRefresh(t.Context(), user.ID)
	require.NoError(t, err)
	require.NotEqual(t, sid, freshSID)
	var oldFamily, freshFamily string
	require.NoError(t, ts.DB.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, sid).Scan(&oldFamily))
	require.NoError(t, ts.DB.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, freshSID).Scan(&freshFamily))
	require.NotEqual(t, oldFamily, freshFamily)
	live := ts.DoRequest(http.MethodGet, pathUsersMe, nil, testhelpers.AuthHeaders(freshAccess))
	require.Equal(t, http.StatusOK, live.Code, live.Body.String())

	require.NoError(t, auth.RecoverPendingTokenTheftRevocations(ctx, ts.DB, ts.Redis, logger.New("test")))
	require.NoError(t, ts.DB.QueryRow(`SELECT token_theft_generation FROM users WHERE id = $1`, user.ID).Scan(&generation))
	assert.EqualValues(t, 1, generation, "cleanup-only recovery must not advance the fence again")
	var active int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&active))
	assert.Equal(t, 1, active)
	var freshLive, freshUnrevoked bool
	require.NoError(t, ts.DB.QueryRow(`SELECT f.terminal_at IS NULL AND f.auth_operation_id IS NULL AND f.epoch_generation IS NULL, r.revoked_at IS NULL
		FROM login_family_sids s JOIN login_families f USING (family_id,user_id)
		JOIN refresh_tokens r ON r.id=s.sid AND r.user_id=s.user_id WHERE s.sid=$1 AND f.family_id=$2`, freshSID, freshFamily).Scan(&freshLive, &freshUnrevoked))
	assert.True(t, freshLive)
	assert.True(t, freshUnrevoked)
	var replayOwner string
	require.NoError(t, ts.DB.QueryRow(`SELECT auth_operation_id FROM login_families WHERE family_id=$1 AND terminal_at IS NOT NULL`, oldFamily).Scan(&replayOwner))
	assert.Equal(t, owner, replayOwner)
	var pending int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM token_theft_pending_revocations WHERE user_id=$1`, user.ID).Scan(&pending))
	assert.Zero(t, pending)
	legacyPending, err = ts.Redis.SIsMember(ctx, legacyFallbackKey, user.ID).Result()
	require.NoError(t, err)
	assert.False(t, legacyPending)
	blocked, err := ts.Redis.Exists(ctx, middleware.TokenTheftBlockedKey(user.ID)).Result()
	require.NoError(t, err)
	assert.Zero(t, blocked)
	live = ts.DoRequest(http.MethodGet, pathUsersMe, nil, testhelpers.AuthHeaders(freshAccess))
	require.Equal(t, http.StatusOK, live.Code, live.Body.String())
	rotated := ts.DoRequest(http.MethodPost, pathRefresh, nil, http.Header{"X-Refresh-Token": {freshRefresh}})
	require.Equal(t, http.StatusOK, rotated.Code, rotated.Body.String())
}

func TestRecoverPendingTokenTheftRevocations_FsyncsLegacyRetirementForCompletedRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-completed-legacy")
	ctx := context.Background()
	const legacyFallbackKey = "token_theft_pending_revocations:fallback"
	_, err := ts.DB.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, operation_id, completed_at) VALUES ($1, $2, NOW())`,
		user.ID, uuid.New(),
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.SAdd(ctx, legacyFallbackKey, user.ID).Err())

	require.NoError(t, auth.RecoverPendingTokenTheftRevocations(ctx, ts.DB, ts.Redis, logger.New("test")))
	member, err := ts.Redis.SIsMember(ctx, legacyFallbackKey, user.ID).Result()
	require.NoError(t, err)
	assert.False(t, member)
	var pending int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID).Scan(&pending))
	assert.Zero(t, pending, "the SQL row retires only after Redis cleanup receives its AOF receipt")

	_, err = ts.DB.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), user.ID, auth.HashRefreshToken("post-completion-session"),
	)
	require.NoError(t, err)
	require.NoError(t, auth.RecoverPendingTokenTheftRevocations(ctx, ts.DB, ts.Redis, logger.New("test")))
	var active int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID).Scan(&active))
	assert.Equal(t, 1, active, "a retired legacy fallback cannot repeat the sweep")
}
