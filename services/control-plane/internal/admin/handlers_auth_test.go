package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// adminTestPassword is a strong throwaway password meeting ValidatePasswordStrength.
const adminTestPassword = "Adm1n-Str0ng-P@ssw0rd!" // #nosec G101 -- test fixture, not a real credential // pragma: allowlist secret

// authHandlerCfg returns an admin config wired to the virtual-authenticator RP.
func authHandlerCfg() *config.Config {
	return &config.Config{
		AdminWebAuthnRPID:           testAdminRPID,
		AdminWebAuthnRPOrigins:      []string{testAdminOrigin},
		AdminWebAuthnAllowedAAGUIDs: []string{testAllowedAAGID},
	}
}

// enrolledAdmin creates an active admin with one registered virtual-authenticator
// credential and returns the admin id, username, and the virtual authenticator
// (whose key is needed to sign login assertions).
func enrolledAdmin(t *testing.T, db *sql.DB, _ *redis.Client) (adminID, username string, va *virtualAuthenticator) {
	t.Helper()
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)

	svc, err := admin.NewAdminWebAuthn(authHandlerCfg())
	require.NoError(t, err)

	hash, err := auth.HashPassword(adminTestPassword)
	require.NoError(t, err)

	username = uniqueAdminUsername("auth")
	created, err := repo.CreatePending(ctx, username, hash)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)

	va = newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)

	_, err = admin.FinishAdminRegistration(ctx, svc, repo, admin.AdminRegistrationInput{
		User: user, Session: *session, Request: httpReq(regBody),
		AdminID: created.ID, AllowedAAGUIDs: []string{testAllowedAAGID}, CredentialName: "primary",
	})
	require.NoError(t, err)
	require.NoError(t, repo.SetStatus(ctx, created.ID, admin.StatusActive))

	return created.ID, username, va
}

// postJSON issues an in-process POST with a JSON body and returns the recorder.
func postJSON(engine *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	return rec
}

// adminAuthEngine wires PasswordLogin / WebAuthnLogin / Logout on a bare engine.
func adminAuthEngine(t *testing.T, db *sql.DB, rdb *redis.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	r := gin.New()
	r.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	r.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)
	r.POST("/admin/api/v1/auth/logout", h.Logout)
	return r
}

func TestPasswordLogin_HappyPath_ReturnsChallengeNoCookie(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, _ := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	rec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Handle, "password step must return a challenge handle")
	require.NotNil(t, resp.PublicKey, "password step must return assertion options")

	// CRITICAL: the password step must NEVER set a session cookie.
	assert.Empty(t, rec.Header().Get("Set-Cookie"), "password alone must not mint a session")
}

func TestPasswordLogin_WrongPassword_GenericUnauthorized(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, _ := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	rec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": "wrong-password",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, rec.Header().Get("Set-Cookie"))
}

// TestPasswordLogin_UnknownUser_SameAsWrongPassword asserts username-enumeration
// safety: an unknown username and a known-username-wrong-password produce the
// SAME status code and body shape.
func TestPasswordLogin_UnknownUser_SameAsWrongPassword(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, _ := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	unknown := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": uniqueAdminUsername("ghost"),
		"password": adminTestPassword,
	})
	wrongPw := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": "definitely-wrong",
	})

	assert.Equal(t, http.StatusUnauthorized, unknown.Code)
	assert.Equal(t, http.StatusUnauthorized, wrongPw.Code)
	assert.JSONEq(t, wrongPw.Body.String(), unknown.Body.String(),
		"unknown-user and wrong-password responses must be indistinguishable")
}

