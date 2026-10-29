package api_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pauseJSON = `{"error":"Recovery Circle recovery is temporarily paused. Use another recovery method.","error_code":"recovery_circle_paused"}`
const pauseCiphertext = "pause-regression-stored-encrypted-share"

type pauseFixture struct {
	ts                       *testhelpers.TestServer
	owner, contact, other    testhelpers.TestUser
	token, pending, complete string
}

func setupPauseFixture(t *testing.T) pauseFixture {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	f := pauseFixture{ts: ts, owner: ts.CreateTestUser(t, "pauseowner"), contact: ts.CreateTestUser(t, "pausecontact"), other: ts.CreateTestUser(t, "pauseother")}
	ts.CreateFriendship(t, f.owner.ID, f.contact.ID, "accepted")
	ts.CreateFriendship(t, f.owner.ID, f.other.ID, "accepted")
	hash := sha256.Sum256([]byte("999999"))
	record, err := json.Marshal(map[string]interface{}{"code_hash": hex.EncodeToString(hash[:]), "user_id": f.owner.ID, "attempts": 0})
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Set(t.Context(), "recovery_code:"+strings.ToLower(f.owner.Email), record, 10*time.Minute).Err())
	w := ts.DoRequest(http.MethodPost, "/api/v1/auth/recovery/verify-code", map[string]string{"email": f.owner.Email, "code": "999999"}, nil)
	require.Equal(t, http.StatusOK, w.Code, "recovery-token fixture must pass the real verification route: %s", w.Body.String())
	var body struct {
		Token string `json:"recovery_token"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Token)
	f.token = body.Token
	claims, err := mfa.ValidateChallengeToken(body.Token, ts.Config.JWTSecret, mfa.PurposeRecovery)
	require.NoError(t, err)
	var circle string
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO recovery_circles(user_id,threshold_k,total_shares_n) VALUES($1,2,2) RETURNING id`, f.owner.ID).Scan(&circle))
	_, err = ts.DB.Exec(`INSERT INTO recovery_circle_shares(circle_id,contact_id,share_index,encrypted_share,share_version) VALUES($1,$2,1,$4,1),($1,$3,2,$4,1)`, circle, f.contact.ID, f.other.ID, []byte(pauseCiphertext))
	require.NoError(t, err)
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO recovery_circle_requests(circle_id,user_id,recovery_token_jti,ephemeral_public_key,expires_at) VALUES($1,$2,$3,$4,NOW()+INTERVAL '1 hour') RETURNING id`, circle, f.owner.ID, claims.ID, []byte("fixture-ephemeral-public-key")).Scan(&f.pending))
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO recovery_circle_requests(circle_id,user_id,recovery_token_jti,ephemeral_public_key,expires_at,status,shares_received) VALUES($1,$2,$3,$4,NOW()+INTERVAL '1 hour','complete',2) RETURNING id`, circle, f.owner.ID, claims.ID, []byte("fixture-ephemeral-public-key")).Scan(&f.complete))
	_, err = ts.DB.Exec(`INSERT INTO recovery_circle_responses(request_id,contact_id,encrypted_share) VALUES($1,$2,$4),($1,$3,$4)`, f.complete, f.contact.ID, f.other.ID, []byte(pauseCiphertext))
	require.NoError(t, err)
	// Positive control: the preserved management route sees the same stored circle.
	w = ts.DoRequest(http.MethodGet, "/api/v1/mfa/recovery-circle", nil, testhelpers.AuthHeaders(f.owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "fixture must pass protected parent gates: %s", w.Body.String())
	require.Contains(t, w.Body.String(), f.contact.ID, "management must observe the seeded contact")
	return f
}

func snapshotPauseRows(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	queries := map[string]string{
		"circles":   `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb)::text FROM recovery_circles r`,
		"shares":    `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb)::text FROM recovery_circle_shares r`,
		"requests":  `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb)::text FROM recovery_circle_requests r`,
		"responses": `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb)::text FROM recovery_circle_responses r`,
	}
	result := make(map[string]string, len(queries))
	for name, query := range queries {
		var rows string
		require.NoError(t, db.QueryRow(query).Scan(&rows))
		result[name] = rows
	}
	return result
}

type pauseCall struct {
	name, method, path, pattern, token, body string
	userLimited                              bool
}

