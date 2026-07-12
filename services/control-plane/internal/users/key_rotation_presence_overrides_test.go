package users_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedPresenceOverrideStateForKeyRotation(
	t *testing.T,
	ts *testhelpers.TestServer,
	senderID string,
	targetID string,
) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text, custom_text_emoji)
		 VALUES ($1, 2, 'private status', '🔒')
		 ON CONFLICT (user_id) DO UPDATE
		 SET custom_text_tier = 2, custom_text = 'private status', custom_text_emoji = '🔒'`,
		senderID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data, version)
		 VALUES ($1, 'custom_text', 'old-preference-key-ciphertext', 4)`,
		senderID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		 VALUES ($1, 'custom_text', $2)`,
		senderID,
		targetID,
	)
	require.NoError(t, err)
}

func assertPresenceOverrideStateResetFailClosed(
	t *testing.T,
	ts *testhelpers.TestServer,
	senderID string,
) {
	t.Helper()
	var preferenceCount, targetCount, customTextTier int
	var customText, customTextEmoji *string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`,
		senderID,
	).Scan(&preferenceCount))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM user_presence_overrides WHERE sender_id = $1`,
		senderID,
	).Scan(&targetCount))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT custom_text_tier, custom_text, custom_text_emoji
		 FROM user_presence_settings WHERE user_id = $1`,
		senderID,
	).Scan(&customTextTier, &customText, &customTextEmoji))
	assert.Equal(t, 0, preferenceCount, "old preference-key ciphertext must be removed")
	assert.Equal(t, 0, targetCount, "materialized recipient exceptions must reset with ciphertext")
	assert.Equal(t, 0, customTextTier, "Custom Status visibility must fail closed after key reset")
	assert.Nil(t, customText, "key reset must not leave plaintext Custom Status text to resurrect")
	assert.Nil(t, customTextEmoji, "key reset must not leave a Custom Status emoji to resurrect")
}

func TestReplaceMyKeysResetsPresenceOverridesFailClosed(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacepresencereset")
	target := ts.CreateTestUser(t, "replacepresencetarget")
	seedPresenceOverrideStateForKeyRotation(t, ts, user.ID, target.ID)

	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"public_key":            pubKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	assertPresenceOverrideStateResetFailClosed(t, ts, user.ID)
}

