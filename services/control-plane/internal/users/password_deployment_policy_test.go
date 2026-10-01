package users_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSecurity_DeploymentPasswordPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode   string
		wantDisabled bool
	}{
		{name: "self hosted", mode: "self-hosted", wantDisabled: false},
		{name: "normalized self hosted", mode: "  SELF-HOSTED  ", wantDisabled: false},
		{name: "SaaS", mode: "saas", wantDisabled: true},
		{name: "blank SaaS fallback", wantDisabled: true},
		{name: "unknown SaaS fallback", mode: "unrecognized", wantDisabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", tc.mode)
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "deploymentsettings")
			_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE, trust_sso_security = TRUE WHERE id = $1`, user.ID)
			require.NoError(t, err)

			w := ts.DoRequest(http.MethodGet, urlUsersMeSecurity, nil, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var body struct {
				Disabled bool `json:"password_login_disabled"`
				Trust    bool `json:"trust_sso_security"`
			}
			testhelpers.ParseJSON(t, w, &body)
			assert.Equal(t, tc.wantDisabled, body.Disabled)
			assert.True(t, body.Trust, "the unrelated trust preference remains visible")
			var storedDisabled, storedTrust bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled, &storedTrust))
			assert.True(t, storedDisabled, "reading effective settings must retain the stored preference")
			assert.True(t, storedTrust)
		})
	}
}

func TestPatchSecurity_DeploymentPasswordPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode, passphrase, errorCode                         string
		linked, storedDisabled, requestedDisabled, requestedTrust bool
		status                                                    int
		wantDisabled, wantTrust                                   bool
	}{
		{name: "self hosted refuses disable with linked identity", mode: "self-hosted", passphrase: testPassword, linked: true, requestedDisabled: true, status: http.StatusForbidden, errorCode: "password_login_required_self_hosted"},
		{name: "self hosted refuses disable without identity", mode: "self-hosted", passphrase: testPassword, requestedDisabled: true, status: http.StatusForbidden, errorCode: "password_login_required_self_hosted"},
		{name: "normalized self hosted refusal", mode: "  SELF-HOSTED  ", passphrase: testPassword, linked: true, requestedDisabled: true, status: http.StatusForbidden, errorCode: "password_login_required_self_hosted"},
		{name: "mixed request writes neither preference", mode: "self-hosted", passphrase: testPassword, linked: true, requestedDisabled: true, requestedTrust: true, status: http.StatusForbidden, errorCode: "password_login_required_self_hosted"},
		{name: "refusal retains existing raw flag", mode: "self-hosted", passphrase: testPassword, linked: true, storedDisabled: true, requestedDisabled: true, requestedTrust: true, status: http.StatusForbidden, errorCode: "password_login_required_self_hosted", wantDisabled: true},
		{name: "wrong proof precedes deployment refusal", mode: "self-hosted", passphrase: "wrong-test-passphrase", linked: true, requestedDisabled: true, status: http.StatusUnauthorized, errorCode: "invalid_credentials"},
		{name: "missing proof precedes deployment refusal", mode: "self-hosted", linked: true, requestedDisabled: true, status: http.StatusUnauthorized, errorCode: "passphrase_required"},
		{name: "self hosted explicitly re-enables password", mode: "self-hosted", passphrase: testPassword, storedDisabled: true, requestedTrust: true, status: http.StatusOK, wantTrust: true},
		{name: "SaaS can disable with identity", mode: "saas", passphrase: testPassword, linked: true, requestedDisabled: true, requestedTrust: true, status: http.StatusOK, wantDisabled: true, wantTrust: true},
		{name: "SaaS still requires fallback", mode: "saas", passphrase: testPassword, requestedDisabled: true, status: http.StatusBadRequest, errorCode: "would_lock_out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", tc.mode)
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "deploymentpatch")
			_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = $2 WHERE id = $1`, user.ID, tc.storedDisabled)
			require.NoError(t, err)
			if tc.linked {
				_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
					VALUES ($1, 'google', 'deployment-patch-subject', $2)`, user.ID, user.Email)
				require.NoError(t, err)
			}
			w := ts.DoRequest(http.MethodPatch, urlUsersMeSecurity, map[string]any{
				"password_login_disabled": tc.requestedDisabled,
				"trust_sso_security":      tc.requestedTrust,
				"current_passphrase":      tc.passphrase,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.errorCode != "" {
				var body struct {
					ErrorCode string `json:"error_code"`
				}
				testhelpers.ParseJSON(t, w, &body)
				assert.Equal(t, tc.errorCode, body.ErrorCode)
			}
			var storedDisabled, storedTrust bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled, &storedTrust))
			assert.Equal(t, tc.wantDisabled, storedDisabled)
			assert.Equal(t, tc.wantTrust, storedTrust)
		})
	}
}

func TestDeleteSSOIdentity_DeploymentPasswordFallback(t *testing.T) {
	for _, tc := range []struct {
		name, mode             string
		status, wantIdentities int
		partial, wantDisabled  bool
	}{
		{name: "self hosted can unlink last identity", mode: "self-hosted", status: http.StatusOK},
		{name: "normalized self hosted can unlink", mode: "  SELF-HOSTED  ", status: http.StatusOK},
		{name: "self hosted partial unlink preserves preference", mode: "self-hosted", partial: true, status: http.StatusOK, wantIdentities: 1, wantDisabled: true},
		{name: "SaaS retains last identity", mode: "saas", status: http.StatusBadRequest, wantIdentities: 1, wantDisabled: true},
		{name: "blank retains SaaS lockout guard", status: http.StatusBadRequest, wantIdentities: 1, wantDisabled: true},
		{name: "unknown retains SaaS lockout guard", mode: "unrecognized", status: http.StatusBadRequest, wantIdentities: 1, wantDisabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", tc.mode)
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "deploymentunlink")
			_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE, trust_sso_security = TRUE WHERE id = $1`, user.ID)
			require.NoError(t, err)
			if tc.partial {
				_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
					VALUES ($1, 'apple', 'deployment-unlink-other-subject', $2)`, user.ID, user.Email)
				require.NoError(t, err)
			}
			_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
				VALUES ($1, 'google', 'deployment-unlink-subject', $2)`, user.ID, user.Email)
			require.NoError(t, err)

			w := ts.DoRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google", nil, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusBadRequest {
				var body struct {
					ErrorCode string `json:"error_code"`
				}
				testhelpers.ParseJSON(t, w, &body)
				assert.Equal(t, "would_lock_out", body.ErrorCode)
			}
			var identities int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, user.ID).Scan(&identities))
			assert.Equal(t, tc.wantIdentities, identities)
			var storedDisabled, trust bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled, &trust))
			assert.Equal(t, tc.wantDisabled, storedDisabled, "only successful final self-hosted unlink clears the stored preference")
			assert.True(t, trust, "unlink must preserve the unrelated trust preference")
		})
	}
}