func pauseCalls(f pauseFixture) []pauseCall {
	return []pauseCall{
		{"contact shares", "GET", "/api/v1/mfa/recovery-circle/shares", "/api/v1/mfa/recovery-circle/shares", f.contact.AccessToken, "", true},
		{"create", "POST", "/api/v1/auth/recovery/social-request", "/api/v1/auth/recovery/social-request", "", `{"recovery_token":"` + f.token + `","ephemeral_public_key":"` + base64.StdEncoding.EncodeToString([]byte("valid-ephemeral-public-key")) + `"}`, false},
		{"poll pending", "GET", "/api/v1/auth/recovery/social-request/" + f.pending, "/api/v1/auth/recovery/social-request/:id", f.token, "", false},
		{"poll complete", "GET", "/api/v1/auth/recovery/social-request/" + f.complete, "/api/v1/auth/recovery/social-request/:id", f.token, "", false},
		{"list invitations", "GET", "/api/v1/mfa/recovery-requests/social", "/api/v1/mfa/recovery-requests/social", f.contact.AccessToken, "", true},
		{"respond", "POST", "/api/v1/mfa/recovery-requests/social/" + f.pending + "/respond", "/api/v1/mfa/recovery-requests/social/:id/respond", f.contact.AccessToken, `{"encrypted_share":"` + base64.StdEncoding.EncodeToString([]byte(pauseCiphertext)) + `"}`, true},
	}
}
func servePauseCall(f pauseFixture, c pauseCall) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	w := httptest.NewRecorder()
	f.ts.Router.ServeHTTP(w, req)
	return w
}
func assertPaused(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "admitted Recovery Circle recovery must be paused with 503")
	assert.JSONEq(t, pauseJSON, w.Body.String(), "pause must return exactly the stable error contract")
	assert.Empty(t, w.Header().Get("Retry-After"), "pause must not promise a reopening time")
	assert.NotContains(t, w.Body.String(), `"encrypted_share":`, "pause must never release ciphertext fields")
	assert.NotContains(t, w.Body.String(), base64.StdEncoding.EncodeToString([]byte(pauseCiphertext)), "pause must never release stored ciphertext")
}

// Regression for #3676: every public recovery operation must terminate before handlers and limiters.
func TestRecoveryCirclePauseRoutes(t *testing.T) {
	f := setupPauseFixture(t)
	before := snapshotPauseRows(t, f.ts.DB)
	for _, c := range pauseCalls(f) {
		t.Run(c.name, func(t *testing.T) { assertPaused(t, servePauseCall(f, c)) })
	}
	for _, c := range pauseCalls(f) {
		if c.name != "create" && c.name != "poll pending" && c.name != "respond" {
			continue
		}
		t.Run(c.name+" malformed", func(t *testing.T) {
			c.body = "{broken json"
			if c.name == "poll pending" {
				c.path = "/api/v1/auth/recovery/social-request/not-a-uuid"
				c.token = ""
			}
			if c.name == "respond" {
				c.path = "/api/v1/mfa/recovery-requests/social/not-a-uuid/respond"
			}
			assertPaused(t, servePauseCall(f, c))
		})
	}
	assert.Equal(t, before, snapshotPauseRows(t, f.ts.DB), "paused recovery must preserve every column of all recovery tables")
}

func TestRecoveryCirclePauseRoutesExhaustedLimiters(t *testing.T) {
	f := setupPauseFixture(t)
	before := snapshotPauseRows(t, f.ts.DB)
	for _, c := range pauseCalls(f) {
		t.Run(c.name, func(t *testing.T) {
			key := "ratelimit:ip:192.0.2.10:" + c.method + ":" + c.pattern
			if c.userLimited {
				key = "ratelimit:user:" + f.contact.ID + ":" + c.method + ":" + c.pattern
			}
			require.NoError(t, f.ts.Redis.Set(t.Context(), key, "99", 45*time.Second).Err())
			beforeTTL, err := f.ts.Redis.PTTL(t.Context(), key).Result()
			require.NoError(t, err)
			assertPaused(t, servePauseCall(f, c))
			value, err := f.ts.Redis.Get(t.Context(), key).Result()
			require.NoError(t, err)
			assert.Equal(t, "99", value, "pause must precede the route-specific limiter increment")
			afterTTL, err := f.ts.Redis.PTTL(t.Context(), key).Result()
			require.NoError(t, err)
			assert.Positive(t, afterTTL, "pause must retain the limiter expiration")
			assert.LessOrEqual(t, afterTTL, beforeTTL, "pause must not extend limiter expiration")
		})
	}
	assert.Equal(t, before, snapshotPauseRows(t, f.ts.DB), "exhausted-limiter paused calls must preserve all recovery data")
}

func TestRecoveryCirclePausePreservesParents(t *testing.T) {
	f := setupPauseFixture(t)
	unverified := f.ts.CreateTestUserUnverified(t, "pauseunverified")
	for _, c := range pauseCalls(f) {
		if !c.userLimited {
			continue
		}
		t.Run(c.name+" authentication", func(t *testing.T) {
			for _, token := range []string{"", "invalid-access-token"} {
				c.token = token
				w := servePauseCall(f, c)
				assert.Equal(t, http.StatusUnauthorized, w.Code, "protected pause must retain authentication refusal")
				assert.NotContains(t, w.Body.String(), "recovery_circle_paused", "auth refusal must precede pause")
			}
			c.token = unverified.AccessToken
			w := servePauseCall(f, c)
			assert.Equal(t, http.StatusForbidden, w.Code, "protected pause must retain verified-email refusal")
			assert.JSONEq(t, `{"code":"EMAIL_NOT_VERIFIED","error":"Email verification required"}`, w.Body.String(), "email gate must remain authoritative")
		})
	}
	t.Run("public ban", func(t *testing.T) {
		require.NoError(t, f.ts.Redis.Set(t.Context(), "auth_ban:ip:192.0.2.10", "1", time.Minute).Err())
		for _, c := range pauseCalls(f) {
			if c.userLimited {
				continue
			}
			w := servePauseCall(f, c)
			assert.Equal(t, http.StatusTooManyRequests, w.Code, "public AuthBanCheck must remain ahead of pause")
			assert.NotEmpty(t, w.Header().Get("Retry-After"), "parent ban retains retry guidance")
			assert.Contains(t, w.Body.String(), "Too many authentication failures")
		}
	})
}