func TestReplaceMyKeysRejectsPasswordThatBecameStaleWhileWaitingForUserLock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacekeysstaleauth")
	const recoveredPassword = "RecoveredPassword789!" // pragma: allowlist secret
	recoveredHash, err := auth.HashPassword(recoveredPassword)
	require.NoError(t, err)

	var originalPublicKey []byte
	var originalKeyVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT public_key, key_version FROM public_keys WHERE user_id = $1`,
		user.ID,
	).Scan(&originalPublicKey, &originalKeyVersion))

	blocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.Exec(
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`,
		user.ID,
	)
	require.NoError(t, err)
	_, err = blocker.Exec(
		`SELECT user_id FROM user_keys WHERE user_id = $1 FOR UPDATE`,
		user.ID,
	)
	require.NoError(t, err)

	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{
			keyWrappedPrivateKey:    wrappedKey,
			keyKeyDerivationSalt:    salt,
			"public_key":            publicKey,
			"acknowledge_data_loss": true,
			keyCurrentPassword:      user.Password,
		}, testhelpers.AuthHeaders(user.AccessToken))
	}()

	require.Eventually(t, func() bool {
		var waiting int
		queryErr := ts.DB.QueryRow(`
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND (
				query LIKE '%UPDATE user_keys SET wrapped_private_key%'
				OR query LIKE '%SELECT password_hash FROM users WHERE id = $1 FOR NO KEY UPDATE%'
			  )
		`).Scan(&waiting)
		return queryErr == nil && waiting > 0
	}, 10*time.Second, 25*time.Millisecond,
		"key-replacement request never reached a serialized database write")

	_, err = blocker.Exec(
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
		recoveredHash,
		user.ID,
	)
	require.NoError(t, err)
	require.NoError(t, blocker.Commit())

	select {
	case w := <-response:
		assert.Equal(t, http.StatusUnauthorized, w.Code,
			"authorization with a superseded password must not reset E2EE identity")
	case <-time.After(10 * time.Second):
		t.Fatal("key-replacement request did not complete after releasing database locks")
	}

	var persistedPublicKey []byte
	var persistedKeyVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT public_key, key_version FROM public_keys WHERE user_id = $1`,
		user.ID,
	).Scan(&persistedPublicKey, &persistedKeyVersion))
	assert.Equal(t, originalPublicKey, persistedPublicKey)
	assert.Equal(t, originalKeyVersion, persistedKeyVersion)
}

func TestChangePasswordAtomicallyRotatesPresenceOverrideCiphertext(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "changepresenceatomic")
	target := ts.CreateTestUser(t, "changepresenceatomictarget")
	seedPresenceOverrideStateForKeyRotation(t, ts, user.ID, target.ID)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	const newPassword = "NewPassword456!"                            // pragma: allowlist secret
	const newCiphertext = "bmV3LXByZWZlcmVuY2Uta2V5LWNpcGhlcnRleHQ=" // pragma: allowlist secret

	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       newPassword,
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
		"key_derivation_alg": "argon2id",
		"presence_override": map[string]interface{}{
			"encrypted_data":   newCiphertext,
			"expected_version": 4,
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		PresenceOverrideVersion *int `json:"presence_override_version"`
	}
	testhelpers.ParseJSON(t, w, &body)
	require.NotNil(t, body.PresenceOverrideVersion)
	assert.Equal(t, 5, *body.PresenceOverrideVersion)

	var ciphertext, passwordHash string
	var version, targetCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT encrypted_data, version FROM presence_override_preferences
		 WHERE user_id = $1 AND category = 'custom_text'`,
		user.ID,
	).Scan(&ciphertext, &version))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM user_presence_overrides
		 WHERE sender_id = $1 AND category = 'custom_text' AND target_user_id = $2`,
		user.ID,
		target.ID,
	).Scan(&targetCount))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`,
		user.ID,
	).Scan(&passwordHash))
	passwordMatches, err := auth.VerifyPassword(newPassword, passwordHash)
	require.NoError(t, err)
	assert.True(t, passwordMatches)
	assert.Equal(t, newCiphertext, ciphertext)
	assert.Equal(t, 5, version)
	assert.Equal(t, 1, targetCount, "password rotation must not alter materialized recipients")
}

func TestChangePasswordPresenceOverrideConflictRollsBackPasswordAndKeys(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "changepresenceconflict")
	target := ts.CreateTestUser(t, "changepresenceconflicttarget")
	seedPresenceOverrideStateForKeyRotation(t, ts, user.ID, target.ID)

	var originalHash string
	var originalWrappedKey, originalSalt []byte
	require.NoError(t, ts.DB.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`, user.ID,
	).Scan(&originalHash))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT wrapped_private_key, key_derivation_salt FROM user_keys WHERE user_id = $1`,
		user.ID,
	).Scan(&originalWrappedKey, &originalSalt))

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       "NewPassword456!", // pragma: allowlist secret
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
		"key_derivation_alg": "argon2id",
		"presence_override": map[string]interface{}{
			"encrypted_data":   "bmV3LXByZWZlcmVuY2Uta2V5LWNpcGhlcnRleHQ=", // pragma: allowlist secret
			"expected_version": 3,
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusConflict, w.Code)

	var conflictBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &conflictBody)
	assert.Equal(t, "presence_override_version_conflict", conflictBody["code"])
	assert.Equal(t, float64(4), conflictBody["current_version"])

	var passwordHash, ciphertext string
	var persistedWrappedKey, persistedSalt []byte
	var version, targetCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`, user.ID,
	).Scan(&passwordHash))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT wrapped_private_key, key_derivation_salt FROM user_keys WHERE user_id = $1`,
		user.ID,
	).Scan(&persistedWrappedKey, &persistedSalt))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT encrypted_data, version FROM presence_override_preferences
		 WHERE user_id = $1 AND category = 'custom_text'`,
		user.ID,
	).Scan(&ciphertext, &version))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM user_presence_overrides
		 WHERE sender_id = $1 AND category = 'custom_text'`,
		user.ID,
	).Scan(&targetCount))
	assert.Equal(t, originalHash, passwordHash)
	assert.Equal(t, originalWrappedKey, persistedWrappedKey)
	assert.Equal(t, originalSalt, persistedSalt)
	assert.Equal(t, "old-preference-key-ciphertext", ciphertext)
	assert.Equal(t, 4, version)
	assert.Equal(t, 1, targetCount)
}

