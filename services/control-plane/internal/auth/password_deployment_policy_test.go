package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stored SSO-only preferences must not remove native sign-in on an instance
// where the deployment policy has disabled every SSO entry point.
func TestLogin_DeploymentPasswordPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode, password, errorCode     string
		linked, disabled, mfa, mfaReadError bool
		status                              int
	}{
		{name: "self hosted linked identity", mode: "self-hosted", password: testPassword, linked: true, status: http.StatusOK},
		{name: "self hosted no identity", mode: "self-hosted", password: testPassword, status: http.StatusOK},
		{name: "normalized self hosted", mode: "  SELF-HOSTED  ", password: testPassword, linked: true, status: http.StatusOK},
		{name: "wrong password", mode: "self-hosted", password: testWrongPassword, linked: true, status: http.StatusUnauthorized},
		{name: "disabled account correct password", mode: "self-hosted", password: testPassword, linked: true, disabled: true, status: http.StatusForbidden, errorCode: "account_disabled"},
		{name: "disabled account wrong password", mode: "self-hosted", password: testWrongPassword, linked: true, disabled: true, status: http.StatusUnauthorized},
		{name: "MFA still required", mode: "self-hosted", password: testPassword, linked: true, mfa: true, status: http.StatusOK},
		{name: "MFA read failure", mode: "self-hosted", password: testPassword, linked: true, mfaReadError: true, status: http.StatusInternalServerError},
		{name: "SaaS remains SSO only", mode: "saas", password: testPassword, linked: true, status: http.StatusForbidden, errorCode: "account_uses_sso"},
		{name: "blank falls back to SaaS", password: testPassword, linked: true, status: http.StatusForbidden, errorCode: "account_uses_sso"},
		{name: "unknown falls back to SaaS", mode: "unrecognized", password: testPassword, linked: true, status: http.StatusForbidden, errorCode: "account_uses_sso"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "deploymentpassword")
			_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE, disabled = $2, trust_sso_security = $3 WHERE id = $1`, user.ID, tc.disabled, tc.mfa || tc.mfaReadError)
			require.NoError(t, err)
			if tc.linked {
				_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
					VALUES ($1, 'google', 'deployment-password-subject', $2)`, user.ID, user.Email)
				require.NoError(t, err)
			}
			h := auth.NewHandlerForInstance(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), testhelpers.TestJWTSecret, ts.Hub, tc.mode)
			h.SetPresenceHistory(ts.PresenceHistory)
			if tc.mfa || tc.mfaReadError {
				checker := &stubMFAChecker{LoginMethodsResult: []string{"totp"}, EnabledMethodsResult: []string{"totp"}}
				if tc.mfaReadError {
					checker.LoginMethodsErr = errors.New("test MFA method read unavailable")
				}
				h.SetMFAChecker(checker)
			}

			w := deploymentPasswordLogin(t, h, user.Email, tc.password)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			if tc.errorCode != "" {
				assert.Equal(t, tc.errorCode, body["error_code"])
			}
			if tc.status == http.StatusUnauthorized {
				assert.Equal(t, "Invalid credentials", body["error"])
				assert.Empty(t, body["error_code"], "bad credentials must not reveal account status")
			}
			var sessions int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID).Scan(&sessions))
			if tc.status == http.StatusOK && !tc.mfa {
				assert.NotEmpty(t, body["access_token"])
				assert.NotEmpty(t, body["session_id"])
				assert.Equal(t, 1, sessions)
			} else {
				assert.Empty(t, body["access_token"])
				assert.Empty(t, body["session_id"])
				assert.Zero(t, sessions, "a rejected login or MFA challenge must not mint a session")
				if tc.mfa {
					assert.Equal(t, true, body["mfa_required"])
					assert.NotEmpty(t, body["mfa_challenge_token"])
					assert.Equal(t, []any{"totp"}, body["methods"])
				} else {
					assert.Empty(t, body["mfa_challenge_token"])
				}
			}
			var storedDisabled bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled))
			assert.True(t, storedDisabled, "deployment policy must not rewrite the stored preference")
		})
	}
}

