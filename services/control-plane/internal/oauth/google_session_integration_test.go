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
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

const (
	googleSessionPath  = "/api/v1/auth/sso/%s/session"
	testGoogleClientID = "test-client.apps.googleusercontent.com"
	testGoogleJWKSPath = "/oauth2/v3/certs"
	testGoogleIssuer   = "https://accounts.google.com"
)

// googleJWKSRig is a JWKS-only fake Google JWKS server. Tests sign id_tokens
// directly with the rig's RSA key. rotate() swaps key + kid to exercise the
// fetchKey cache-miss refetch path (mirrors appleJWKSRig).
type googleJWKSRig struct {
	Server *httptest.Server

	mu   sync.Mutex
	priv *rsa.PrivateKey
	kid  string
	gen  int
}

func newGoogleJWKSRig(t *testing.T) *googleJWKSRig {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rig := &googleJWKSRig{priv: priv, kid: "google-kid-1", gen: 1}

	mux := http.NewServeMux()
	mux.HandleFunc(testGoogleJWKSPath, func(w http.ResponseWriter, _ *http.Request) {
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

// rotate generates a fresh key under a new kid — mirrors appleJWKSRig.rotate.
func (r *googleJWKSRig) rotate(t *testing.T) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.priv = priv
	r.kid = fmt.Sprintf("google-kid-%d", r.gen)
}

// sign mints an RS256 id_token under the rig's currently-published key.
func (r *googleJWKSRig) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	r.mu.Lock()
	priv, kid := r.priv, r.kid
	r.mu.Unlock()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(priv)
	require.NoError(t, err)
	return signed
}

// signWith mints a token with an arbitrary method/key/kid.
func (r *googleJWKSRig) signWith(t *testing.T, method jwt.SigningMethod, key interface{}, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

// googleSessionClaims is the happy-path id_token claim set for Google.
// Unlike Apple: no is_private_email, no nonce_supported, email_verified is
// a plain JSON bool (not appleBool).
func googleSessionClaims(now time.Time, nonce string, overrides map[string]any) jwt.MapClaims {
	claims := jwt.MapClaims{
		"iss":            testGoogleIssuer,
		"aud":            testGoogleClientID,
		"sub":            "117550792896476765432",
		"email":          "jane@example.com",
		"email_verified": true,
		"name":           "Jane Tester",
		"picture":        "https://lh3.googleusercontent.com/a/jane",
		"nonce":          nonce,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		claims[k] = v
	}
	return claims
}

type googleSessionRig struct {
	Engine  *gin.Engine
	Redis   redis.Cmdable
	DB      *sql.DB
	Adapter *fakeAuthAdapter
	JWKS    *googleJWKSRig
}

func newGoogleSessionRig(t *testing.T) *googleSessionRig {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(redisCleanup)

	jwks := newGoogleJWKSRig(t)
	google, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:     testGoogleClientID,
		RedirectURI:  "http://127.0.0.1:0/oauth/callback",
		JWKSEndpoint: jwks.Server.URL + testGoogleJWKSPath,
		Issuer:       testGoogleIssuer,
	})
	require.NoError(t, err)
	registry := oauth.NewRegistry()
	registry.Register(google)

	adapter := &fakeAuthAdapter{}
	h := oauth.NewHandler(oauth.HandlerDeps{
		Registry:    registry,
		Redis:       rdb,
		DB:          db,
		AuthHandler: adapter,
	})
	r := gin.New()
	r.POST(fmt.Sprintf(googleSessionPath, ":provider"), h.ProviderSession)
	return &googleSessionRig{Engine: r, Redis: rdb, DB: db, Adapter: adapter, JWKS: jwks}
}

func postGoogleSession(rig *googleSessionRig, provider string, body map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf(googleSessionPath, provider), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.9:50000"
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)
	return w
}

// ───────────────────────── happy paths ─────────────────────────

func TestGoogleSession_NewUser_RegistrationRequired(t *testing.T) {
	rig := newGoogleSessionRig(t)
	nonce := "nonce-google-newuser"
	state := seedAppleSessionState(t, rig.Redis, "google", nonce)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), nonce, nil))

	w := postGoogleSession(rig, "google", map[string]any{"id_token": idToken, "state": state})
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

