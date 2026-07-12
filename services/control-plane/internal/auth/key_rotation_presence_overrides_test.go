package auth_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRecoveryPresenceOverrideState(
	t *testing.T,
	ts *testhelpers.TestServer,
	senderID string,
	targetID string,
) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text, custom_text_emoji)
		 VALUES ($1, 2, 'private recovery status', '🔐')
		 ON CONFLICT (user_id) DO UPDATE
		 SET custom_text_tier = 2, custom_text = 'private recovery status', custom_text_emoji = '🔐'`,
		senderID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data, version)
		 VALUES ($1, 'custom_text', 'unrecoverable-preference-key-ciphertext', 3)`,
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

func assertRecoveryPresenceOverrideReset(
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
	assert.Equal(t, 0, preferenceCount, "unrecoverable preference ciphertext must be removed")
	assert.Equal(t, 0, targetCount, "materialized recipient exceptions must reset with ciphertext")
	assert.Equal(t, 0, customTextTier, "Custom Status visibility must fail closed after recovery")
	assert.Nil(t, customText, "recovery must not leave plaintext Custom Status text to resurrect")
	assert.Nil(t, customTextEmoji, "recovery must not leave a Custom Status emoji to resurrect")
}

func TestRecoveryResetPasswordResetsPresenceOverridesFailClosed(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverpresencepassword")
	target := ts.CreateTestUser(t, "recoverpresencepasswordtarget")
	seedRecoveryPresenceOverrideState(t, ts, user.ID, target.ID)
	recoveryToken := getRecoveryToken2(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPassword2,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  "argon2id",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	assertRecoveryPresenceOverrideReset(t, ts, user.ID)
}

func TestRecoveryResetAccountResetsPresenceOverridesFailClosed(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverpresenceaccount")
	target := ts.CreateTestUser(t, "recoverpresenceaccounttarget")
	seedRecoveryPresenceOverrideState(t, ts, user.ID, target.ID)
	recoveryToken := getRecoveryToken2(t, ts, user)
	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPassword2,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            publicKey,
		"acknowledge_data_loss": true,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	assertRecoveryPresenceOverrideReset(t, ts, user.ID)
}