func TestPatchSecurity_SelfHostedTrustOnlyPreservesPasswordPreference(t *testing.T) {
	for _, tc := range []struct {
		name, passphrase string
		status           int
		wantTrust        bool
	}{
		{name: "valid proof", passphrase: testPassword, status: http.StatusOK, wantTrust: true},
		{name: "wrong proof", passphrase: "wrong-test-passphrase", status: http.StatusUnauthorized},
		{name: "missing proof", status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", "self-hosted")
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "deploymenttrust")
			_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
			require.NoError(t, err)
			w := ts.DoRequest(http.MethodPatch, urlUsersMeSecurity, map[string]any{
				"trust_sso_security": true, "current_passphrase": tc.passphrase,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			var storedDisabled, storedTrust bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&storedDisabled, &storedTrust))
			assert.True(t, storedDisabled, "omitting the password preference must preserve it")
			assert.Equal(t, tc.wantTrust, storedTrust)
		})
	}
}

// These are ordinary sequential settings operations. Each accepted change must
// leave the later operation a truthful fallback decision on the committed state.
func TestSecurityFallback_SequentialSaaSChanges(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		unlinkFirst, wantDisabled, wantTrust bool
		wantIdentities                       int
	}{
		{name: "unlink then refuse disable", unlinkFirst: true},
		{name: "disable then refuse last unlink", wantDisabled: true, wantTrust: true, wantIdentities: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", "saas")
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "sequentialfallback")
			_, err := ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
				VALUES ($1, 'google', 'sequential-fallback-subject', $2)`, user.ID, user.Email)
			require.NoError(t, err)
			patch := func() *httptest.ResponseRecorder {
				return ts.DoRequest(http.MethodPatch, urlUsersMeSecurity, map[string]any{
					"password_login_disabled": true, "trust_sso_security": true, "current_passphrase": user.Password,
				}, testhelpers.AuthHeaders(user.AccessToken))
			}
			unlink := func() *httptest.ResponseRecorder {
				return ts.DoRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google", nil, testhelpers.AuthHeaders(user.AccessToken))
			}
			first, second := patch, unlink
			if tc.unlinkFirst {
				first, second = unlink, patch
			}
			accepted := first()
			require.Equal(t, http.StatusOK, accepted.Code, accepted.Body.String())
			refused := second()
			require.Equal(t, http.StatusBadRequest, refused.Code, refused.Body.String())
			var refusal struct {
				ErrorCode string `json:"error_code"`
			}
			testhelpers.ParseJSON(t, refused, &refusal)
			assert.Equal(t, "would_lock_out", refusal.ErrorCode)
			var disabled, trust bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&disabled, &trust))
			assert.Equal(t, tc.wantDisabled, disabled)
			assert.Equal(t, tc.wantTrust, trust, "the refused mixed patch must leave both flags unchanged")
			var identities int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, user.ID).Scan(&identities))
			assert.Equal(t, tc.wantIdentities, identities)
		})
	}
}

// The existing statement hook supplies only an ordinary database fault here:
// no concurrent request, callback, held lock, or interleaved write is involved.
func TestSecurityFallback_DatabaseFailurePreservesState(t *testing.T) {
	for _, tc := range []struct {
		name, statement, errorCode string
		unlink, commitFailure      bool
	}{
		{name: "patch count failure", statement: "SELECT COUNT(*) FROM user_sso_identities", errorCode: "lookup_failed"},
		{name: "patch write failure", statement: "UPDATE users SET", errorCode: "update_failed"},
		{name: "patch determinate commit failure", commitFailure: true, errorCode: "update_failed"},
		{name: "unlink count failure", statement: "SELECT COUNT(*) FROM user_sso_identities", unlink: true, errorCode: "lookup_failed"},
		{name: "unlink write failure", statement: "DELETE FROM user_sso_identities", unlink: true, errorCode: "delete_failed"},
		{name: "unlink determinate commit failure", unlink: true, commitFailure: true, errorCode: "delete_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", "saas")
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "fallbackdatabasefault")
			_, err := ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
				VALUES ($1, 'google', 'fallback-database-fault-subject', $2)`, user.ID, user.Email)
			require.NoError(t, err)
			hook, hookedDB := stmthook.Open(t)
			h := users.NewHandler(hookedDB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
			if tc.commitFailure {
				// ArmCommit(false) rolls back the transaction before returning the
				// injected error, so unchanged state is a determinate expectation.
				hook.ArmCommit(false, stmthook.ErrInjected)
			} else {
				hook.Arm([]string{tc.statement}, nil, stmthook.ErrInjected)
			}
			request := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set("user_id", user.ID)
				if tc.unlink {
					c.Params = gin.Params{{Key: "provider", Value: "google"}}
					c.Request = httptest.NewRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google", nil)
					h.DeleteSSOIdentity(c)
				} else {
					c.Request = httptest.NewRequest(http.MethodPatch, urlUsersMeSecurity, strings.NewReader(
						`{"password_login_disabled":true,"trust_sso_security":true,"current_passphrase":"TestPassword123!"}`))
					c.Request.Header.Set("Content-Type", "application/json")
					h.PatchSecurity(c)
				}
				return w
			}
			failed := request()
			require.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
			var refusal struct {
				ErrorCode string `json:"error_code"`
				OK        bool   `json:"ok"`
			}
			testhelpers.ParseJSON(t, failed, &refusal)
			assert.Equal(t, tc.errorCode, refusal.ErrorCode)
			assert.False(t, refusal.OK)
			if !tc.commitFailure {
				seen, hookErr := hook.Report()
				require.NoError(t, hookErr)
				require.Equal(t, 1, seen, "the chosen database operation must have received the injected failure")
			}
			var disabled, trust bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&disabled, &trust))
			assert.False(t, disabled)
			assert.False(t, trust)
			var identities int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, user.ID).Scan(&identities))
			assert.Equal(t, 1, identities, "the failed request must preserve the existing identity")
			// The hook is one-shot. A later valid request proves the failure
			// leaves the account usable and the transaction finished.
			retried := request()
			require.Equal(t, http.StatusOK, retried.Code, retried.Body.String())
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&disabled, &trust))
			assert.Equal(t, !tc.unlink, disabled)
			assert.Equal(t, !tc.unlink, trust)
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, user.ID).Scan(&identities))
			if tc.unlink {
				assert.Zero(t, identities)
			} else {
				assert.Equal(t, 1, identities)
			}
		})
	}
}

