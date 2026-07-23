package users_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2201: the destructive flows FAIL CLOSED (503) when the credential fence is
// not wired — a destructive credential flow must never run without epoch
// rotation.

func newFenceMissingContext(t *testing.T, uid string, body map[string]interface{}) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", uid)
	return c, w
}

func TestChangePassword_FenceMissingFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "fencemisscp")
	// nil credFence: the handler must 503 before touching credential state.
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
		"current_password":    user.Password,
		"new_password":        "AnotherSecurePass456!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
	})
	h.ChangePassword(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	// The password must be unchanged (the 503 fired before the mutation tx).
	var hash string
	require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&hash))
	assert.NotEmpty(t, hash)
}

func TestReplaceMyKeys_FenceMissingFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "fencemissrk")
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	h.SetPresenceHistory(ts.PresenceHistory)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
		"current_password":      user.Password,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            pub,
		"acknowledge_data_loss": true,
	})
	c.Request.Method = http.MethodPut
	h.ReplaceMyKeys(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	// Key material must be untouched (key_version still 1).
	var kv int
	require.NoError(t, ts.DB.QueryRow(`SELECT key_version FROM user_keys WHERE user_id = $1`, user.ID).Scan(&kv))
	assert.Equal(t, 1, kv)
}

// failingPairIssuer forces the continuation-mint failure branch.
type failingPairIssuer struct{}

func (failingPairIssuer) IssueTokenPair(_ *gin.Context, _, _ string) (string, string, string, error) {
	return "", "", "", assert.AnError
}

// #2201: a continuation-pair mint failure degrades to the re-login flow — the
// committed change still returns 200, just without token fields.
func TestChangePassword_ContinuationMintFailureDegrades(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "contmintfail")
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil,
		testCredFence(t, ts.DB), failingPairIssuer{})

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
		"current_password":    user.Password,
		"new_password":        "AnotherSecurePass456!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
	})
	h.ChangePassword(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "access_token",
		"mint failure must degrade to re-login, not fail the committed change")
}

// #2201: with no pair issuer wired at all, the committed change succeeds with
// the pre-#2201 response shape.
func TestChangePassword_NoPairIssuerStillSucceeds(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "contnilissuer")
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil,
		testCredFence(t, ts.DB), nil)

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
		"current_password":    user.Password,
		"new_password":        "AnotherSecurePass456!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
	})
	h.ChangePassword(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "access_token")
}

// #2201: a generic transaction failure (fault-injected by renaming user_keys)
// takes the definite-rollback path: 500, fence restored, password unchanged.
func TestChangePassword_GenericTxFailureRollsBackFence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "chpwtxfail")
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil,
		testCredFence(t, ts.DB), nil)

	var beforeHash string
	require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&beforeHash))

	_, err := ts.DB.Exec("ALTER TABLE user_keys RENAME TO user_keys_txfailtest")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE user_keys_txfailtest RENAME TO user_keys"); err != nil {
			t.Errorf("cleanup: failed to rename user_keys back: %v", err)
		}
	})

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
		"current_password":    user.Password,
		"new_password":        "AnotherSecurePass456!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
	})
	h.ChangePassword(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var afterHash string
	require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&afterHash))
	assert.Equal(t, beforeHash, afterHash, "password must be unchanged after rollback")
}
