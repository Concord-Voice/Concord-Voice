package oauth_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// newSSOTestRig wires a Gin engine, test Redis, and a Handler with the Google
// provider registered. Returns the engine, the Redis client (so tests can read
// state keys directly), and the handler.
func newSSOTestRig(t *testing.T) (*gin.Engine, *redis.Client, *oauth.Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	provider, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:    "test-client.apps.googleusercontent.com",
		RedirectURI: "http://127.0.0.1:0/oauth/callback",
	})
	require.NoError(t, err)
	registry := oauth.NewRegistry()
	registry.Register(provider)

	h := oauth.NewHandler(oauth.HandlerDeps{
		Registry: registry,
		Redis:    rdb,
	})
	r := gin.New()
	g := r.Group("/api/v1/auth/sso")
	g.GET("/:provider", h.Initiate)
	return r, rdb, h
}

func TestInitiate_StoresStateInRedis(t *testing.T) {
	r, rdb, _ := newSSOTestRig(t)

	// After #975, google is client-driven — a code_challenge is required.
	challenge := strings.Repeat("c", 43)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/sso/google?redirect_uri=http%3A%2F%2F127.0.0.1%3A65000%2Foauth%2Fcallback&code_challenge="+challenge, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.NotEmpty(t, resp["auth_url"])
	require.NotEmpty(t, resp["state"])
	require.NotEmpty(t, resp["nonce"], "google /initiate must return nonce after #975")

	stored, err := rdb.Get(context.Background(), "sso_state:"+resp["state"]).Result()
	require.NoError(t, err)
	assert.Contains(t, stored, `"provider":"google"`)
	assert.Contains(t, stored, `"nonce":`)
	// After #975 google is client-driven: code_verifier stored as empty string.
	assert.Contains(t, stored, `"code_verifier":""`)

	ttl, err := rdb.TTL(context.Background(), "sso_state:"+resp["state"]).Result()
	require.NoError(t, err)
	assert.True(t, ttl > 8*time.Minute && ttl <= 10*time.Minute, "ttl=%s", ttl)
}