func TestDeleteSSOIdentity_SelfHostedFinalUnlinkFailurePreservesPreference(t *testing.T) {
	for _, tc := range []struct {
		name, statement string
		commitFailure   bool
	}{
		{name: "identity delete failure", statement: "DELETE FROM user_sso_identities"},
		{name: "password preference write failure", statement: "UPDATE users SET password_login_disabled"},
		{name: "determinate commit failure", commitFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTANCE_TYPE", "self-hosted")
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "finalunlinkfault")
			_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE, trust_sso_security = TRUE WHERE id = $1`, user.ID)
			require.NoError(t, err)
			_, err = ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
				VALUES ($1, 'google', 'final-unlink-fault-subject', $2)`, user.ID, user.Email)
			require.NoError(t, err)
			hook, hookedDB := stmthook.Open(t)
			h := users.NewHandler(hookedDB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
			h.SetInstanceType("self-hosted")
			if tc.commitFailure {
				hook.ArmCommit(false, stmthook.ErrInjected)
			} else {
				hook.Arm([]string{tc.statement}, nil, stmthook.ErrInjected)
			}
			request := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set("user_id", user.ID)
				c.Params = gin.Params{{Key: "provider", Value: "google"}}
				c.Request = httptest.NewRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google", nil)
				h.DeleteSSOIdentity(c)
				return w
			}
			failed := request()
			require.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
			assert.JSONEq(t, `{"error_code":"delete_failed"}`, failed.Body.String())
			if !tc.commitFailure {
				seen, hookErr := hook.Report()
				require.NoError(t, hookErr)
				require.Equal(t, 1, seen, "the selected database operation must receive the ordinary injected fault")
			}
			var disabled, trust bool
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&disabled, &trust))
			assert.True(t, disabled, "failed unlink must preserve the stored preference")
			assert.True(t, trust)
			var identities int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, user.ID).Scan(&identities))
			assert.Equal(t, 1, identities, "failed unlink must preserve the linked identity")
			// Fault-only hooks are one-shot. The ordinary successful retry must
			// commit both deletion and durable password fallback together.
			retried := request()
			require.Equal(t, http.StatusOK, retried.Code, retried.Body.String())
			require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&disabled, &trust))
			assert.False(t, disabled)
			assert.True(t, trust)
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, user.ID).Scan(&identities))
			assert.Zero(t, identities)
		})
	}
}

func TestDeleteSSOIdentity_SelfHostedNotLinkedPreservesAccountPreferences(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "notlinkedpreference")
	otherUser := ts.CreateTestUser(t, "notlinkedotheraccount")
	for _, id := range []string{user.ID, otherUser.ID} {
		_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE, trust_sso_security = TRUE WHERE id = $1`, id)
		require.NoError(t, err)
	}
	_, err := ts.DB.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		VALUES ($1, 'google', 'not-linked-other-account-subject', $2)`, otherUser.ID, otherUser.Email)
	require.NoError(t, err)
	w := ts.DoRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error_code":"not_linked"}`, w.Body.String())
	for _, account := range []struct {
		id         string
		identities int
	}{{user.ID, 0}, {otherUser.ID, 1}} {
		var disabled, trust bool
		require.NoError(t, ts.DB.QueryRow(`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, account.id).Scan(&disabled, &trust))
		assert.True(t, disabled, "a not-linked refusal must change neither account's stored preference")
		assert.True(t, trust)
		var identities int
		require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, account.id).Scan(&identities))
		assert.Equal(t, account.identities, identities)
	}
}