func TestRecoveryCirclePausePreservesManagement(t *testing.T) {
	f := setupPauseFixture(t)
	route := "/api/v1/mfa/recovery-circle"
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		w := f.ts.DoRequest(method, route, nil, nil)
		require.Equal(t, http.StatusUnauthorized, w.Code, "owner management must retain authentication")
	}
	badPassword := map[string]interface{}{"password": "incorrect-test-password"}
	w := f.ts.DoRequest(http.MethodDelete, route, badPassword, testhelpers.AuthHeaders(f.owner.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code, "delete must retain owner step-up refusal: %s", w.Body.String())
	before := snapshotPauseRows(t, f.ts.DB)
	for _, c := range pauseCalls(f) {
		assertPaused(t, servePauseCall(f, c))
	}
	assert.Equal(t, before, snapshotPauseRows(t, f.ts.DB), "pause preserves saved owner configuration before deliberate management")
	encoded := base64.StdEncoding.EncodeToString([]byte("replacement-encrypted-share"))
	w = f.ts.DoRequest(http.MethodPut, route, map[string]interface{}{
		"password": f.owner.Password, "threshold_k": 2, "total_shares_n": 2,
		"shares": []map[string]interface{}{{"contact_id": f.contact.ID, "share_index": 1, "encrypted_share": encoded}, {"contact_id": f.other.ID, "share_index": 2, "encrypted_share": encoded}},
	}, testhelpers.AuthHeaders(f.owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "authorized owner must still save configuration: %s", w.Body.String())
	var version int
	require.NoError(t, f.ts.DB.QueryRow(`SELECT share_version FROM recovery_circles WHERE user_id=$1`, f.owner.ID).Scan(&version))
	assert.Equal(t, 2, version, "owner PUT must really persist the replacement")
	w = f.ts.DoRequest(http.MethodGet, route, nil, testhelpers.AuthHeaders(f.owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), f.contact.ID)
	w = f.ts.DoRequest(http.MethodDelete, route, map[string]string{"password": f.owner.Password}, testhelpers.AuthHeaders(f.owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "authorized owner must still delete configuration: %s", w.Body.String())
	var count int
	require.NoError(t, f.ts.DB.QueryRow(`SELECT count(*) FROM recovery_circles WHERE user_id=$1`, f.owner.ID).Scan(&count))
	assert.Zero(t, count, "owner DELETE must really remove the circle")
}

func TestRecoveryCirclePausePreservesParentsClientVersion(t *testing.T) {
	t.Setenv("TEST_CLIENT_MIN_VERSION", "1.2.3")
	ts := testhelpers.SetupTestServer(t)
	contact := ts.CreateTestUser(t, "pauseversion")
	for _, path := range []string{"/api/v1/mfa/recovery-circle/shares", "/api/v1/mfa/recovery-requests/social", "/api/v1/mfa/recovery-requests/social/invalid/respond"} {
		t.Run(path, func(t *testing.T) {
			method := http.MethodGet
			if strings.HasSuffix(path, "/respond") {
				method = http.MethodPost
			}
			for _, proof := range []string{"", "forged-service-proof"} {
				req := httptest.NewRequest(method, path, strings.NewReader("{malformed"))
				req.Header = testhelpers.AuthHeaders(contact.AccessToken)
				req.Header.Set("X-Concord-Service-Proof", proof)
				req.Header.Set("X-Concord-Service-Timestamp", "0")
				w := httptest.NewRecorder()
				ts.Router.ServeHTTP(w, req)
				assert.Equal(t, http.StatusForbidden, w.Code, "client-version gate must refuse before pause, including forged service hops")
				assert.JSONEq(t, `{"error":"Client version too old","code":"CLIENT_VERSION_TOO_OLD","updateAvailable":true,"requiredMinVersion":"1.2.3","downloadHelpUrl":"https://concordvoice.com/download"}`, w.Body.String(), "client-version refusal contract must remain intact")
			}
			req := httptest.NewRequest(method, path, nil)
			req.Header = testhelpers.AuthHeaders(contact.AccessToken)
			req.Header.Set("X-Concord-Client-Version", "1.2.3")
			w := httptest.NewRecorder()
			ts.Router.ServeHTTP(w, req)
			assertPaused(t, w)
		})
	}
}