func TestChangePasswordAbsentPresenceOverrideReturnsVersionZero(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "changepresenceabsent")
	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       "NewPassword456!", // pragma: allowlist secret
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
		"key_derivation_alg": "argon2id",
		"presence_override": map[string]interface{}{
			"encrypted_data":   "ZW1wdHktcHJlc2VuY2Utb3ZlcnJpZGU=", // pragma: allowlist secret
			"expected_version": 0,
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		PresenceOverrideVersion *int `json:"presence_override_version"`
	}
	testhelpers.ParseJSON(t, w, &body)
	require.NotNil(t, body.PresenceOverrideVersion)
	assert.Zero(t, *body.PresenceOverrideVersion)

	var preferenceCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`,
		user.ID,
	).Scan(&preferenceCount))
	assert.Zero(t, preferenceCount, "password rotation must not create an empty override row")
}

func TestChangePasswordRejectsPasswordThatBecameStaleWhileWaitingForUserLock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "changepasswordstaleauth")
	const recoveredPassword = "RecoveredPassword789!" // pragma: allowlist secret
	recoveredHash, err := auth.HashPassword(recoveredPassword)
	require.NoError(t, err)

	blocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.Exec(
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`,
		user.ID,
	)
	require.NoError(t, err)

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
			keyCurrentPassword:   user.Password,
			keyNewPassword:       "NewPassword456!", // pragma: allowlist secret
			keyWrappedPrivateKey: wrappedKey,
			keyKeyDerivationSalt: salt,
			"key_derivation_alg": "argon2id",
			"presence_override": map[string]interface{}{
				"encrypted_data":   "ZW1wdHktcHJlc2VuY2Utb3ZlcnJpZGU=", // pragma: allowlist secret
				"expected_version": 0,
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
	}()

	require.Eventually(t, func() bool {
		var waiting int
		queryErr := ts.DB.QueryRow(`
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND (
				query LIKE '%SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE%'
				OR query LIKE '%SELECT password_hash FROM users WHERE id = $1 FOR NO KEY UPDATE%'
			  )
		`).Scan(&waiting)
		return queryErr == nil && waiting > 0
	}, 10*time.Second, 25*time.Millisecond,
		"password-change request never reached the serialized user-row lock")

	_, err = blocker.Exec(
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
		recoveredHash,
		user.ID,
	)
	require.NoError(t, err)
	require.NoError(t, blocker.Commit())

	select {
	case w := <-response:
		assert.Equal(t, http.StatusUnauthorized, w.Code,
			"authorization with a superseded password must not overwrite recovered credentials")
	case <-time.After(10 * time.Second):
		t.Fatal("password-change request did not complete after releasing user-row lock")
	}

	var persistedHash string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`,
		user.ID,
	).Scan(&persistedHash))
	matchesRecoveredPassword, err := auth.VerifyPassword(recoveredPassword, persistedHash)
	require.NoError(t, err)
	assert.True(t, matchesRecoveredPassword,
		"stale authorization must not replace credentials committed by recovery")
}

func TestChangePasswordMFAReauthenticationUsesLockedTransactionConnection(t *testing.T) {
	// SetupTestDB's first package-level call runs migrations through a driver
	// that retains one pool connection. Open a second pool after that one-time
	// bootstrap so MaxOpenConns(1) measures only the request under test.
	_, _ = testhelpers.SetupTestDB(t)
	db, _ := testhelpers.SetupTestDB(t)
	require.Zero(t, db.Stats().InUse)
	ts := &testhelpers.TestServer{DB: db}
	user := ts.CreateTestUser(t, "changepasswordsingleconnmfa")
	_, err := ts.DB.Exec(
		`UPDATE users
		 SET mfa_enabled = TRUE, mfa_methods = ARRAY['totp']::TEXT[]
		 WHERE id = $1`,
		user.ID,
	)
	require.NoError(t, err)
	encryptionKey := make([]byte, 32)
	totpKey, err := mfa.GenerateSecret(user.Email)
	require.NoError(t, err)
	secretCiphertext, secretNonce, err := mfa.EncryptSecret(
		[]byte(totpKey.Secret()),
		encryptionKey,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO user_mfa_totp (
			user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed
		 ) VALUES ($1, $2, $3, TRUE, TRUE)`,
		user.ID,
		secretCiphertext,
		secretNonce,
	)
	require.NoError(t, err)
	mfaCode, err := totp.GenerateCode(totpKey.Secret(), time.Now())
	require.NoError(t, err)
	var requestLogs bytes.Buffer
	requestLogger := logger.NewWithWriter(&requestLogs)
	mfaHandler := mfa.NewHandler(
		db,
		nil,
		requestLogger,
		encryptionKey,
		"test-secret",
		nil,
		"test",
	)
	handler := users.NewHandler(db, requestLogger, nil, mfaHandler, nil)

	// A password/key rotation already owns one transaction connection while it
	// re-checks MFA under the locked users row. Constraining the pool to one
	// connection proves that the re-check never tries to acquire a second one.
	ts.DB.SetMaxOpenConns(1)
	ts.DB.SetMaxIdleConns(1)
	t.Cleanup(func() {
		ts.DB.SetMaxOpenConns(5)
		ts.DB.SetMaxIdleConns(2)
	})

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	body, err := json.Marshal(map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       "NewPassword456!", // pragma: allowlist secret
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
		"key_derivation_alg": "argon2id",
		"mfa_code":           mfaCode,
		"presence_override": map[string]interface{}{
			"encrypted_data":   "ZW1wdHktcHJlc2VuY2Utb3ZlcnJpZGU=", // pragma: allowlist secret
			"expected_version": 0,
		},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, urlUsersMePassword, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", user.ID)
	handler.ChangePassword(c)

	require.Equal(t, http.StatusOK, w.Code, "%s\n%s", w.Body.String(), requestLogs.String())
}