func TestInitiate_AuthURLEmbedsState(t *testing.T) {
	r, _, _ := newSSOTestRig(t)
	// After #975, google is client-driven — supply a valid code_challenge.
	challenge := strings.Repeat("d", 43)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/sso/google?redirect_uri=http%3A%2F%2F127.0.0.1%3A65000%2Foauth%2Fcallback&code_challenge="+challenge, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))

	parsed, err := url.Parse(resp["auth_url"])
	require.NoError(t, err)
	q := parsed.Query()
	assert.Equal(t, resp["state"], q.Get("state"))
	// After #975 google embeds the CLIENT-supplied challenge (not a server-generated one).
	assert.Equal(t, challenge, q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	// redirect_uri must be overridden to the request-time loopback URI.
	assert.Equal(t, "http://127.0.0.1:65000/oauth/callback", q.Get("redirect_uri"))
}

func TestInitiate_RejectsNonLoopbackRedirect(t *testing.T) {
	r, _, _ := newSSOTestRig(t)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/sso/google?redirect_uri=https%3A%2F%2Fevil.example%2Fcallback", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "redirect_uri")
}

func TestInitiate_UnknownProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	registry := oauth.NewRegistry() // empty
	h := oauth.NewHandler(oauth.HandlerDeps{Registry: registry, Redis: rdb})
	r := gin.New()
	r.GET("/api/v1/auth/sso/:provider", h.Initiate)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/sso/google?redirect_uri=http%3A%2F%2F127.0.0.1%3A1%2Foauth%2Fcallback", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestCallback_RouteRemoved_Returns404 pins the #975 retirement: the server-side
// Google Callback route is removed. Any client still hitting /:provider/callback
// gets a 404 from Gin (route not registered). Old desktop clients that haven't
// updated will see this; the correct path is /:provider/session (#974/#975).
func TestCallback_RouteRemoved_Returns404(t *testing.T) {
	r, _, _ := newSSOTestRig(t) // does NOT register a callback route
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/google/callback",
		strings.NewReader(`{"code":"x","state":"y"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// ===========================================================================
// CompleteRegistration and CompleteLink shared rig (Callback removed in #975)
// ===========================================================================

// ssoCallbackRig wires a real DB + Redis behind a Gin router. Used by
// CompleteRegistration and CompleteLink tests. The name is preserved for
// continuity; the Callback route itself was removed in #975.
type ssoCallbackRig struct {
	Engine  *gin.Engine
	Redis   redis.Cmdable
	DB      *sql.DB
	Handler *oauth.Handler
	// Adapter is exposed so individual tests can flip behavior flags
	// post-construction (e.g. MFANotEnabled, IssueAccessFail) without having
	// to pre-thread a customized adapter through newSSOCallbackTestRigWithAuth.
	// Tests that need a fundamentally different adapter still use the
	// WithAuth constructor.
	Adapter *fakeAuthAdapter
}

// newSSOCallbackTestRig builds a rig for CompleteRegistration/CompleteLink tests.
// Endpoint overrides on GoogleConfig let the provider talk to httptest.
// Defaults the auth adapter to a passing stub.
func newSSOCallbackTestRig(t *testing.T, fakeGoogleBaseURL string) *ssoCallbackRig {
	t.Helper()
	adapter := &fakeAuthAdapter{}
	rig := newSSOCallbackTestRigWithAuth(t, fakeGoogleBaseURL, adapter)
	rig.Adapter = adapter
	return rig
}

// newSSOCallbackTestRigWithAuth allows callers to inject a custom auth
// adapter (e.g., one configured to return errors) so the handler's
// error-recovery paths can be exercised. The Callback route is NOT
// registered — it was removed in #975.
func newSSOCallbackTestRigWithAuth(t *testing.T, fakeGoogleBaseURL string, authStub oauth.AuthAdapter) *ssoCallbackRig {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(redisCleanup)

	provider, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:     "test-client.apps.googleusercontent.com",
		RedirectURI:  "http://127.0.0.1:0/oauth/callback",
		JWKSEndpoint: fakeGoogleBaseURL + "/jwks",
		Issuer:       "https://accounts.google.com",
	})
	require.NoError(t, err)
	registry := oauth.NewRegistry()
	registry.Register(provider)

	h := oauth.NewHandler(oauth.HandlerDeps{
		Registry:    registry,
		Redis:       rdb,
		DB:          db,
		AuthHandler: authStub,
	})
	r := gin.New()
	g := r.Group("/api/v1/auth/sso")
	g.GET("/:provider", h.Initiate)
	return &ssoCallbackRig{Engine: r, Redis: rdb, DB: db, Handler: h}
}

// insertSSOTestUser inserts a minimal users row (no E2EE keys) sufficient for
// the callback handler's lookups. trust_sso_security and email_verified default
// to FALSE/TRUE respectively unless overridden by the caller via UPDATE.
func insertSSOTestUser(t *testing.T, db *sql.DB, email, username string) string {
	t.Helper()
	userID := uuid.New().String()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, $4, true, true)`,
		userID, email, username, "$argon2id$placeholder", // pragma: allowlist secret
	)
	require.NoError(t, err)
	return userID
}

// fakeAuthAdapter implements oauth.AuthAdapter for callback tests without
// pulling in internal/auth (which would create an import cycle).
//
// IssueAccessFail / IssueMFAFail / HashFail / ValidateUsernameFail /
// ValidatePasswordFail / VerifyPasswordLocked let individual tests inject
// error returns from those methods to exercise the handler's error branches.
// MFANotEnabled simulates a user with trust_sso_security=FALSE but no MFA
// enrolled — the adapter returns mfaEnabled=false and the handler must fall
// through to direct token issuance rather than serving an unverifiable
// challenge token.
//
// Default zero-value is the happy path (all validations pass, all issuances
// succeed, MFA is enrolled).
type fakeAuthAdapter struct {
	IssueAccessFail      bool
	IssueMFAFail         bool
	MFANotEnabled        bool
	HashFail             bool
	ValidateUsernameFail bool
	ValidatePasswordFail bool
	// VerifyPasswordLocked makes VerifyPassword return the
	// auth.ErrAccountLocked sentinel, simulating the lockout-counter
	// threshold being reached. The handler matches via errors.Is and
	// translates to HTTP 423 Locked. Used by CompleteLink's
	// 423-translation test path.
	VerifyPasswordLocked bool
}

func (f *fakeAuthAdapter) IssueAccessAndRefresh(_ context.Context, userID string) (string, string, error) {
	if f.IssueAccessFail {
		return "", "", fmt.Errorf("token issuance unavailable")
	}
	return "fake-access-" + userID, "fake-refresh-" + userID, nil
}
func (f *fakeAuthAdapter) IssueMFAChallenge(_ context.Context, userID string) (string, []string, []string, interface{}, bool, error) {
	if f.IssueMFAFail {
		return "", nil, nil, nil, false, fmt.Errorf("mfa unavailable")
	}
	if f.MFANotEnabled {
		// User has trust_sso_security=FALSE but no MFA enrolled — caller
		// must fall through to direct token issuance.
		return "", nil, nil, nil, false, nil
	}
	// Deterministic happy-path stub: TOTP-only, no recovery-only methods,
	// no WebAuthn options. Tests that want to drive the recovery_only or
	// webauthn_options branches should extend this stub or use the real
	// adapter via internal/auth/oauth_adapter_test.go.
	return "fake-mfa-" + userID, []string{"totp"}, nil, nil, true, nil
}
func (f *fakeAuthAdapter) VerifyPassword(_ context.Context, _, password string) error {
	if f.VerifyPasswordLocked {
		// Return the production sentinel so the handler can match via
		// errors.Is — same wire as the real auth.Handler.VerifyPassword.
		return auth.ErrAccountLocked
	}
	if password == "correct-password" { // pragma: allowlist secret
		return nil
	}
	return fmt.Errorf("invalid_credentials")
}
func (f *fakeAuthAdapter) HashPassword(_ context.Context, password string) (string, error) {
	if f.HashFail {
		return "", fmt.Errorf("hash unavailable")
	}
	// Deterministic stub — real Argon2id hashing is exercised in internal/auth tests.
	// The prefix lets test assertions distinguish hashed values from plaintext.
	return "argon2id:fake-hash:" + password, nil
}
func (f *fakeAuthAdapter) ValidateUsername(_ string) error {
	if f.ValidateUsernameFail {
		return fmt.Errorf("username contains reserved word")
	}
	return nil
}
func (f *fakeAuthAdapter) NormalizeUsername(u string) string {
	return strings.ToLower(u)
}
func (f *fakeAuthAdapter) ValidatePasswordStrength(_ string) error {
	if f.ValidatePasswordFail {
		return fmt.Errorf("password must contain at least 3 character classes")
	}
	return nil
}

// ===========================================================================
// CompleteRegistration tests
// ===========================================================================

// TestCompleteRegistration_HappyPath exercises the new-user finalization
// path end-to-end: a previously issued sso_token (Redis) is consumed,
// users + user_keys + public_keys + user_sso_identities rows are inserted
// in a single transaction, and an access_token is returned. Critical
// invariants verified: password_login_disabled=TRUE on the new user, // pragma: allowlist secret
// user_sso_identities link is present, sso_token is consumed (single-use).
func TestCompleteRegistration_HappyPath(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	// Seed sso_token Redis record (as if Callback had set it).
	ssoToken := "sso-token-test-1" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-newcomp",
		"provider_email":   "newcomp@example.test",
		"name":             "New Comp",
		"branch":           "new_user",
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "newcomp",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("wrapped-priv-key-bytes")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("salt-bytes-xxxxxxx")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("public-key-bytes")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotEmpty(t, resp["access_token"])

	// Verify users row created with password_login_disabled=TRUE. // pragma: allowlist secret
	var pld bool
	require.NoError(t, rig.DB.QueryRow(
		`SELECT password_login_disabled FROM users WHERE email = $1`, "newcomp@example.test",
	).Scan(&pld))
	assert.True(t, pld, "SSO-registered users must have password_login_disabled=TRUE") // pragma: allowlist secret

	// Verify user_sso_identities row exists for this provider/sub.
	var providerName string
	require.NoError(t, rig.DB.QueryRow(
		`SELECT provider FROM user_sso_identities WHERE provider_user_id = $1`, "google-sub-newcomp",
	).Scan(&providerName))
	assert.Equal(t, "google", providerName)

	// Verify sso_token consumed (single-use defense).
	_, err = rig.Redis.Get(context.Background(), "sso_token:"+ssoToken).Bytes()
	require.Error(t, err, "sso_token must be deleted after consumption")
}

// TestCompleteRegistration_NormalizesUsername locks the #1931 SSO half: the
// username is stored normalized (lowercase) so the SSO path matches the password
// path. A mixed-case submission must persist lowercase in users.username.
func TestCompleteRegistration_NormalizesUsername(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-mixedcase-1" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-mixedcase",
		"provider_email":   "mixedcase@example.test",
		"name":             "Mixed Case",
		"branch":           "new_user",
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "NewCompMixed",         // mixed-case submission
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("wrapped-priv-key-bytes")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("salt-bytes-xxxxxxx")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("public-key-bytes")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)

	var storedUsername string
	require.NoError(t, rig.DB.QueryRow(
		`SELECT username FROM users WHERE email = $1`, "mixedcase@example.test",
	).Scan(&storedUsername))
	assert.Equal(t, "newcompmixed", storedUsername,
		"SSO registration must store the username normalized (lowercase) — #1931")
}

