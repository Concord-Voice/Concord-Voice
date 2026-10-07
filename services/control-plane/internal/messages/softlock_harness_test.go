package messages_test

// Shared harness for the delete-rate soft-lock's handler tests (#3455).
//
// The handlers are mounted on a bare gin engine WITHOUT the route limiter —
// the handler seam design spec §6 places AC1 and AC4 at — so a test can make
// sixteen deletes in a row. Postgres is the real test database.
//
// The soft-lock and budget Redis is a per-test miniredis, for the reason
// internal/api/router_klipy_ratelimit_test.go:30-38 records: not isolation,
// but its frozen clock. TTLs advance only via FastForward, which is what lets
// these tests cross the 30 s window without a sleep. The permission cache
// stays on the test Redis, as in production it shares nothing with the
// counter. Every client here, live or dead, uses MaxRetries: -1 and
// DialerRetries: 1 (C8): a dead call with the defaults costs ~1.7 s.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// softLockValidCode is the one code softLockVerifier accepts.
const softLockValidCode = "314159"

// softLockVerifier stands in for the MFA verifier. It records every purpose
// it is asked to verify for, so a test can pin the route's purpose. A code it
// accepts is spent the way the real TOTP verifier spends one, by advancing
// user_mfa_totp.last_used_step inside the caller's transaction, so a test can
// see whether that transaction committed.
type softLockVerifier struct {
	mu       sync.Mutex
	purposes []stepup.Purpose
}

func (v *softLockVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return nil, nil
}

func (v *softLockVerifier) VerifyCodeTx(ctx context.Context, tx *sql.Tx, userID string, purpose stepup.Purpose, code string) (bool, error) {
	v.mu.Lock()
	v.purposes = append(v.purposes, purpose)
	v.mu.Unlock()
	if tx == nil || code != softLockValidCode {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_mfa_totp SET last_used_step = COALESCE(last_used_step, 0) + 1
		WHERE user_id = $1`, userID); err != nil {
		return false, err
	}
	return true, nil
}

func (v *softLockVerifier) calls() []stepup.Purpose {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]stepup.Purpose(nil), v.purposes...)
}

type softLockHarness struct {
	ts       *testhelpers.TestServer
	mr       *miniredis.Miniredis
	handler  *messages.Handler
	router   *gin.Engine
	verifier *softLockVerifier
	logs     *testhelpers.SyncBuffer
}

// fastRedisClient builds a client with the C8 fast-failure options.
func fastRedisClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// newSoftLockHarness mounts the handler on a live miniredis.
func newSoftLockHarness(t *testing.T) *softLockHarness {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	mr := miniredis.RunT(t)
	return buildSoftLockHarness(t, ts, ts.DB, fastRedisClient(t, mr.Addr()), mr)
}

// buildSoftLockHarness mounts a handler whose soft-lock and budget use rdb and
// whose queries use db, which lets a test hand it a dead client or a pool with
// a lock_timeout. mr may be nil when rdb is dead.
func buildSoftLockHarness(t *testing.T, ts *testhelpers.TestServer, db *sql.DB, rdb *redis.Client, mr *miniredis.Miniredis) *softLockHarness {
	t.Helper()
	logs := &testhelpers.SyncBuffer{}
	log := logger.NewWithWriter(logs)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(ts.Redis), log)
	engine := purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 0)
	h := messages.NewHandler(db, log, ts.Hub, resolver, nil, engine)
	verifier := &softLockVerifier{}
	h.SetMFAVerifier(verifier)
	h.SetRedis(rdb)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", c.GetHeader("X-Test-User"))
		claims := jwt.MapClaims{}
		if epoch := c.GetHeader("X-Test-Epoch"); epoch != "" {
			claims["cred_epoch"] = epoch
		}
		if sid := c.GetHeader("X-Test-Session"); sid != "" {
			claims["sid"] = sid // the access token's session, which a step-up grace is keyed on (#3454)
		}
		if len(claims) > 0 {
			c.Set(middleware.JWTClaimsContextKey, claims)
		}
		c.Next()
	})
	router.DELETE("/messages/:id", h.DeleteMessage)
	router.DELETE("/channels/:id/messages", h.PurgeChannel)
	router.DELETE("/servers/:id/messages", h.PurgeServer)

	return &softLockHarness{ts: ts, mr: mr, handler: h, router: router, verifier: verifier, logs: logs}
}

// request is one call through the harness router. body is JSON-encoded when
// non-nil; epoch, when set, is the token's cred_epoch claim, and session its
// sid claim.
type request struct {
	method, path, userID, epoch, session string
	body                                 any
}

func (s *softLockHarness) do(t *testing.T, r request) *httptest.ResponseRecorder {
	t.Helper()
	var payload *bytes.Reader
	if r.body == nil {
		payload = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(r.body)
		require.NoError(t, err)
		payload = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(r.method, r.path, payload)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", r.userID)
	if r.epoch != "" {
		req.Header.Set("X-Test-Epoch", r.epoch)
	}
	if r.session != "" {
		req.Header.Set("X-Test-Session", r.session)
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func (s *softLockHarness) deleteMessage(t *testing.T, userID, messageID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return s.do(t, request{method: http.MethodDelete, path: "/messages/" + messageID, userID: userID, body: body})
}

// mintToken mints a password step-up token for userID and purpose, as the
// mint endpoint does after verifying the password (#3509). The harness sends
// no epoch claim, and a test user's epoch is NULL, so the token binds to NULL.
func (s *softLockHarness) mintToken(t *testing.T, userID string, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), s.ts.DB, userID, stepup.FactorPassword, purpose, "")
	require.Nil(t, e)
	return token
}

// softLockWorld is one server with a text channel, its owner, a member who
// authors messages (ManageOwnMessages only), and a moderator
// (ManageAllMessages through the admin role).
type softLockWorld struct {
	owner, author, moderator testhelpers.TestUser
	serverID, channelID      string
}

func (s *softLockHarness) world(t *testing.T, enforcing bool) softLockWorld {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	w := softLockWorld{
		owner:     s.ts.CreateTestUser(t, "sl_owner_"+suffix),
		author:    s.ts.CreateTestUser(t, "sl_author_"+suffix),
		moderator: s.ts.CreateTestUser(t, "sl_mod_"+suffix),
	}
	w.serverID = s.ts.CreateTestServer(t, w.owner.ID, "softlock-"+suffix)
	w.channelID = s.ts.CreateTestChannel(t, w.serverID, "general")
	s.ts.AddMemberToServer(t, w.serverID, w.author.ID, "member")
	s.ts.AddMemberToServer(t, w.serverID, w.moderator.ID, "admin")
	s.setEnforcing(t, w.serverID, enforcing)
	return w
}

func (s *softLockHarness) setEnforcing(t *testing.T, serverID string, on bool) {
	t.Helper()
	_, err := s.ts.DB.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = $2 WHERE id = $1`, serverID, on)
	require.NoError(t, err)
}

