package oauth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

const appleSessionPath = "/api/v1/auth/sso/%s/session"

// appleJWKSRig is a JWKS-only fake Apple. /session consumes a CLIENT-minted
// id_token, so unlike the retired Exchange tests there is no /auth/token to
// fake — tests sign id_tokens directly with the rig's RSA key. rotate()
// swaps key + kid to exercise the fetchKey cache-miss refetch path.
type appleJWKSRig struct {
	Server *httptest.Server

	mu   sync.Mutex
	priv *rsa.PrivateKey
	kid  string
	gen  int
}

func newAppleJWKSRig(t *testing.T) *appleJWKSRig {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rig := &appleJWKSRig{priv: priv, kid: "apple-kid-1", gen: 1}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/keys", func(w http.ResponseWriter, _ *http.Request) {
		rig.mu.Lock()
		n := base64.RawURLEncoding.EncodeToString(rig.priv.N.Bytes())
		kid := rig.kid
		rig.mu.Unlock()
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": n, "e": e,
			}},
		})
	})
	rig.Server = httptest.NewServer(mux)
	t.Cleanup(rig.Server.Close)
	return rig
}

// rotate generates a fresh key under a new kid — the previous kid disappears
// from the published JWKS, exactly how Apple rotates.
func (r *appleJWKSRig) rotate(t *testing.T) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.priv = priv
	r.kid = fmt.Sprintf("apple-kid-%d", r.gen)
}

// sign mints an RS256 id_token under the rig's currently-published key.
func (r *appleJWKSRig) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	r.mu.Lock()
	priv, kid := r.priv, r.kid
	r.mu.Unlock()
	return signAppleToken(t, jwt.SigningMethodRS256, priv, kid, claims)
}

// signWith mints a token with an arbitrary method/key/kid — the negative
// matrix (HS256 confusion, unknown kid, foreign key).
func signAppleToken(t *testing.T, method jwt.SigningMethod, key interface{}, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

// appleSessionClaims is the happy-path id_token claim set (replaces
// makeAppleClaims from the retired Exchange test block); overrides mutate.
func appleSessionClaims(now time.Time, nonce string, overrides map[string]any) jwt.MapClaims {
	claims := jwt.MapClaims{
		"iss":              "https://appleid.apple.com",
		"aud":              testAppleClientID,
		"sub":              "001234.aabbccddeeff.1234",
		"email":            "jane@example.com",
		"email_verified":   true,
		"is_private_email": false,
		"nonce":            nonce,
		"nonce_supported":  true,
		"iat":              now.Unix(),
		"exp":              now.Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		claims[k] = v
	}
	return claims
}

type appleSessionRig struct {
	Engine  *gin.Engine
	Redis   redis.Cmdable
	DB      *sql.DB
	Adapter *fakeAuthAdapter
	JWKS    *appleJWKSRig
}

func newAppleSessionRig(t *testing.T, withRateLimit bool) *appleSessionRig {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(redisCleanup)

	jwks := newAppleJWKSRig(t)
	apple, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:     testAppleClientID,
		TeamID:       testAppleTeamID,
		KeyID:        testAppleKeyID,
		PrivateKey:   generateP256PEM(t),
		RedirectURI:  "http://127.0.0.1:0/oauth/callback",
		JWKSEndpoint: jwks.Server.URL + "/auth/keys",
		Issuer:       "https://appleid.apple.com",
	})
	require.NoError(t, err)
	registry := oauth.NewRegistry()
	registry.Register(apple)

	adapter := &fakeAuthAdapter{}
	h := oauth.NewHandler(oauth.HandlerDeps{
		Registry:    registry,
		Redis:       rdb,
		DB:          db,
		AuthHandler: adapter,
	})
	r := gin.New()
	handlers := []gin.HandlerFunc{h.ProviderSession}
	if withRateLimit {
		handlers = []gin.HandlerFunc{
			middleware.RateLimitByIP(rdb, 10, 15*time.Minute),
			h.ProviderSession,
		}
	}
	r.POST(fmt.Sprintf(appleSessionPath, ":provider"), handlers...)
	// Callback route is NOT registered — it was removed in #975.
	return &appleSessionRig{Engine: r, Redis: rdb, DB: db, Adapter: adapter, JWKS: jwks}
}