func TestGoogleSession_ExistingSSO_IssuesTokens(t *testing.T) {
	rig := newGoogleSessionRig(t)
	userID := insertSSOTestUser(t, rig.DB, "googlesession-existing@example.com", "googleexisting")
	_, err := rig.DB.Exec(`UPDATE users SET trust_sso_security = TRUE WHERE id = $1`, userID)
	require.NoError(t, err)
	_, err = rig.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'google-sub-existing', $2)`, userID, "googlesession-existing@example.com")
	require.NoError(t, err)

	nonce := "nonce-google-existing"
	state := seedAppleSessionState(t, rig.Redis, "google", nonce)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "googlesession-existing@example.com", "sub": "google-sub-existing"}))

	w := postGoogleSession(rig, "google", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["access_token"], "trusted existing-SSO must mint tokens directly")
	assert.NotEmpty(t, w.Header().Get("Set-Cookie"), "refresh cookie must be set")
}

func TestGoogleSession_ExistingSSO_RequiresMFA(t *testing.T) {
	rig := newGoogleSessionRig(t)
	userID := insertSSOTestUser(t, rig.DB, "googlesession-mfa@example.com", "googlemfa")
	// trust_sso_security stays FALSE → MFA challenge path.
	_, err := rig.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'google-sub-mfa', $2)`, userID, "googlesession-mfa@example.com")
	require.NoError(t, err)

	nonce := "nonce-google-mfa"
	state := seedAppleSessionState(t, rig.Redis, "google", nonce)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "googlesession-mfa@example.com", "sub": "google-sub-mfa"}))

	w := postGoogleSession(rig, "google", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["mfa_challenge_token"], "untrusted existing-SSO must surface MFA challenge")
	assert.NotEmpty(t, resp["methods"])
}

func TestGoogleSession_RealEmail_OffersAccountLink(t *testing.T) {
	rig := newGoogleSessionRig(t)
	insertSSOTestUser(t, rig.DB, "googlesession-link@example.com", "googlelink")

	nonce := "nonce-google-link"
	state := seedAppleSessionState(t, rig.Redis, "google", nonce)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "googlesession-link@example.com", "sub": "google-sub-link"}))

	w := postGoogleSession(rig, "google", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["account_link_available"])
	assert.NotEmpty(t, resp["sso_token"])
	assert.Contains(t, resp["masked_email"], "@example.com")
}

func TestGoogleSession_JWKSRotation(t *testing.T) {
	rig := newGoogleSessionRig(t)

	state1 := seedAppleSessionState(t, rig.Redis, "google", "nonce-grot-1")
	tok1 := rig.JWKS.sign(t, googleSessionClaims(time.Now(), "nonce-grot-1",
		map[string]any{"email": "grot1@example.com", "sub": "sub-grot-1"}))
	w1 := postGoogleSession(rig, "google", map[string]any{"id_token": tok1, "state": state1})
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())

	// Rotate JWKS: the cached kid-1 must not satisfy kid-2 → refetch.
	rig.JWKS.rotate(t)
	state2 := seedAppleSessionState(t, rig.Redis, "google", "nonce-grot-2")
	tok2 := rig.JWKS.sign(t, googleSessionClaims(time.Now(), "nonce-grot-2",
		map[string]any{"email": "grot2@example.com", "sub": "sub-grot-2"}))
	w2 := postGoogleSession(rig, "google", map[string]any{"id_token": tok2, "state": state2})
	require.Equal(t, http.StatusOK, w2.Code, "kid miss after rotation must re-fetch JWKS")
}

// ───────────────────────── state-class rejections (invalid_state) ─────────────────────────

func TestGoogleSession_UnknownState(t *testing.T) {
	rig := newGoogleSessionRig(t)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), "n", nil))
	w := postGoogleSession(rig, "google", map[string]any{"id_token": idToken, "state": "never-seeded"})
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_state")
}

func TestGoogleSession_StateReplay_Rejected(t *testing.T) {
	rig := newGoogleSessionRig(t)
	nonce := "nonce-google-replay"
	state := seedAppleSessionState(t, rig.Redis, "google", nonce)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), nonce,
		map[string]any{"email": "greplay@example.com", "sub": "sub-greplay"}))
	body := map[string]any{"id_token": idToken, "state": state}

	w1 := postGoogleSession(rig, "google", body)
	require.Equal(t, http.StatusOK, w1.Code)
	w2 := postGoogleSession(rig, "google", body)
	require.Equal(t, http.StatusUnauthorized, w2.Code, "GET-then-DEL one-shot must reject replay")
	assert.Contains(t, w2.Body.String(), "invalid_state")
}

