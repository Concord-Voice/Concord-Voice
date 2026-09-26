package users_test

// Regression for the WebAuthn inline-token purpose binding (RS11, #3453): the
// E2EE key reset and the password change must not share a purpose.
//
// PUT /api/v1/users/me/keys (ReplaceMyKeys, the destructive E2EE identity
// reset) and POST /api/v1/users/me/password (ChangePassword) both verify their
// MFA leg through verifyStepUpWithLockedUser. That helper hard-coded
// stepup.PurposePasswordChange, so a WebAuthn inline token the user minted to
// change their password also reset their E2EE identity. One purpose per ROUTE
// is the invariant stepup/purpose.go states; two routes sharing one is the
// defect it names.
//
// Every token here comes from a REAL begin+finish ceremony against the
// production *mfa.Handler (a software P-256 authenticator), and both routes
// run through a production users.Handler wired with that same *mfa.Handler as
// its verifier, which is router.go's wiring.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const krKeyringHex = "0101010101010101010101010101010101010101010101010101010101010101"

type krAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	count  uint32
}

// krNewAuthenticator registers a software P-256 credential for userID
// (same construction as mfa's sfNewAuthenticator).
func krNewAuthenticator(t *testing.T, db *sql.DB, userID string) *krAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := key.PublicKey.ECDH()
	require.NoError(t, err)
	point := pub.Bytes()
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         int64(webauthncose.P256),
		XCoord:        point[1:33],
		YCoord:        point[33:65],
	})
	require.NoError(t, err)
	credID := []byte("kr-credential-" + userID)
	_, err = db.Exec(`
		INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'KR Software Key', 'hardware', $4, 0, NOW())
	`, uuid.New().String(), userID, credID, cose)
	require.NoError(t, err)
	return &krAuthenticator{key: key, credID: credID}
}

func (a *krAuthenticator) assertion(t *testing.T, challenge string) string {
	t.Helper()
	a.count++
	clientData, err := json.Marshal(map[string]any{
		"type": "webauthn.get", "challenge": challenge, "origin": "https://webauthn.io", "crossOrigin": false,
	})
	require.NoError(t, err)
	rpID := sha256.Sum256([]byte("webauthn.io"))
	authData := binary.BigEndian.AppendUint32(append(rpID[:], 0x05), a.count)
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	require.NoError(t, err)
	b64 := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf(`{"id":%q,"rawId":%q,"type":"public-key","response":{"authenticatorData":%q,"clientDataJSON":%q,"signature":%q}}`,
		b64(a.credID), b64(a.credID), b64(authData), b64(clientData), b64(sig))
}

func krMFAHandler(t *testing.T, db *sql.DB, rdb *redis.Client) *mfa.Handler {
	t.Helper()
	svc, err := mfa.NewWebAuthnService("webauthn.io", "test", []string{"https://webauthn.io"})
	require.NoError(t, err)
	kr, err := mfa.ParseKeyring(krKeyringHex, 1, "")
	require.NoError(t, err)
	return mfa.NewHandler(db, rdb, logger.NewWithWriter(io.Discard), kr, "test", svc, "test")
}

func krGin(method, path, body, userID string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID)
	return c, w
}