// TestWebAuthnLogin_HappyPath_MintsSession drives the full two-step ceremony and
// asserts a session cookie is set only after the WebAuthn step.
func TestWebAuthnLogin_HappyPath_MintsSession(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	// Step 1: password.
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	// Step 2: sign the returned challenge with the virtual authenticator.
	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusOK, waRec.Code, waRec.Body.String())

	setCookie := waRec.Header().Get("Set-Cookie")
	require.NotEmpty(t, setCookie, "WebAuthn step must mint a session cookie")
	assert.Contains(t, setCookie, "__Host-cv_admin_sid=")
	assert.Contains(t, setCookie, "HttpOnly")

	// The advanced authenticator sign count must be persisted (Gitar #1703 fix):
	// clone-detection is defeated if the stored counter never moves.
	var storedCount int64
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&storedCount))
	assert.Equal(t, int64(va.signCount), storedCount, "login must persist the advanced sign count")
}

// TestWebAuthnLogin_LockedAfterHandleIssued_Returns429 covers the Gitar #1703 fix:
// the WebAuthn step re-checks lockout, so a handle issued before the account was
// locked cannot be redeemed once the lock engages.
func TestWebAuthnLogin_LockedAfterHandleIssued_Returns429(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)
	ctx := context.Background()

	// Step 1: password step succeeds and issues a valid handle (not yet locked).
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	// Lock the account (per-account axis) AFTER the handle was issued.
	lk := admin.NewLockout(rdb, nil)
	for i := 0; i < 6; i++ {
		require.NoError(t, lk.RecordFailure(ctx, username, "203.0.113.9"))
	}

	// Step 2: the held handle must NOT be redeemable while locked → 429, no session.
	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusTooManyRequests, waRec.Code, waRec.Body.String())
	assert.Empty(t, waRec.Header().Get("Set-Cookie"), "locked WebAuthn step must not mint a session")
}

// TestWebAuthnLogin_BadAssertion_NoSession asserts a tampered assertion is
// rejected and no session is minted.
func TestWebAuthnLogin_BadAssertion_NoSession(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code)
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	va.signCount++
	// tamperSig=true corrupts the ECDSA signature.
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, true)

	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	assert.Equal(t, http.StatusUnauthorized, waRec.Code)
	assert.Empty(t, waRec.Header().Get("Set-Cookie"))
}

// TestWebAuthnLogin_UnknownHandle_Rejected asserts a fabricated/expired handle is
// rejected without touching the authenticator.
func TestWebAuthnLogin_UnknownHandle_Rejected(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, _, _ = enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    "0000000000000000000000000000000000000000000000000000000000000000",
		"assertion": json.RawMessage(`{}`),
	})
	assert.Equal(t, http.StatusUnauthorized, waRec.Code)
	assert.Empty(t, waRec.Header().Get("Set-Cookie"))
}

// TestWebAuthnLogin_HandleIsSingleUse asserts the challenge handle cannot be
// replayed after a successful login.
func TestWebAuthnLogin_HandleIsSingleUse(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code)
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

	first := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusOK, first.Code)

	// Replaying the same handle must fail (GETDEL consumed it).
	second := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	assert.Equal(t, http.StatusUnauthorized, second.Code)
}

func TestLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	// Full login to obtain a real session cookie.
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code)
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))
	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusOK, waRec.Code)

	sid := sessionIDFromSetCookie(t, waRec.Header().Get("Set-Cookie"))

	// Logout carrying the session cookie.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Set-Cookie"), "Max-Age=0")

	// The session is gone: AdminAuthRequired now rejects it.
	store := admin.NewSessionStore(rdb, nil)
	_, err := store.Get(context.Background(), sid)
	assert.ErrorIs(t, err, admin.ErrSessionInvalid)
}

// sessionIDFromSetCookie extracts the __Host-cv_admin_sid value from a Set-Cookie
// header.
func sessionIDFromSetCookie(t *testing.T, setCookie string) string {
	t.Helper()
	require.NotEmpty(t, setCookie)
	header := http.Header{}
	header.Add("Set-Cookie", setCookie)
	resp := http.Response{Header: header}
	for _, ck := range resp.Cookies() {
		if ck.Name == "__Host-cv_admin_sid" {
			return ck.Value
		}
	}
	t.Fatalf("no __Host-cv_admin_sid in Set-Cookie: %q", setCookie)
	return ""
}