func deploymentPasswordLogin(t *testing.T, h *auth.Handler, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"email": email, "password": password})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, pathLogin, bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Login(c)
	return w
}

func TestLogin_SelfHostedPasswordPolicyRetainsLockout(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "deploymentlockout")
	_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	h := auth.NewHandlerForInstance(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), testhelpers.TestJWTSecret, ts.Hub, "self-hosted")
	h.SetPresenceHistory(ts.PresenceHistory)
	for range 5 {
		w := deploymentPasswordLogin(t, h, user.Email, testWrongPassword)
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	}
	w := deploymentPasswordLogin(t, h, user.Email, testPassword)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var sessions int
	require.NoError(t, ts.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1`, user.ID).Scan(&sessions))
	assert.Zero(t, sessions, "a locked account must remain unable to mint a password session")
}

func TestRecoveryReset_SelfHostedStoredSSOPreferenceAllowsNewPassword(t *testing.T) {
	for _, tc := range []struct {
		name, path   string
		accountReset bool
	}{
		{name: "password rewrap", path: pathRecoveryResetPwd},
		{name: "account key reset", path: pathRecoveryResetAcct, accountReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", "self-hosted")
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "deploymentrecovery")
			before := ts.DoRequest(http.MethodPost, pathLogin, map[string]string{"email": user.Email, "password": user.Password}, nil)
			require.Equal(t, http.StatusOK, before.Code, before.Body.String())
			var oldEpoch string
			require.NoError(t, ts.DB.QueryRow(`SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`, user.ID).Scan(&oldEpoch))
			_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
			require.NoError(t, err)
			_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
				VALUES ($1, 'google', 'deployment-recovery-subject', $2)`, user.ID, user.Email)
			require.NoError(t, err)

			// The normal local recovery-code fixture obtains its token through
			// verification; no token is constructed or external provider used.
			seedRecoveryCodeLocal(t, ts, user.Email, "999999", user.ID)
			verify := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{
				"email": user.Email, "code": "999999",
			}, nil)
			require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
			var recovery struct {
				Token string `json:"recovery_token"`
			}
			testhelpers.ParseJSON(t, verify, &recovery)
			require.NotEmpty(t, recovery.Token)
			pub, wrapped, salt := testhelpers.E2EETestKeys()
			payload := map[string]any{
				"recovery_token": recovery.Token, "new_password": testNewPassword,
				"public_key": pub, "wrapped_private_key": wrapped,
				"key_derivation_salt": salt, "key_derivation_alg": "argon2id",
			}
			if tc.accountReset {
				payload["acknowledge_data_loss"] = true
			}
			reset := ts.DoRequest(http.MethodPost, tc.path, payload, nil)
			require.Equal(t, http.StatusOK, reset.Code, reset.Body.String())
			var storedDisabled bool
			var newEpoch string
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, credential_epoch FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled, &newEpoch))
			assert.True(t, storedDisabled, "recovery retains the stored SSO preference")
			assert.NotEqual(t, oldEpoch, newEpoch, "recovery still rotates the credential epoch")
			var liveSessions int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID).Scan(&liveSessions))
			assert.Zero(t, liveSessions, "recovery still revokes the pre-reset native session")

			login := ts.DoRequest(http.MethodPost, pathLogin, map[string]string{"email": user.Email, "password": testNewPassword}, nil)
			require.Equal(t, http.StatusOK, login.Code, login.Body.String())
			var session struct {
				AccessToken string `json:"access_token"`
				SessionID   string `json:"session_id"`
			}
			testhelpers.ParseJSON(t, login, &session)
			assert.NotEmpty(t, session.AccessToken)
			assert.NotEmpty(t, session.SessionID)
			oldPassword := ts.DoRequest(http.MethodPost, pathLogin, map[string]string{"email": user.Email, "password": testPassword}, nil)
			require.Equal(t, http.StatusUnauthorized, oldPassword.Code, oldPassword.Body.String())
		})
	}
}