// TestCompleteRegistration_ReplayedSSOToken_Rejected covers the single-use
// invariant for sso_token: if no Redis record exists for the supplied token
// (forged, expired, or already-consumed), the handler refuses with 401
// sso_token_invalid before any DB writes.
func TestCompleteRegistration_ReplayedSSOToken_Rejected(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	// Don't seed a token — request a non-existent one. Username/password
	// satisfy the binding tags so the sso_token replay check (NOT input
	// validation) is what produces the 401.
	body := map[string]any{
		"sso_token":           "never-existed",
		"username":            "ghost",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "sso_token_invalid")
}

// TestCompleteRegistration_InvalidJSON covers the binding-failure path.
// A malformed body fails ShouldBindJSON before any Redis or DB call, so
// no sso_token is consumed and no users row is created.
func TestCompleteRegistration_InvalidJSON(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_request")
}

// TestCompleteRegistration_BranchMismatch_Rejected covers the critical
// security check: a token issued for the account-link path must not be
// accepted on the registration finalization route. Otherwise an attacker
// who steals a link-flow token could register a fresh account in the
// targeted user's name.
func TestCompleteRegistration_BranchMismatch_Rejected(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-link-branch" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-link",
		"provider_email":   "linktarget@example.test",
		"branch":           "account_link", // NOT new_user
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "linkattempt",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "sso_token_invalid")

	// And no users row should have been created.
	var n int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE email = $1`, "linktarget@example.test",
	).Scan(&n))
	assert.Zero(t, n, "branch-mismatch must not create a users row")
}

// TestCompleteRegistration_InvalidKeyMaterial covers the base64-decode
// failure path: malformed wrapped_private_key/salt/public_key returns
// 400 invalid_key_material. The sso_token IS consumed even on this
// failure (it has already been Get-then-Del'd by then), which is fine —
// the user can re-initiate from /Initiate.
func TestCompleteRegistration_InvalidKeyMaterial(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-bad-base64" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-badb64",
		"provider_email":   "badb64@example.test",
		"branch":           "new_user",
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "badb64",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": "!!!not-base64!!!",
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_key_material")
}

// TestCompleteRegistration_EmailTaken_409 covers the UNIQUE-violation
// branch on users_email_key. A user with the same email already exists
// (registered via the password path), and SSO registration must surface
// a 409 email_taken error code rather than silently failing the tx.
func TestCompleteRegistration_EmailTaken_409(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	// Pre-create a user with the email the SSO flow will try to claim.
	_ = insertSSOTestUser(t, rig.DB, "taken@example.test", "alreadyhere")

	ssoToken := "sso-token-email-taken" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-taken-email",
		"provider_email":   "taken@example.test",
		"branch":           "new_user",
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "newusername",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "email_taken")
}

// TestCompleteRegistration_UsernameTaken_409 covers the parallel
// UNIQUE-violation branch on users_username_key.
func TestCompleteRegistration_UsernameTaken_409(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	// Pre-create a user with the username the SSO flow will try to claim.
	_ = insertSSOTestUser(t, rig.DB, "different@example.test", "takenname")

	ssoToken := "sso-token-username-taken" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-taken-name",
		"provider_email":   "fresh@example.test",
		"branch":           "new_user",
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "takenname",            // collides with seeded user
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "username_taken")
}

// TestCompleteRegistration_UsernameTaken_CaseVariant_409 locks the #1931 review
// finding: an SSO username that collides only CASE-INSENSITIVELY with an existing
// user must still map to 409 username_taken (not 500). The username is normalized
// to lowercase before INSERT (C2), so the duplicate trips a unique index; the
// error-code detection matches BOTH users_username_key and users_username_lower_key
// so the 409 mapping is order-independent.
func TestCompleteRegistration_UsernameTaken_CaseVariant_409(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	// Seed a lowercase user; the SSO flow will submit a mixed-case variant.
	_ = insertSSOTestUser(t, rig.DB, "casevariant-seed@example.test", "casevariant")

	ssoToken := "sso-token-username-taken-casevariant" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-casevariant",
		"provider_email":   "casevariant-fresh@example.test",
		"branch":           "new_user",
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "CaseVariant",          // case-insensitive collision with 'casevariant'
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code, "case-variant collision must be 409, not 500")
	assert.Contains(t, w.Body.String(), "username_taken")
}

// TestCompleteRegistration_TokenIssuanceFails covers the error path after
// commit succeeds but the AuthAdapter fails to mint tokens. Per the SSO
// design, the user row IS persisted — the user can retry login. Handler
// returns 500 token_issuance_failed without rolling back the new user.
func TestCompleteRegistration_TokenIssuanceFails(t *testing.T) {
	rig := newSSOCallbackTestRigWithAuth(t, "http://unused", &fakeAuthAdapter{IssueAccessFail: true})
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-issuance-fail" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-issuance",
		"provider_email":   "issuance@example.test",
		"branch":           "new_user",
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "issuancefail",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "token_issuance_failed")

	// Verify the user IS persisted despite token-issuance failure: the
	// IssueAccessAndRefresh call happens AFTER tx.Commit, so the new user
	// must already be in the DB. This locks in the post-commit ordering
	// invariant — if a future refactor moved IssueAccessAndRefresh before
	// Commit, the rollback would silently undo the user, and this test
	// would fail rather than silently pass on the 500 alone.
	var userCount int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE email = $1`, "issuance@example.test",
	).Scan(&userCount))
	assert.Equal(t, 1, userCount, "user should be persisted even when token issuance fails")
}

