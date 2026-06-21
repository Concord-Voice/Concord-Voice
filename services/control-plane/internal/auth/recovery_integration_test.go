package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	recoveryCodeKeyPrefix  = "recovery_code:"
	pathRecoveryBegin      = "/api/v1/auth/recovery/begin"
	pathRecoveryVerifyCode = "/api/v1/auth/recovery/verify-code"
	pathRecoveryResetPwd   = "/api/v1/auth/recovery/reset-password" //nolint:gosec // G101 false positive: URL path, not a credential
	pathRecoveryResetAcct  = "/api/v1/auth/recovery/reset-account"
	testRecoveryCode       = "123456"
	testNewPassword        = "NewSecurePassword123!" //nolint:gosec // G101 false positive: test fixture, not a real credential
	testKeyDerivationAlg   = "argon2id"
)

// seedRecoveryCode inserts a known recovery code into Redis so verify-code can find it.
func seedRecoveryCode(t *testing.T, ts *testhelpers.TestServer, email, code, userID string) {
	t.Helper()
	hash := sha256.Sum256([]byte(code))
	record := map[string]interface{}{
		"code_hash": hex.EncodeToString(hash[:]),
		"user_id":   userID,
		"attempts":  0,
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := recoveryCodeKeyPrefix + email
	err = ts.Redis.Set(context.Background(), key, data, 10*time.Minute).Err()
	require.NoError(t, err)
}

// --- RecoveryBegin Tests ---

func TestRecoveryBeginReturnsOKForExistingEmail(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveruser1")

	w := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": user.Email,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "If an account exists")
}

func TestRecoveryBeginReturnsOKForNonexistentEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": "nonexistent@test.concord.chat",
	}, nil)

	// Must return same 200 as existing email (anti-enumeration)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "If an account exists")
}

func TestRecoveryBeginStoresCodeInRedis(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveruser2")

	ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": user.Email,
	}, nil)

	// Verify a recovery code was stored in Redis
	key := recoveryCodeKeyPrefix + user.Email
	val, err := ts.Redis.Get(context.Background(), key).Result()
	require.NoError(t, err)
	assert.NotEmpty(t, val)

	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(val), &record))
	assert.NotEmpty(t, record["code_hash"])
	assert.Equal(t, user.ID, record["user_id"])
	assert.Equal(t, float64(0), record["attempts"])
}

// --- RecoveryVerifyCode Tests ---

func TestRecoveryVerifyCodeSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser1")

	// Seed a known code
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  testRecoveryCode,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["recovery_token"])
	// User has no recovery key by default
	assert.Equal(t, false, body["has_recovery_key"])
}

func TestRecoveryVerifyCodeWrongCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser2")

	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "999999",
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryVerifyCodeAttemptTracking(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser3")

	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	// 5 wrong attempts should exhaust the code
	for i := 0; i < 5; i++ {
		w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
			"email": user.Email,
			"code":  "000000",
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	}

	// Flush rate limit key so the 6th request reaches the handler
	// (route is rate-limited to 5 req/15min — we've used all 5 above)
	ts.Redis.Del(context.Background(), "ratelimit:ip:192.0.2.1:POST:"+pathRecoveryVerifyCode)

	// Even the correct code should now fail (max attempts exceeded)
	// Anti-enumeration: returns same 401 as wrong code
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  testRecoveryCode,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryVerifyCodeDeletesCodeOnSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser4")

	seedRecoveryCode(t, ts, user.Email, "654321", user.ID)

	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "654321",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Code should be deleted from Redis (single-use)
	key := recoveryCodeKeyPrefix + user.Email
	_, err := ts.Redis.Get(context.Background(), key).Result()
	assert.Error(t, err) // redis.Nil
}

// --- ResetPassword Token Single-Use Tests ---

func TestRecoveryResetPasswordTokenSingleUse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser1")

	// Get a recovery token
	seedRecoveryCode(t, ts, user.Email, "111111", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "111111",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	_ = pub // not needed for reset-password

	resetPayload := map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
	}

	// First reset should succeed
	w = ts.DoRequest("POST", pathRecoveryResetPwd, resetPayload, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second reset with same token should fail (single-use)
	w = ts.DoRequest("POST", pathRecoveryResetPwd, resetPayload, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "already been used")
}

// --- ResetAccount Token Single-Use Tests ---

func TestRecoveryResetAccountTokenSingleUse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser2")

	// Get a recovery token
	seedRecoveryCode(t, ts, user.Email, "222222", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "222222",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	resetPayload := map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPassword,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}

	// First reset should succeed
	w = ts.DoRequest("POST", pathRecoveryResetAcct, resetPayload, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second reset with same token should fail (single-use)
	w = ts.DoRequest("POST", pathRecoveryResetAcct, resetPayload, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryResetAccountRequiresDataLossAcknowledgement(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser3")

	seedRecoveryCode(t, ts, user.Email, "333333", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "333333",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	// Missing acknowledge_data_loss should fail
	w = ts.DoRequest("POST", pathRecoveryResetAcct, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Anti-Enumeration Tests ---

func TestRecoveryBeginSameResponseForValidAndInvalidEmails(t *testing.T) {
	ts := setupTS(t)
	ts.CreateTestUser(t, "enumuser1")

	// Valid email
	w1 := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": "enumuser1@test.concord.chat",
	}, nil)

	// Invalid email
	w2 := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": "doesnotexist@test.concord.chat",
	}, nil)

	// Both must return identical status and message
	assert.Equal(t, w1.Code, w2.Code)

	var body1, body2 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	testhelpers.ParseJSON(t, w2, &body2)
	assert.Equal(t, body1["message"], body2["message"])
}

// --- RecoveryResetPassword Happy Path ---

func TestRecoveryResetPasswordSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetpwdok")

	// Get recovery token
	seedRecoveryCode(t, ts, user.Email, "999999", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "999999",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	// Reset password
	w = ts.DoRequest("POST", pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Login with new password should work
	w = ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    user.Email,
		"password": testNewPassword,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRecoveryResetPasswordWeakPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetpwdweak")

	seedRecoveryCode(t, ts, user.Email, "888888", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "888888",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	// Weak password should fail
	w = ts.DoRequest("POST", pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        "weak",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryResetPasswordInvalidToken(t *testing.T) {
	ts := setupTS(t)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      "invalid-token",
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- RecoveryResetAccount Happy Path ---

func TestRecoveryResetAccountSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetacctok")

	seedRecoveryCode(t, ts, user.Email, "777777", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "777777",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w = ts.DoRequest("POST", pathRecoveryResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPassword,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Login with new password
	w = ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    user.Email,
		"password": testNewPassword,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Logout with cookie ---

func TestLogoutWithCookie(t *testing.T) {
	ts := setupTS(t)

	// New flow: register returns pending_id; access token comes from confirm step.
	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", "/api/v1/auth/register", map[string]interface{}{
		"email":               "logoutcookie@test.concord.chat",
		"username":            "logoutcookie",
		"password":            "TestPassword123!", //nolint:gosec // test credential
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
	}, nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)

	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	confirmW := ts.DoRequest("POST", "/api/v1/auth/register/confirm", map[string]string{
		"pending_id": pendingID, "code": code,
	}, nil)
	require.Equal(t, http.StatusOK, confirmW.Code)

	var confirmBody map[string]interface{}
	testhelpers.ParseJSON(t, confirmW, &confirmBody)
	accessToken := confirmBody["access_token"].(string)

	w = ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