// enroll gives userID a confirmed TOTP factor, which puts "totp" in its P1
// subject. Enroll before the user's first request: the permission cache is
// keyed on MFA generation, which a raw insert does not bump.
func (s *softLockHarness) enroll(t *testing.T, userID string) {
	t.Helper()
	_, err := s.ts.DB.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, '\x00', '\x00', 1, TRUE, TRUE)`, userID)
	require.NoError(t, err)
}

// totpStepSpent reports whether a code userID verified was spent, i.e. the
// transaction softLockVerifier wrote its step in committed.
func (s *softLockHarness) totpStepSpent(t *testing.T, userID string) bool {
	t.Helper()
	var spent bool
	require.NoError(t, s.ts.DB.QueryRow(`SELECT last_used_step IS NOT NULL FROM user_mfa_totp WHERE user_id = $1`,
		userID).Scan(&spent))
	return spent
}

func (s *softLockHarness) setOwnRule(t *testing.T, userID string, on bool) {
	t.Helper()
	_, err := s.ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = EXCLUDED.require_auth_before_purge`, userID, on)
	require.NoError(t, err)
}

// seed inserts n messages by user into channelID in one statement and
// returns their ids, oldest first.
func (s *softLockHarness) seed(t *testing.T, channelID string, user testhelpers.TestUser, n int) []string {
	t.Helper()
	rows, err := s.ts.DB.Query(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		SELECT gen_random_uuid(), $1, $2, 'softlock', 1, FALSE, NOW() + g * INTERVAL '1 millisecond', NOW()
		FROM generate_series(1, $3) AS g
		RETURNING id`, channelID, user.ID, n)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, n)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func (s *softLockHarness) messageExists(t *testing.T, id string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, s.ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`, id).Scan(&exists))
	return exists
}

// burstKey, dayKey and budgetKey spell the Redis keys through the types that
// own them, so a key rename cannot leave these tests reading a dead key.
func burstKey(userID, serverID string) string {
	return stepup.NewDeleteSoftLock(nil).Key(uuid.MustParse(userID), stepup.ServerDeleteScope(uuid.MustParse(serverID)))
}

func dayKey(userID string) string {
	return stepup.NewDeleteSoftLock(nil).DayKey(uuid.MustParse(userID))
}

func budgetKey(userID string) string {
	return stepup.NewBudget(nil, stepup.MFASettingsBudgetPrefix).Key(userID)
}

// counter reads a miniredis counter, "" when the key is absent.
func (s *softLockHarness) counter(key string) string {
	if !s.mr.Exists(key) {
		return ""
	}
	v, _ := s.mr.Get(key)
	return v
}

// decode reads a JSON object body.
func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body
}

// requireSoftLockRefusal asserts a soft-lock 403: the refusal's own flag, the
// delete_rate_limited flag, and a positive Retry-After.
func requireSoftLockRefusal(t *testing.T, w *httptest.ResponseRecorder, flag string) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	body := decode(t, w)
	require.Equal(t, true, body["delete_rate_limited"], "every soft-lock refusal carries delete_rate_limited")
	if flag != "" {
		require.Equal(t, true, body[flag], "refusal must carry %s", flag)
	}
	retryAfter := w.Header().Get("Retry-After")
	require.NotEmpty(t, retryAfter, "every soft-lock refusal carries Retry-After")
	require.NotEqual(t, "0", retryAfter)
	return body
}

// softLockLogLines returns the soft-lock's own log lines.
func (s *softLockHarness) softLockLogLines() []string {
	var lines []string
	for _, line := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(line, "failure_class=delete_") {
			lines = append(lines, line)
		}
	}
	return lines
}

// requireNoPopulationField asserts C7 on every soft-lock log line: no field
// that could tell an operator who is in the population.
func (s *softLockHarness) requireNoPopulationField(t *testing.T, w softLockWorld) {
	t.Helper()
	lines := s.softLockLogLines()
	require.NotEmpty(t, lines, "the scenario must have produced soft-lock log lines to inspect")
	for _, line := range lines {
		for _, forbidden := range []string{
			w.author.ID, w.moderator.ID, w.owner.ID, w.serverID,
			"user_id=", "server_id=", "scope=", "enforcing", "mfa_methods", "methods=",
		} {
			require.NotContains(t, line, forbidden, "C7: a soft-lock log line carried %q", forbidden)
		}
	}
}