// TestCompleteRegistration_HashFails covers the AuthAdapter.HashPassword
// failure branch: the adapter returns an error, so the handler must
// return 500 hash_failed before opening the DB transaction.
func TestCompleteRegistration_HashFails(t *testing.T) {
	rig := newSSOCallbackTestRigWithAuth(t, "http://unused", &fakeAuthAdapter{HashFail: true})
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-hash-fail" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-hashfail",
		"provider_email":   "hashfail@example.test",
		"branch":           "new_user",
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "hashfail",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "hash_failed")

	// And no users row should have been created.
	var n int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE email = $1`, "hashfail@example.test",
	).Scan(&n))
	assert.Zero(t, n, "hash failure must not create a users row")
}

// TestCompleteRegistration_MalformedRedisJSON covers the defensive path:
// the sso_token Redis record is corrupt (e.g., a deploy with mismatched
// schema, or an out-of-band write). The handler must reject with 401
// sso_token_invalid rather than panic on unmarshal.
func TestCompleteRegistration_MalformedRedisJSON(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-bad-redis" //nolint:gosec // test fixture, not a real secret
	// Seed a non-JSON byte string deliberately.
	require.NoError(t, rig.Redis.Set(context.Background(),
		"sso_token:"+ssoToken, "not-json-payload", 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "badredis",
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "sso_token_invalid")
}

// TestCompleteRegistration_InvalidUsername covers the username-validation
// gate: AuthAdapter.ValidateUsername returns an error (e.g., reserved word,
// profanity, bad charset). Handler must return 400 invalid_username with the
// validator's error in 'detail', and must NOT consume the sso_token (the
// user can fix the username and retry without re-doing the OAuth dance).
func TestCompleteRegistration_InvalidUsername(t *testing.T) {
	rig := newSSOCallbackTestRigWithAuth(t, "http://unused", &fakeAuthAdapter{ValidateUsernameFail: true})
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-bad-username" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-baduser",
		"provider_email":   "baduser@example.test",
		"branch":           "new_user",
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "admin",                // valid binding tag (3..50), rejected by stub
		"password":            "TestPassphrase!12345", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_username")
	// The validator's error message is surfaced in 'detail' for actionable UX.
	assert.Contains(t, w.Body.String(), "reserved")

	// And no users row should have been created.
	var n int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE email = $1`, "baduser@example.test",
	).Scan(&n))
	assert.Zero(t, n, "username validation failure must not create a users row")

	// And the sso_token must NOT have been consumed — validation runs before
	// the Redis Get, so the user can retry with a corrected username.
	_, err = rig.Redis.Get(context.Background(), "sso_token:"+ssoToken).Bytes()
	require.NoError(t, err, "sso_token must remain available after username validation failure")
}

