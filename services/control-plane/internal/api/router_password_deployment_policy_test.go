package api_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real router must carry its instance type into both auth and users. A
// self-hosted user can sign in, see effective settings, and unlink a stale SSO
// identity without losing the native password session just established.
func TestNewRouter_SelfHostedNativePasswordPolicy(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "  SELF-HOSTED  ")
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "routernativepassword")
	_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		VALUES ($1, 'google', 'router-native-password-subject', $2)`, user.ID, user.Email)
	require.NoError(t, err)
	otherUser := ts.CreateTestUser(t, "routernativeother")
	_, err = ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE, trust_sso_security = TRUE WHERE id = $1`, otherUser.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		VALUES ($1, 'google', 'router-other-account-subject', $2)`, otherUser.ID, otherUser.Email)
	require.NoError(t, err)

	login := ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": user.Password,
	}, nil)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var session struct {
		AccessToken string `json:"access_token"`
	}
	testhelpers.ParseJSON(t, login, &session)
	require.NotEmpty(t, session.AccessToken)
	headers := testhelpers.AuthHeaders(session.AccessToken)

	settings := ts.DoRequest(http.MethodGet, "/api/v1/users/me/security", nil, headers)
	require.Equal(t, http.StatusOK, settings.Code, settings.Body.String())
	assert.JSONEq(t, `{"password_login_disabled":false,"trust_sso_security":false}`, settings.Body.String())
	patch := ts.DoRequest(http.MethodPatch, "/api/v1/users/me/security", map[string]any{
		"password_login_disabled": true, "trust_sso_security": true, "current_passphrase": user.Password,
	}, headers)
	require.Equal(t, http.StatusForbidden, patch.Code, patch.Body.String())
	assert.JSONEq(t, `{"error_code":"password_login_required_self_hosted"}`, patch.Body.String())
	unlink := ts.DoRequest(http.MethodDelete, "/api/v1/users/me/sso-identities/google", nil, headers)
	require.Equal(t, http.StatusOK, unlink.Code, unlink.Body.String())

	var storedDisabled, trust bool
	require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled, &trust))
	assert.False(t, storedDisabled, "successful final self-hosted unlink must restore the durable password fallback")
	assert.False(t, trust, "refused mixed settings must not write the trust preference")
	var identities int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, user.ID).Scan(&identities))
	assert.Zero(t, identities)
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, otherUser.ID).Scan(&identities))
	assert.Equal(t, 1, identities, "unlink affects only the authenticated user's identity")
	var otherDisabled, otherTrust bool
	require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, otherUser.ID).Scan(&otherDisabled, &otherTrust))
	assert.True(t, otherDisabled, "final unlink must clear only the authenticated user's preference")
	assert.True(t, otherTrust)
	loginWithoutIdentity := ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": user.Password,
	}, nil)
	require.Equal(t, http.StatusOK, loginWithoutIdentity.Code, loginWithoutIdentity.Body.String())

	// Construct a newly configured production router over the same persisted
	// account, as happens when the operator returns the instance to SaaS.
	t.Setenv("INSTANCE_TYPE", "saas")
	saas := testhelpers.SetupTestServer(t)
	descriptor := saas.DoRequest(http.MethodGet, "/api/v1/server/capabilities", nil, nil)
	require.Equal(t, http.StatusOK, descriptor.Code, descriptor.Body.String())
	var capabilities struct {
		Server struct {
			InstanceType string `json:"instanceType"`
		} `json:"server"`
	}
	testhelpers.ParseJSON(t, descriptor, &capabilities)
	require.Equal(t, "saas", capabilities.Server.InstanceType)
	saasLogin := saas.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": user.Password,
	}, nil)
	require.Equal(t, http.StatusOK, saasLogin.Code, saasLogin.Body.String())
	var newSession struct {
		AccessToken string `json:"access_token"`
		SessionID   string `json:"session_id"`
	}
	testhelpers.ParseJSON(t, saasLogin, &newSession)
	assert.NotEmpty(t, newSession.AccessToken)
	assert.NotEmpty(t, newSession.SessionID)
	require.NoError(t, saas.DB.QueryRow(`SELECT password_login_disabled FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled))
	assert.False(t, storedDisabled)
}
