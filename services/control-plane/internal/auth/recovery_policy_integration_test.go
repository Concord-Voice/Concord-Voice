package auth_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/lib/pq"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"
)

func TestRecoveryPolicyEmailCannotBypassReservedTOTP(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverypolicy")
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{totp,email}', recovery_only_methods = '{totp}' WHERE id = $1`, user.ID)
	require.NoError(t, err)
	// Real enabled factor state must remain authoritative even if the flags lag.
	_, err = ts.DB.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed) VALUES ($1, $2, $3, TRUE, TRUE)`, user.ID, []byte("sealed-test-secret"), []byte("test-nonce"))
	require.NoError(t, err)
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)
	w := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": testRecoveryCode}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.NotContains(t, body, "recovery_token", "email alone must not grant reset authority")
	require.NotContains(t, body, "recovery_wrapped_private_key")
	require.Equal(t, true, body["mfa_required"])
	require.NotEmpty(t, body["mfa_challenge_token"])
}

func TestRecoveryPolicyReservedFactorCompletesRecovery(t *testing.T) {
	for _, method := range []string{"totp", "backup_code"} {
		t.Run(method, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "recoverwithfactor")
			keyring, err := mfa.ParseKeyring(strings.Repeat("0", 64), 1, "")
			require.NoError(t, err)
			key, err := totp.Generate(totp.GenerateOpts{Issuer: "Concord Test", AccountName: user.Email})
			require.NoError(t, err)
			secret := key.Secret()
			sealed, nonce, version, err := keyring.Seal([]byte(secret))
			require.NoError(t, err)
			codes, hashes, err := mfa.GenerateBackupCodes()
			require.NoError(t, err)
			_, err = ts.DB.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
			 VALUES ($1, $2, $3, $4, TRUE, TRUE, $5, $6)`, user.ID, sealed, nonce, version, pq.Array(hashes), pq.Array(make([]bool, len(hashes))))
			require.NoError(t, err)
			// A stale mirror and a restriction which lapses at sign-in must not
			// let recovery skip the actual reserved authenticator.
			_, err = ts.DB.Exec(`UPDATE users SET mfa_methods = '{}', recovery_only_methods = '{totp}' WHERE id = $1`, user.ID)
			require.NoError(t, err)
			seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)
			w := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": testRecoveryCode}, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var challenge map[string]interface{}
			testhelpers.ParseJSON(t, w, &challenge)
			require.NotContains(t, challenge, "recovery_token")
			token := testhelpers.JSONField[string](t, challenge, "mfa_challenge_token")
			code, err := totp.GenerateCode(secret, time.Now())
			require.NoError(t, err)
			if method == "backup_code" {
				code = codes[0]
			}
			body := map[string]string{"mfa_challenge_token": token, "method": method, "code": code}
			w = ts.DoRequest(http.MethodPost, "/api/v1/auth/mfa/verify", body, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var result map[string]interface{}
			testhelpers.ParseJSON(t, w, &result)
			final := testhelpers.JSONField[string](t, result, "recovery_token")
			claims, err := mfa.ValidateChallengeToken(final, testhelpers.TestJWTSecret, mfa.PurposeRecovery)
			require.NoError(t, err)
			require.Equal(t, user.ID, claims.UserID)
			require.Equal(t, "totp", claims.RecoveryFactor, "backup codes retain their authenticator-family proof")
			require.NotContains(t, result, "access_token")
			w = ts.DoRequest(http.MethodPost, "/api/v1/auth/mfa/verify", body, nil)
			require.Equal(t, http.StatusUnauthorized, w.Code, "the recovery challenge is single use")
		})
	}
}

func TestRecoveryPolicyUnsupportedRequirementsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, reserved, flags string
		hardened, redisSMS    bool
	}{
		{name: "reserved SMS", reserved: "{sms}"},
		{name: "hardened SQL SMS", reserved: "{}", flags: "{email,sms}", hardened: true},
		{name: "hardened actual SMS", reserved: "{}", hardened: true, redisSMS: true},
		{name: "unknown reserved factor", reserved: "{future_factor}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "unsupportedrecovery")
			if tc.flags == "" {
				tc.flags = "{}"
			}
			_, err := ts.DB.Exec(`UPDATE users SET recovery_only_methods=$1, mfa_methods=$2, recovery_hardened=$3 WHERE id=$4`, tc.reserved, tc.flags, tc.hardened, user.ID)
			require.NoError(t, err)
			if tc.redisSMS {
				require.NoError(t, ts.Redis.Set(context.Background(), "mfa_emailsms_enabled:"+user.ID+":sms", "1", 0).Err())
			}
			seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)
			w := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": testRecoveryCode}, nil)
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "recovery_token")
			w = ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": testRecoveryCode}, nil)
			require.Equal(t, http.StatusUnauthorized, w.Code, "policy failure must not restore consumed email proof: %s", w.Body.String())
		})
	}
}

func TestRecoveryPolicyLegacyAndIncompleteTokensCannotReachRecoveryConsumers(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverypurpose")
	for _, purpose := range []mfa.ChallengePurpose{"recovery", mfa.PurposeRecoveryChallenge} {
		token, _, err := mfa.GenerateChallengeToken(user.ID, purpose, mfa.JWTSecret(testhelpers.TestJWTSecret), "")
		require.NoError(t, err)
		for _, path := range []string{pathRecoveryResetPwd, pathRecoveryResetAcct, "/api/v1/auth/recovery/social-request"} {
			body := map[string]interface{}{"recovery_token": token, "new_password": testNewPassword, "wrapped_private_key": "dGVzdA==", "key_derivation_salt": "dGVzdA==", "key_derivation_alg": "argon2id", "public_key": "dGVzdA==", "acknowledge_data_loss": true, "ephemeral_public_key": "dGVzdA=="}
			w := doRecoveryConsumerRequest(t, ts, http.MethodPost, path, body, nil)
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		}
		w := ts.DoRequest(http.MethodGet, "/api/v1/auth/recovery/device-request/00000000-0000-4000-8000-000000000001", nil, testhelpers.AuthHeaders(token))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		w = doDormantSocialRequest(t, ts, http.MethodGet, "/api/v1/auth/recovery/social-request/00000000-0000-4000-8000-000000000001", nil, testhelpers.AuthHeaders(token))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	}
}

func TestRecoveryPolicyTighteningInvalidatesPriorEmailAuthority(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "tightenedrecovery")
	token := recoveryTokenFor(t, ts, user)
	_, err := ts.DB.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed) VALUES ($1, $2, $3, TRUE, TRUE)`, user.ID, []byte("sealed-test-secret"), []byte("test-nonce"))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE users SET recovery_only_methods = '{totp}' WHERE id = $1`, user.ID)
	require.NoError(t, err)
	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest(http.MethodPost, pathRecoveryResetAcct, map[string]interface{}{
		"recovery_token": token, "new_password": testNewPassword,
		"wrapped_private_key": wrappedKey, "key_derivation_salt": salt,
		"key_derivation_alg": "argon2id", "public_key": publicKey, "acknowledge_data_loss": true,
	}, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}