// TestCompleteRegistration_InvalidPassword covers the password-strength
// gate: AuthAdapter.ValidatePasswordStrength returns an error (e.g., missing
// char-class diversity). Handler must return 400 invalid_password and must
// NOT consume the sso_token.
func TestCompleteRegistration_InvalidPassword(t *testing.T) {
	rig := newSSOCallbackTestRigWithAuth(t, "http://unused", &fakeAuthAdapter{ValidatePasswordFail: true})
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration",
		rig.Handler.CompleteRegistration)

	ssoToken := "sso-token-bad-password" //nolint:gosec // test fixture, not a real secret
	payload := map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-badpass",
		"provider_email":   "badpass@example.test",
		"branch":           "new_user",
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_token:"+ssoToken, raw, 5*time.Minute).Err())

	body := map[string]any{
		"sso_token": ssoToken,
		"username":  "freshuser",
		// Valid binding tags (12..128), rejected by stub.
		"password":            "passwordpassword123", // pragma: allowlist secret
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("k")),
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("s")),
		"public_key":          base64.StdEncoding.EncodeToString([]byte("p")),
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_password")
	// The validator's error message is surfaced in 'detail'.
	assert.Contains(t, w.Body.String(), "character classes")

	// And no users row should have been created.
	var n int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE email = $1`, "badpass@example.test",
	).Scan(&n))
	assert.Zero(t, n, "password validation failure must not create a users row")

	// And the sso_token must NOT have been consumed.
	_, err = rig.Redis.Get(context.Background(), "sso_token:"+ssoToken).Bytes()
	require.NoError(t, err, "sso_token must remain available after password validation failure")
}

// ===========================================================================
// CompleteLink tests
// ===========================================================================

// TestCompleteLink_HappyPath exercises the email-match account-linking flow
// end-to-end: an existing user with a verified email, a previously-issued
// account_link sso_token (Redis), and the user's correct password produces
// a 200 + access_token. Critical invariants:
//   - user_sso_identities row created for (provider, sub)
//   - password_login_disabled MUST stay FALSE (linking does not flip it)
//   - sso_token is consumed (single-use)
func TestCompleteLink_HappyPath(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-link", rig.Handler.CompleteLink)

	userID := insertSSOTestUser(t, rig.DB, "linkme@example.test", "linkme")

	ssoToken := "sso-link-token-1" //nolint:gosec // test fixture, not a real secret
	payload, err := json.Marshal(map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-link-1",
		"provider_email":   "linkme@example.test",
		"target_user_id":   userID,
		"branch":           "account_link",
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(),
		"sso_token:"+ssoToken, payload, 5*time.Minute).Err())

	body, err := json.Marshal(map[string]string{
		"sso_token": ssoToken,
		"password":  "correct-password", // pragma: allowlist secret
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-link", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotEmpty(t, resp["access_token"])

	// Verify identity row was created.
	var providerName string
	require.NoError(t, rig.DB.QueryRow(
		`SELECT provider FROM user_sso_identities WHERE provider_user_id = $1`,
		"google-sub-link-1",
	).Scan(&providerName))
	assert.Equal(t, "google", providerName)

	// Verify password_login_disabled stays FALSE — linking MUST NOT silently
	// flip the user's password-posture. The user explicitly opted to link;
	// they did not opt out of password login.
	var pld bool
	require.NoError(t, rig.DB.QueryRow(
		`SELECT password_login_disabled FROM users WHERE id = $1`, userID,
	).Scan(&pld))
	assert.False(t, pld, "Linking should NOT silently flip password_login_disabled")

	// Verify sso_token consumed (single-use defense).
	_, err = rig.Redis.Get(context.Background(), "sso_token:"+ssoToken).Bytes()
	require.Error(t, err, "sso_token must be deleted after consumption")
}

// TestCompleteLink_WrongPassword exercises the password-mismatch branch:
// the AuthAdapter returns a generic invalid_credentials error (not the
// auth.ErrAccountLocked sentinel), so the handler must respond 401 with
// the invalid_credentials error_code.
func TestCompleteLink_WrongPassword(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-link", rig.Handler.CompleteLink)

	userID := insertSSOTestUser(t, rig.DB, "wrongpw@example.test", "wrongpw")

	ssoToken := "sso-link-token-wrong" //nolint:gosec // test fixture, not a real secret
	payload, err := json.Marshal(map[string]any{
		"provider":         "google",
		"provider_user_id": "x",
		"provider_email":   "wrongpw@example.test",
		"target_user_id":   userID,
		"branch":           "account_link",
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(),
		"sso_token:"+ssoToken, payload, 5*time.Minute).Err())

	body, err := json.Marshal(map[string]string{
		"sso_token": ssoToken,
		"password":  "wrong",
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-link", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_credentials")
}

// TestCompleteLink_AccountLocked exercises the lockout-inheritance branch:
// AuthAdapter.VerifyPassword returns the auth.ErrAccountLocked sentinel
// (which the production adapter returns when the shared /login lockout
// counter trips). The handler matches via errors.Is and must translate to
// 423 Locked + account_locked error_code, NOT 401.
func TestCompleteLink_AccountLocked(t *testing.T) {
	rig := newSSOCallbackTestRigWithAuth(t, "http://unused", &fakeAuthAdapter{VerifyPasswordLocked: true})
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-link", rig.Handler.CompleteLink)

	userID := insertSSOTestUser(t, rig.DB, "locked@example.test", "lockeduser")

	ssoToken := "sso-link-token-locked" //nolint:gosec // test fixture, not a real secret
	payload, err := json.Marshal(map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-locked",
		"provider_email":   "locked@example.test",
		"target_user_id":   userID,
		"branch":           "account_link",
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(),
		"sso_token:"+ssoToken, payload, 5*time.Minute).Err())

	body, err := json.Marshal(map[string]string{
		"sso_token": ssoToken,
		"password":  "correct-password", // pragma: allowlist secret
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-link", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusLocked, w.Code)
	assert.Contains(t, w.Body.String(), "account_locked")
	// And no SSO identity row should have been created — the link must not
	// proceed past a locked account, even if the account_link sso_token is
	// otherwise valid.
	var n int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, userID,
	).Scan(&n))
	assert.Zero(t, n, "account_locked must not create an SSO identity row")
}

// TestCompleteLink_NewUserBranch_Rejected covers the symmetric security check
// to TestCompleteRegistration_BranchMismatch_Rejected: a token issued for the
// new-user registration path must not be accepted on the account-link route.
// Otherwise an attacker who steals a registration-flow token could forge an
// SSO identity row binding the SSO sub to an arbitrary existing account.
func TestCompleteLink_NewUserBranch_Rejected(t *testing.T) {
	rig := newSSOCallbackTestRig(t, "http://unused")
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-link", rig.Handler.CompleteLink)

	userID := insertSSOTestUser(t, rig.DB, "newbranch@example.test", "newbranch")

	ssoToken := "sso-link-token-newbranch" //nolint:gosec // test fixture, not a real secret
	payload, err := json.Marshal(map[string]any{
		"provider":         "google",
		"provider_user_id": "google-sub-newbranch",
		"provider_email":   "newbranch@example.test",
		"target_user_id":   "",         // empty for new_user; populated for account_link
		"branch":           "new_user", // NOT account_link
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(),
		"sso_token:"+ssoToken, payload, 5*time.Minute).Err())

	body, err := json.Marshal(map[string]string{
		"sso_token": ssoToken,
		"password":  "correct-password", // pragma: allowlist secret
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-link", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "sso_token_invalid")
	var n int
	require.NoError(t, rig.DB.QueryRow(
		`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, userID,
	).Scan(&n))
	assert.Zero(t, n, "new_user-branch token must not create an SSO identity row")
}