// seedAppleSessionState writes an sso_state record the way Initiate does
// post-#974 for apple: state embedded (#972), client-owned PKCE so
// code_verifier is EMPTY. Returns the state value.
func seedAppleSessionState(t *testing.T, rdb redis.Cmdable, provider, nonce string) string {
	t.Helper()
	state := fmt.Sprintf("session-state-%s-%d", provider, time.Now().UnixNano())
	payload, err := json.Marshal(map[string]any{
		"provider":      provider,
		"state":         state,
		"nonce":         nonce,
		"code_verifier": "",
		"redirect_uri":  "http://127.0.0.1:51620/oauth/callback",
		"created_at":    time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "sso_state:"+state, payload, 10*time.Minute).Err())
	return state
}

func postAppleSession(rig *appleSessionRig, provider string, body map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf(appleSessionPath, provider), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.9:50000"
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)
	return w
}

// ───────────────────────── happy paths ─────────────────────────

func TestAppleSession_NewUser_RegistrationRequired(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	nonce := "nonce-newuser"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce, nil))

	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["sso_registration_required"])
	assert.Equal(t, "jane@example.com", resp["email"])
	assert.NotEmpty(t, resp["sso_token"])

	// One-shot: the state record is consumed.
	err := rig.Redis.Get(context.Background(), "sso_state:"+state).Err()
	assert.ErrorIs(t, err, redis.Nil, "state must be deleted after consumption")
}

func TestAppleSession_FirstAuthUserData_ThreadsDisplayName(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	nonce := "nonce-userdata"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "firstauth@example.com", "sub": "sub-firstauth"}))

	w := postAppleSession(rig, "apple", map[string]any{
		"id_token":        idToken,
		"state":           state,
		"apple_user_data": `{"name":{"firstName":"Jane","lastName":"Doe"},"email":"firstauth@example.com"}`,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Jane Doe", resp["name"],
		"first-auth user JSON must thread through parseAppleUserData to the response (→ users.display_name)")
}

func TestAppleSession_OversizeUserData_TruncatedToEmpty(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	nonce := "nonce-oversize"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "oversize@example.com", "sub": "sub-oversize"}))

	oversize := `{"name":{"firstName":"` + strings.Repeat("A", 64*1024) + `"}}`
	w := postAppleSession(rig, "apple", map[string]any{
		"id_token":        idToken,
		"state":           state,
		"apple_user_data": oversize,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	name, _ := resp["name"].(string)
	assert.Empty(t, name, "oversize apple_user_data must truncate to empty (Callback parity)")
}

func TestAppleSession_ExistingSSO_IssuesTokens(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	userID := insertSSOTestUser(t, rig.DB, "applesession-existing@example.com", "applesessionexisting")
	_, err := rig.DB.Exec(`UPDATE users SET trust_sso_security = TRUE WHERE id = $1`, userID)
	require.NoError(t, err)
	_, err = rig.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'apple', 'sub-existing', $2)`, userID, "applesession-existing@example.com")
	require.NoError(t, err)

	nonce := "nonce-existing"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "applesession-existing@example.com", "sub": "sub-existing"}))

	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["access_token"], "trusted existing-SSO must mint tokens directly")
	assert.NotEmpty(t, w.Header().Get("Set-Cookie"), "refresh cookie must be set (respondExistingSSO reuse)")
}

func TestAppleSession_ExistingSSO_RequiresMFA(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	userID := insertSSOTestUser(t, rig.DB, "applesession-mfa@example.com", "applesessionmfa")
	// trust_sso_security stays FALSE (insert default) → MFA challenge path.
	_, err := rig.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'apple', 'sub-mfa', $2)`, userID, "applesession-mfa@example.com")
	require.NoError(t, err)

	nonce := "nonce-mfa"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "applesession-mfa@example.com", "sub": "sub-mfa"}))

	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["mfa_challenge_token"], "untrusted existing-SSO must surface the MFA challenge")
	assert.NotEmpty(t, resp["methods"])
}

