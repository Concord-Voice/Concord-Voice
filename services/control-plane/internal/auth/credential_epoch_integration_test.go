package auth_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2201 acceptance criteria: an access token minted before each destructive
// credential flow returns 401 afterward and cannot mutate representative rows;
// the authenticated flows hand the caller a continuation pair minted under the
// new epoch; the recovery flows terminate every session.

const (
	pathUsersMeAuth  = "/api/v1/users/me"
	pathPrefsMe      = "/api/v1/users/me/preferences"
	pathMePassword   = "/api/v1/users/me/password" //nolint:gosec // URL path, not a credential
	pathMeKeys       = "/api/v1/users/me/keys"
	credEpochNewPass = "AnotherSecurePass456!" //nolint:gosec // test fixture
)

// assertTokenDead asserts the pre-flow access token is rejected on a protected
// read AND on a representative encrypted-state mutation (AC-1).
func assertTokenDead(t *testing.T, ts *testhelpers.TestServer, token string) {
	t.Helper()
	w := ts.DoRequest("GET", pathUsersMeAuth, nil, testhelpers.AuthHeaders(token))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "old token must 401 on protected routes")

	w = ts.DoRequest("PUT", pathPrefsMe, map[string]interface{}{
		"encrypted_data": "c3RhbGUtY2lwaGVydGV4dA==",
	}, testhelpers.AuthHeaders(token))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "old token must not mutate encrypted state")
}

// assertEpochRotated asserts the durable epoch exists and the Redis marker is
// the active form of it.
func assertEpochRotated(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	var epoch sql.NullString
	require.NoError(t, ts.DB.QueryRow(`SELECT credential_epoch FROM users WHERE id = $1`, userID).Scan(&epoch))
	require.True(t, epoch.Valid && epoch.String != "", "users.credential_epoch must be set after a destructive flow")

	cached, err := ts.Redis.Get(context.Background(), credepoch.Key(userID)).Result()
	require.NoError(t, err, "active epoch marker must be published after definite commit")
	assert.Equal(t, "active:"+epoch.String, cached)
	return epoch.String
}