// krMint runs a real inline ceremony: begin {"purpose": purpose}, sign the
// stored challenge, finish. Returns the minted mfa_token.
func krMint(t *testing.T, mh *mfa.Handler, rdb *redis.Client, a *krAuthenticator, userID, purpose string) string {
	t.Helper()
	c, w := krGin(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/begin", fmt.Sprintf(`{"purpose":%q}`, purpose), userID)
	mh.WebAuthnVerifyInlineBegin(c)
	require.Equal(t, http.StatusOK, w.Code, "begin(%s): %s", purpose, w.Body.String())
	raw, err := rdb.Get(context.Background(), "mfa_inline_purpose_session:"+userID).Bytes()
	require.NoError(t, err)
	var session webauthn.SessionData
	require.NoError(t, json.Unmarshal(raw, &session))
	c2, w2 := krGin(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/finish", a.assertion(t, session.Challenge), userID)
	mh.WebAuthnVerifyInlineFinish(c2)
	require.Equal(t, http.StatusOK, w2.Code, "finish(%s): %s", purpose, w2.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &out))
	token, _ := out["mfa_token"].(string)
	require.NotEmpty(t, token)
	return token
}

func krChangePassword(t *testing.T, h *users.Handler, user testhelpers.TestUser, mfaCode string) *httptest.ResponseRecorder {
	t.Helper()
	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	body, err := json.Marshal(map[string]any{
		"current_password":    user.Password,
		"new_password":        "AnotherSecurePass456!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"mfa_code":            mfaCode,
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", user.ID)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/users/me/password", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ChangePassword(c)
	return w
}

func krPasswordHash(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	var hash string
	require.NoError(t, db.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&hash))
	return hash
}

func krReplaceKeys(t *testing.T, h *users.Handler, user testhelpers.TestUser, mfaCode string) *httptest.ResponseRecorder {
	t.Helper()
	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	body, err := json.Marshal(map[string]any{
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            publicKey,
		"acknowledge_data_loss": true,
		"current_password":      user.Password,
		"mfa_code":              mfaCode,
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", user.ID)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/users/me/keys", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ReplaceMyKeys(c)
	return w
}

func krKeyVersion(t *testing.T, db *sql.DB, userID string) int {
	t.Helper()
	var v int
	require.NoError(t, db.QueryRow(`SELECT key_version FROM public_keys WHERE user_id = $1`, userID).Scan(&v))
	return v
}

func krKeyResetFixture(t *testing.T, name string) (*testhelpers.TestServer, testhelpers.TestUser, *krAuthenticator, *mfa.Handler, *users.Handler) {
	t.Helper()
	ts := setupTS(t)
	user := ts.CreateTestUser(t, name)
	a := krNewAuthenticator(t, ts.DB, user.ID)
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = true, mfa_methods = ARRAY['webauthn'] WHERE id = $1`, user.ID)
	require.NoError(t, err)
	mh := krMFAHandler(t, ts.DB, ts.Redis)
	// router.go wiring: the users handler's MFAVerifier is the *mfa.Handler.
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, mh, nil, testCredFence(t, ts.DB), nil)
	service := presencehistory.NewService(ts.DB, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(immediatePresenceDelivery{}))
	h.SetPresenceHistory(service)
	return ts, user, a, mh, h
}

const (
	// Wire values, spelled out so the test does not agree with a renamed
	// constant: the desktop sends these strings.
	krPurposeKeyReset       = "account.e2ee_key_reset"
	krPurposePasswordChange = "account.password_change" // pragma: allowlist secret
)

// Control: the key reset's MFA gate is live. A token minted for an unrelated
// purpose is refused as an invalid code, and the keys are untouched.
func TestKeyResetInlinePurpose_UnrelatedPurposeRefused(t *testing.T) {
	ts, user, a, mh, h := krKeyResetFixture(t, "krunrelated")
	before := krKeyVersion(t, ts.DB, user.ID)

	token := krMint(t, mh, ts.Redis, a, user.ID, "mfa_settings.backup_email_set")
	w := krReplaceKeys(t, h, user, token)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid MFA code")
	assert.Equal(t, before, krKeyVersion(t, ts.DB, user.ID), "a refused step-up must not reset the E2EE identity")
}

// A token minted to change the password must not reset the E2EE identity.
func TestKeyResetInlinePurpose_RefusesAPasswordChangeToken(t *testing.T) {
	ts, user, a, mh, h := krKeyResetFixture(t, "krpwdtoken")
	before := krKeyVersion(t, ts.DB, user.ID)

	token := krMint(t, mh, ts.Redis, a, user.ID, krPurposePasswordChange)
	w := krReplaceKeys(t, h, user, token)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"a WebAuthn inline token minted for %s was accepted by the E2EE key reset: %s", krPurposePasswordChange, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid MFA code", "refused exactly like an invalid code")
	assert.Equal(t, before, krKeyVersion(t, ts.DB, user.ID),
		"public_keys.key_version advanced: the E2EE identity was reset with a password-change token")

	// The refusal did not consume the token: it still changes the password.
	w = krChangePassword(t, h, user, token)
	assert.Equal(t, http.StatusOK, w.Code, "a refused token must survive for its own route: %s", w.Body.String())
}

// The reverse direction: a token minted for the key reset must not change the
// password, and the key reset accepts its own token.
func TestKeyResetInlinePurpose_OwnPurposeAcceptedAndNotSpendableOnPasswordChange(t *testing.T) {
	ts, user, a, mh, h := krKeyResetFixture(t, "krowntoken")
	hashBefore := krPasswordHash(t, ts.DB, user.ID)
	versionBefore := krKeyVersion(t, ts.DB, user.ID)

	token := krMint(t, mh, ts.Redis, a, user.ID, krPurposeKeyReset)
	w := krChangePassword(t, h, user, token)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"a WebAuthn inline token minted for %s was accepted by the password change: %s", krPurposeKeyReset, w.Body.String())
	assert.Equal(t, hashBefore, krPasswordHash(t, ts.DB, user.ID), "a refused step-up must not change the password")

	w = krReplaceKeys(t, h, user, token)
	require.Equal(t, http.StatusOK, w.Code, "the key reset must accept its own purpose: %s", w.Body.String())
	assert.Greater(t, krKeyVersion(t, ts.DB, user.ID), versionBefore, "the key reset ran")
}