func TestAppleSession_RealEmail_OffersAccountLink(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	insertSSOTestUser(t, rig.DB, "applesession-link@example.com", "applesessionlink")

	nonce := "nonce-link"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "applesession-link@example.com", "sub": "sub-link"}))

	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["account_link_available"])
	assert.NotEmpty(t, resp["sso_token"])
	assert.Contains(t, resp["masked_email"], "@example.com")
}

func TestAppleSession_RelayEmail_NoAutoLink(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	relay := "abc123@privaterelay.appleid.com"
	insertSSOTestUser(t, rig.DB, relay, "applesessionrelay")

	nonce := "nonce-relay"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email": relay, "is_private_email": true, "sub": "sub-relay"}))

	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["sso_registration_required"],
		"relay emails must NEVER auto-link to an existing account (spec §9 invariant)")
	_, hasLink := resp["account_link_available"]
	assert.False(t, hasLink)
}

func TestAppleSession_JWKSRotation(t *testing.T) {
	rig := newAppleSessionRig(t, false)

	state1 := seedAppleSessionState(t, rig.Redis, "apple", "nonce-rot-1")
	tok1 := rig.JWKS.sign(t, appleSessionClaims(time.Now(), "nonce-rot-1",
		map[string]any{"email": "rot1@example.com", "sub": "sub-rot-1"}))
	w1 := postAppleSession(rig, "apple", map[string]any{"id_token": tok1, "state": state1})
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())

	// Rotate JWKS: the cached kid-1 must not satisfy kid-2 → refetch.
	rig.JWKS.rotate(t)
	state2 := seedAppleSessionState(t, rig.Redis, "apple", "nonce-rot-2")
	tok2 := rig.JWKS.sign(t, appleSessionClaims(time.Now(), "nonce-rot-2",
		map[string]any{"email": "rot2@example.com", "sub": "sub-rot-2"}))
	w2 := postAppleSession(rig, "apple", map[string]any{"id_token": tok2, "state": state2})
	require.Equal(t, http.StatusOK, w2.Code, "kid miss after rotation must re-fetch JWKS")
}

// ───────────────────────── state-class rejections (invalid_state) ─────────────────────────

func TestAppleSession_UnknownState(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), "n", nil))
	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": "never-seeded"})
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_state")
}

func TestAppleSession_StateReplay_Rejected(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	nonce := "nonce-replay"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "replay@example.com", "sub": "sub-replay"}))
	body := map[string]any{"id_token": idToken, "state": state}

	w1 := postAppleSession(rig, "apple", body)
	require.Equal(t, http.StatusOK, w1.Code)
	w2 := postAppleSession(rig, "apple", body)
	require.Equal(t, http.StatusUnauthorized, w2.Code, "GET-then-DEL one-shot must reject replay")
	assert.Contains(t, w2.Body.String(), "invalid_state")
}

func TestAppleSession_GoogleState_Rejected(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	nonce := "nonce-xprov"
	state := seedAppleSessionState(t, rig.Redis, "google", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce, nil))
	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_state")
}

// ───────────────────────── id_token rejections (invalid_id_token) ─────────────────────────