// ===========================================================================
// Apple SSO — post-#974 the Callback path is 410-gated; the former
// Callback-driven Apple tests live on as the AppleSession matrix in
// apple_session_integration_test.go. Only the /login short-circuit test
// remains here (it seeds the DB directly and never touches Callback).
// ===========================================================================

// TestHandler_Apple_LoginShortCircuit covers the /login endpoint's SSO-only
// short-circuit for an account that has only an Apple identity row linked.
// Mirrors TestLogin_PasswordDisabled_ReturnsAccountUsesSSO (auth package),
// which exercises the same path with a Google identity. We assert the
// providers list contains "apple" (regardless of any other providers the
// account might have, this is a contains-check, not an equality-check).
//
// Note: /login lives in internal/auth, NOT internal/oauth. This test uses
// testhelpers.SetupTestServer to wire the full app router, mirroring how
// internal/auth/handlers_integration_test.go sets up its short-circuit
// tests. The oauth_test package can import testhelpers without creating a
// cycle.
func TestHandler_Apple_LoginShortCircuit(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "ssoonlyapple")

	// Flip the SSO-only flag and link an Apple identity.
	_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'apple', 'apple-sub-shortcircuit', $2)`, user.ID, user.Email)
	require.NoError(t, err)

	w := ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    user.Email,
		"password": "any-password-the-user-types", //nolint:gosec // test credential, irrelevant
	}, nil)

	require.Equal(t, http.StatusForbidden, w.Code,
		"password_login_disabled + Apple-only identity must surface 403 account_uses_sso")
	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "account_uses_sso", resp["error_code"])
	providers, ok := resp["providers"].([]interface{})
	require.True(t, ok, "providers field should be a JSON array")
	assert.Contains(t, providers, "apple",
		"providers list must include 'apple' when an Apple identity is the only one linked")
}

// ===========================================================================
// Cloudflare KV bridge tests (#973)
// ===========================================================================

// recordingPutter implements the CFKV seam, capturing calls.
type recordingPutter struct {
	mu    sync.Mutex
	calls []putterCall
	err   error
}
type putterCall struct {
	Key, Value string
	TTL        int
}

func (r *recordingPutter) Put(_ context.Context, key, value string, ttl int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, putterCall{key, value, ttl})
	return r.err
}

// newSSOTestRigWithCFKV mirrors newSSOTestRig but registers BOTH google and
// apple fake providers (Initiate only calls AuthorizationURL + Name on the
// provider, so the registry_test.go fake suffices) and wires the CFKV seam.
// Also returns the rig's Redis client so tests can assert on state keys.
func newSSOTestRigWithCFKV(t *testing.T, putter oauth.StatePortPutter) (*gin.Engine, *redis.Client) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	registry := oauth.NewRegistry()
	registry.Register(&fakeProvider{name: "google"})
	registry.Register(&fakeProvider{name: "apple"})

	h := oauth.NewHandler(oauth.HandlerDeps{
		Registry: registry,
		Redis:    rdb,
		CFKV:     putter,
	})
	r := gin.New()
	g := r.Group("/api/v1/auth/sso")
	g.GET("/:provider", h.Initiate)
	return r, rdb
}

// initiateRequest performs GET /api/v1/auth/sso/<provider>?redirect_uri=<uri>
// — the same request-building shape as TestInitiate_StoresStateInRedis.
// All providers (apple + google after #974/#975) require a syntactically valid
// S256 code_challenge: the client-driven PKCE contract rejects initiates
// without one, and the #973 bridge tests exercise behavior downstream of that gate.
func initiateRequest(t *testing.T, r *gin.Engine, provider, redirectURI string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/auth/sso/" + provider + "?redirect_uri=" + url.QueryEscape(redirectURI) +
		"&code_challenge=" + strings.Repeat("c", 43)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestInitiate_AppleWritesBridgeMapping pins the #973 contract: apple
// initiates publish state→port to the bridge KV with the stateTTL-aligned
// TTL; google initiates do not touch the bridge; bridge failure is loud.
func TestInitiate_AppleWritesBridgeMapping(t *testing.T) {
	putter := &recordingPutter{}
	rig, _ := newSSOTestRigWithCFKV(t, putter)

	w := initiateRequest(t, rig, "apple", "http://127.0.0.1:51620/oauth/callback")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, putter.calls, 1)
	assert.Equal(t, resp.State, putter.calls[0].Key)
	assert.Equal(t, "51620", putter.calls[0].Value)
	assert.Equal(t, 600, putter.calls[0].TTL)
}

func TestInitiate_GoogleDoesNotTouchBridge(t *testing.T) {
	putter := &recordingPutter{}
	rig, _ := newSSOTestRigWithCFKV(t, putter)
	w := initiateRequest(t, rig, "google", "http://127.0.0.1:51620/oauth/callback")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, putter.calls)
}

func TestInitiate_BridgeFailureIsLoud(t *testing.T) {
	putter := &recordingPutter{err: fmt.Errorf("cf down")}
	rig, _ := newSSOTestRigWithCFKV(t, putter)
	w := initiateRequest(t, rig, "apple", "http://127.0.0.1:51620/oauth/callback")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "bridge_unavailable")
}

func TestInitiate_NilBridgeIsNoop(t *testing.T) {
	rig, _ := newSSOTestRigWithCFKV(t, nil)
	w := initiateRequest(t, rig, "apple", "http://127.0.0.1:51620/oauth/callback")
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestInitiate_ApplePortlessRedirectRejected pins the bridge precondition:
// validateLoopbackRedirect tolerates a portless loopback URI, but the bridge
// cannot relay without an explicit port — Initiate must fail fast with 400
// rather than let the flow die minutes later at the Worker (spec F3).
func TestInitiate_ApplePortlessRedirectRejected(t *testing.T) {
	putter := &recordingPutter{}
	rig, rdb := newSSOTestRigWithCFKV(t, putter)

	w := initiateRequest(t, rig, "apple", "http://127.0.0.1/oauth/callback")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_redirect_uri")
	assert.Empty(t, putter.calls, "no KV write may happen for a rejected redirect")

	// The precondition runs BEFORE the state record is written: a rejected
	// redirect must not orphan an sso_state entry (the rig's Redis is a
	// dedicated, flushed test DB, so any key here came from this request).
	keys, err := rdb.Keys(context.Background(), "sso_state:*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys, "rejected redirect must not orphan an sso_state record")
}

// TestInitiate_EmbedsStateInRecord pins the #972 contract: the sso_state
// record carries its own state value so the broker endpoint can perform a
// constant-time equality check against a server-held copy (spec F3).
func TestInitiate_EmbedsStateInRecord(t *testing.T) {
	r, rdb, _ := newSSOTestRig(t)

	// After #975 google is client-driven — code_challenge is required.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/sso/google?redirect_uri="+url.QueryEscape("http://127.0.0.1:51620/oauth/callback")+
			"&code_challenge="+strings.Repeat("e", 43), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.State)

	raw, err := rdb.Get(context.Background(), "sso_state:"+resp.State).Bytes()
	require.NoError(t, err)
	var rec struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, resp.State, rec.State, "record must embed its own state value")
}