type keyRotationCoordinator struct {
	calls    int
	senderID uuid.UUID
}

func (c *keyRotationCoordinator) WithSender(senderID uuid.UUID, fn func()) {
	c.calls++
	c.senderID = senderID
	fn()
}

type postCommitResetClearer struct {
	db                    *sql.DB
	calls                 int
	senderID              uuid.UUID
	preferenceCountAtCall int
	tierAtCall            int
}

func (c *postCommitResetClearer) ClearCustomTextForPresenceAudience(senderID uuid.UUID) {
	c.calls++
	c.senderID = senderID
	_ = c.db.QueryRow(
		`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`,
		senderID,
	).Scan(&c.preferenceCountAtCall)
	_ = c.db.QueryRow(
		`SELECT custom_text_tier FROM user_presence_settings WHERE user_id = $1`,
		senderID,
	).Scan(&c.tierAtCall)
}

func TestReplaceMyKeysCoordinatesAndClearsCustomTextAfterCommit(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacepresenceclear")
	target := ts.CreateTestUser(t, "replacepresencecleartarget")
	seedPresenceOverrideStateForKeyRotation(t, ts, user.ID, target.ID)
	handler := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil)
	coordinator := &keyRotationCoordinator{}
	clearer := &postCommitResetClearer{db: ts.DB}
	users.SetCustomStatusCoordinatorForTest(handler, coordinator)
	users.SetCustomTextResetBroadcasterForTest(handler, clearer)

	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	body, err := json.Marshal(map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            publicKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", user.ID)
	c.Request = httptest.NewRequest(http.MethodPut, urlUsersMeKeys, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReplaceMyKeys(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, coordinator.calls)
	assert.Equal(t, uuid.MustParse(user.ID), coordinator.senderID)
	assert.Equal(t, 1, clearer.calls)
	assert.Equal(t, uuid.MustParse(user.ID), clearer.senderID)
	assert.Zero(t, clearer.preferenceCountAtCall, "clear must run after reset transaction commits")
	assert.Zero(t, clearer.tierAtCall, "visibility must already be Off when clear is sent")
}