// runs each of the 8 rejection classes through a fresh state so one class's
// state consumption can't mask another's verdict.
func TestAppleSession_IDTokenRejectionMatrix(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	now := time.Now()

	foreignKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cases := map[string]func(nonce string) string{
		"nonce_mismatch": func(_ string) string {
			return rig.JWKS.sign(t, appleSessionClaims(now, "some-other-nonce", nil))
		},
		"bad_signature_foreign_key": func(nonce string) string {
			return signAppleToken(t, jwt.SigningMethodRS256, foreignKey, "apple-kid-1",
				appleSessionClaims(now, nonce, nil))
		},
		"hs256_confusion": func(nonce string) string {
			return signAppleToken(t, jwt.SigningMethodHS256, []byte("shared-secret"), "apple-kid-1",
				appleSessionClaims(now, nonce, nil))
		},
		"expired": func(nonce string) string {
			return rig.JWKS.sign(t, appleSessionClaims(now, nonce,
				map[string]any{"iat": now.Add(-2 * time.Hour).Unix(), "exp": now.Add(-time.Hour).Unix()}))
		},
		"wrong_audience": func(nonce string) string {
			return rig.JWKS.sign(t, appleSessionClaims(now, nonce, map[string]any{"aud": "com.evil.app"}))
		},
		"wrong_issuer": func(nonce string) string {
			return rig.JWKS.sign(t, appleSessionClaims(now, nonce, map[string]any{"iss": "https://evil.example"}))
		},
		"nonce_unsupported": func(nonce string) string {
			return rig.JWKS.sign(t, appleSessionClaims(now, nonce, map[string]any{"nonce_supported": false}))
		},
		"unknown_kid": func(nonce string) string {
			r := rig.JWKS
			r.mu.Lock()
			priv := r.priv
			r.mu.Unlock()
			return signAppleToken(t, jwt.SigningMethodRS256, priv, "kid-not-in-jwks",
				appleSessionClaims(now, nonce, nil))
		},
	}

	i := 0
	for name, mint := range cases {
		i++
		nonce := fmt.Sprintf("nonce-reject-%d", i)
		t.Run(name, func(t *testing.T) {
			state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
			w := postAppleSession(rig, "apple", map[string]any{"id_token": mint(nonce), "state": state})
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "invalid_id_token")
			assert.NotContains(t, w.Body.String(), "invalid_state",
				"token-class failures must not masquerade as state-class")
		})
	}
}

func TestAppleSession_EmailUnverified_Forbidden(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	nonce := "nonce-unverified"
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, appleSessionClaims(time.Now(), nonce,
		map[string]any{"email_verified": false}))

	w := postAppleSession(rig, "apple", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "oauth_email_unverified")
}

// ───────────────────────── request / provider gates ─────────────────────────

func TestAppleSession_ProviderGates(t *testing.T) {
	// google_404_until_975 subtest removed: google is now supported by
	// ProviderSession (#975). Google session coverage lives in
	// google_session_integration_test.go.

	t.Run("apple_unregistered_404", func(t *testing.T) {
		h := oauth.NewHandler(oauth.HandlerDeps{Registry: oauth.NewRegistry()})
		r := gin.New()
		r.POST(fmt.Sprintf(appleSessionPath, ":provider"), h.ProviderSession)
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf(appleSessionPath, "apple"),
			strings.NewReader(`{"id_token":"x","state":"y"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestAppleSession_MalformedBody(t *testing.T) {
	rig := newAppleSessionRig(t, false)

	t.Run("missing_id_token", func(t *testing.T) {
		w := postAppleSession(rig, "apple", map[string]any{"state": "s"})
		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "invalid_request")
	})
	t.Run("missing_state", func(t *testing.T) {
		w := postAppleSession(rig, "apple", map[string]any{"id_token": "t"})
		require.Equal(t, http.StatusBadRequest, w.Code)
	})
	t.Run("oversize_state", func(t *testing.T) {
		w := postAppleSession(rig, "apple", map[string]any{"id_token": "t", "state": strings.Repeat("s", 257)})
		require.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAppleSession_RateLimited(t *testing.T) {
	rig := newAppleSessionRig(t, true)
	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		last = postAppleSession(rig, "apple", map[string]any{"id_token": "x", "state": "not-seeded"})
	}
	require.Equal(t, http.StatusTooManyRequests, last.Code,
		"11th request within the window must trip the 10/15min Callback-parity limit")
}

// ───────────────────────── Callback retirement (#974/#975) ───────────────────────

// TestCallback_Retired_Returns404 pins the #975 retirement: the /:provider/callback
// route is removed entirely (both apple and google). Gin returns 404 — no handler
// exists. The prior #974 apple 410-gate lived inside h.Callback; since the route
// itself is gone, clients get 404 regardless of provider.
func TestCallback_Retired_Returns404(t *testing.T) {
	rig := newAppleSessionRig(t, false)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/apple/callback",
		strings.NewReader(`{"code":"any-code","state":"any-state"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code,
		"/:provider/callback route removed in #975 — Gin returns 404")
}