func TestCredEpochFence_ChangePassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "epochchgpw")
	oldToken := user.AccessToken

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", pathMePassword, map[string]interface{}{
		"current_password":    user.Password,
		"new_password":        credEpochNewPass,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  "argon2id",
	}, testhelpers.AuthHeaders(oldToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	continuation, ok := body["access_token"].(string)
	require.True(t, ok, "ChangePassword must return a continuation access_token")
	require.NotEmpty(t, body["refresh_token"])
	require.NotEmpty(t, body["session_id"])

	newEpoch := assertEpochRotated(t, ts, user.ID)
	assertTokenDead(t, ts, oldToken)

	// Continuation pair works and carries the NEW epoch claim.
	w = ts.DoRequest("GET", pathUsersMeAuth, nil, testhelpers.AuthHeaders(continuation))
	assert.Equal(t, http.StatusOK, w.Code, "continuation token must be admitted")
	claims, err := auth.ValidateAccessToken(continuation, testhelpers.TestJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, newEpoch, claims.CredentialEpoch, "continuation must be minted under the new epoch")

	// Every refresh session except the continuation session is revoked.
	var liveOthers int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL AND id != $2`,
		user.ID, body["session_id"]).Scan(&liveOthers))
	assert.Zero(t, liveOthers, "all prior refresh sessions must be revoked")
}

func TestCredEpochFence_ReplaceMyKeys(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "epochrepkeys")
	oldToken := user.AccessToken

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("PUT", pathMeKeys, map[string]interface{}{
		"current_password":      user.Password,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, testhelpers.AuthHeaders(oldToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	continuation, ok := body["access_token"].(string)
	require.True(t, ok, "ReplaceMyKeys must return a continuation access_token")

	assertEpochRotated(t, ts, user.ID)
	assertTokenDead(t, ts, oldToken)

	w = ts.DoRequest("GET", pathUsersMeAuth, nil, testhelpers.AuthHeaders(continuation))
	assert.Equal(t, http.StatusOK, w.Code, "continuation token must be admitted")
}

func recoveryTokenFor(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) string {
	t.Helper()
	seedRecoveryCode(t, ts, user.Email, "222333", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "222333",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	token, ok := body["recovery_token"].(string)
	require.True(t, ok)
	return token
}

func TestCredEpochFence_RecoveryResetPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "epochrecpw")
	oldToken := user.AccessToken

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      recoveryTokenFor(t, ts, user),
		"new_password":        credEpochNewPass,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  "argon2id",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertEpochRotated(t, ts, user.ID)
	assertTokenDead(t, ts, oldToken)

	// Recovery terminates EVERY session (no continuation).
	var live int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID).Scan(&live))
	assert.Zero(t, live, "recovery must revoke every refresh session")

	// Media-plane parity (AC-6): the voice join-authorize hop carries the user
	// bearer through AuthRequired, so a superseded bearer is rejected there too.
	w = ts.DoRequest("POST", "/api/v1/channels/9e7bcd7e-14a5-4bff-8f3e-0f4e5d3c2b1a/voice/join", nil, testhelpers.AuthHeaders(oldToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "voice join must reject a superseded bearer")
}

func TestCredEpochFence_RecoveryResetAccount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "epochrecacct")
	oldToken := user.AccessToken

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", pathRecoveryResetAcct, map[string]interface{}{
		"recovery_token":        recoveryTokenFor(t, ts, user),
		"new_password":          credEpochNewPass,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertEpochRotated(t, ts, user.ID)
	assertTokenDead(t, ts, oldToken)
}

// AC-2 (transactional fence, lock-wait path): a sensitive-write GuardTx that
// starts before a reset's user-row lock releases must observe the NEW epoch
// once the reset commits, and abort.
func TestCredEpochFence_GuardTxWaitsOnResetLock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "epochinterleave")
	ctx := context.Background()

	// "Reset" transaction: lock the user row and rotate the epoch, holding the
	// lock while the guarded write tries to read it.
	resetTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = resetTx.ExecContext(ctx, `SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`, user.ID)
	require.NoError(t, err)
	_, err = resetTx.ExecContext(ctx, `UPDATE users SET credential_epoch = 'interleavednew' WHERE id = $1`, user.ID)
	require.NoError(t, err)

	guardResult := make(chan error, 1)
	go func() {
		writeTx, gerr := ts.DB.BeginTx(ctx, nil)
		if gerr != nil {
			guardResult <- gerr
			return
		}
		defer func() { _ = writeTx.Rollback() }()
		// The old-token request carries no epoch claim (user pre-rotation).
		guardResult <- credepoch.GuardTx(ctx, writeTx, user.ID, "")
	}()

	// The guard must be blocked on the reset's row lock, not completed.
	select {
	case early := <-guardResult:
		t.Fatalf("GuardTx completed while the reset held the row lock: %v", early)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, resetTx.Commit())

	select {
	case res := <-guardResult:
		assert.ErrorIs(t, res, credepoch.ErrEpochMismatch,
			"a write admitted before the reset must abort once the reset commits")
	case <-time.After(5 * time.Second):
		t.Fatal("GuardTx did not complete after the reset committed")
	}
}

// AC-5: with Redis unreachable (transport error, not a miss), verification
// falls back to the durable DB epoch — admitting only the matching claim —
// and fails closed only when the DB cannot answer either.
func TestCredEpochFence_RedisDownFallsBackToDB(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "epochredisdown")
	_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = 'dbtruth1' WHERE id = $1`, user.ID)
	require.NoError(t, err)

	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // nothing listens
	t.Cleanup(func() { _ = deadRedis.Close() })
	fence := credepoch.New(ts.DB, deadRedis, logger.NewWithWriter(io.Discard))

	assert.NoError(t, fence.Check(context.Background(), user.ID, "dbtruth1"),
		"DB fallback must admit the matching epoch while Redis is down")
	assert.ErrorIs(t, fence.Check(context.Background(), user.ID, "stale"), credepoch.ErrEpochMismatch,
		"DB fallback must reject a superseded epoch while Redis is down")
}