func TestGoogleSession_AppleState_Rejected(t *testing.T) {
	rig := newGoogleSessionRig(t)
	nonce := "nonce-gxprov"
	// state was seeded for "apple" — should fail provider-binding check.
	state := seedAppleSessionState(t, rig.Redis, "apple", nonce)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), nonce, nil))
	w := postGoogleSession(rig, "google", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_state")
}

// ───────────────────────── id_token rejections (invalid_id_token) ─────────────────────────

func TestGoogleSession_IDTokenRejectionMatrix(t *testing.T) {
	rig := newGoogleSessionRig(t)
	now := time.Now()

	foreignKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cases := map[string]func(nonce string) string{
		"nonce_mismatch": func(_ string) string {
			return rig.JWKS.sign(t, googleSessionClaims(now, "some-other-nonce", nil))
		},
		"bad_signature_foreign_key": func(nonce string) string {
			return rig.JWKS.signWith(t, jwt.SigningMethodRS256, foreignKey, "google-kid-1",
				googleSessionClaims(now, nonce, nil))
		},
		"hs256_confusion": func(nonce string) string {
			return rig.JWKS.signWith(t, jwt.SigningMethodHS256, []byte("shared-secret"), "google-kid-1",
				googleSessionClaims(now, nonce, nil))
		},
		"expired": func(nonce string) string {
			return rig.JWKS.sign(t, googleSessionClaims(now, nonce,
				map[string]any{"iat": now.Add(-2 * time.Hour).Unix(), "exp": now.Add(-time.Hour).Unix()}))
		},
		"wrong_audience": func(nonce string) string {
			return rig.JWKS.sign(t, googleSessionClaims(now, nonce, map[string]any{"aud": "com.evil.app"}))
		},
		"wrong_issuer": func(nonce string) string {
			return rig.JWKS.sign(t, googleSessionClaims(now, nonce, map[string]any{"iss": "https://evil.example"}))
		},
		"unknown_kid": func(nonce string) string {
			r := rig.JWKS
			r.mu.Lock()
			priv := r.priv
			r.mu.Unlock()
			return rig.JWKS.signWith(t, jwt.SigningMethodRS256, priv, "kid-not-in-jwks",
				googleSessionClaims(now, nonce, nil))
		},
	}

	i := 0
	for name, mint := range cases {
		i++
		nonce := fmt.Sprintf("nonce-greject-%d", i)
		t.Run(name, func(t *testing.T) {
			state := seedAppleSessionState(t, rig.Redis, "google", nonce)
			w := postGoogleSession(rig, "google", map[string]any{"id_token": mint(nonce), "state": state})
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "invalid_id_token")
			assert.NotContains(t, w.Body.String(), "invalid_state",
				"token-class failures must not masquerade as state-class")
		})
	}
}

func TestGoogleSession_EmailUnverified_Forbidden(t *testing.T) {
	rig := newGoogleSessionRig(t)
	nonce := "nonce-gunverified"
	state := seedAppleSessionState(t, rig.Redis, "google", nonce)
	idToken := rig.JWKS.sign(t, googleSessionClaims(time.Now(), nonce,
		map[string]any{"email_verified": false}))

	w := postGoogleSession(rig, "google", map[string]any{"id_token": idToken, "state": state})
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "oauth_email_unverified")
}

// ───────────────────────── provider gates ─────────────────────────

func TestGoogleSession_AppleProvider_Unregistered_404(t *testing.T) {
	// Google registered, Apple NOT registered — requesting apple session → 404.
	// "apple" passes the apple|google allowlist gate, then Registry.Get("apple")
	// fails because only google is wired into this rig.
	rig := newGoogleSessionRig(t) // only google in registry
	w := postGoogleSession(rig, "apple", map[string]any{"id_token": "x", "state": "y"})
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "unknown_provider")
}

func TestGoogleSession_UnknownProvider_404(t *testing.T) {
	rig := newGoogleSessionRig(t)
	w := postGoogleSession(rig, "facebook", map[string]any{"id_token": "x", "state": "y"})
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "unknown_provider")
}

func TestGoogleSession_MalformedBody(t *testing.T) {
	rig := newGoogleSessionRig(t)

	t.Run("missing_id_token", func(t *testing.T) {
		w := postGoogleSession(rig, "google", map[string]any{"state": "s"})
		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "invalid_request")
	})
	t.Run("missing_state", func(t *testing.T) {
		w := postGoogleSession(rig, "google", map[string]any{"id_token": "t"})
		require.Equal(t, http.StatusBadRequest, w.Code)
	})
}
