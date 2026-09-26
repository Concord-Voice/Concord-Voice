package servers_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regression for PR #3464 review finding 8 (CWE-307). The toggle's OFF
// confirmation charged its own step-up budget, so a holder of a stolen owner
// or Administrator session got five more factor guesses every window on top of
// the five the MFA-settings routes allow. The toggle now draws from the SAME
// per-user budget, so the two surfaces together admit five.
//
// Both directions run through the real router against a real MFA-settings
// route, so neither half can pass by agreeing with a key spelling the other
// side does not use.
const (
	mfaSettingsEmailSmsDisableURL = "/api/v1/mfa/email-sms/disable"
	mfaSettingsBackupEmailURL     = "/api/v1/mfa/backup-email"
)

// settingsGuess sends one credentialed, wrong step-up to an MFA-settings route.
// A credentialed request is charged before it is verified (stepup.Budget).
func settingsGuess(env *mfaEnv, u testhelpers.TestUser, method, url string) int {
	body := map[string]any{"mfa_code": mfaWrongCode}
	if url == mfaSettingsBackupEmailURL {
		body["email"] = "shared-budget@example.com"
	}
	return env.ts.DoRequest(method, url, body, testhelpers.AuthHeaders(u.AccessToken)).Code
}

// Toggle guesses spend the budget an MFA-settings route then refuses on.
func TestMFAEnforcement_ToggleGuessesSpendTheMFASettingsBudget(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfasb1", true)
	enrollMFATOTP(t, env, f.owner.ID)

	// Control: before any toggle guess, the settings route answers with its
	// ordinary verification refusal, not the budget's 429.
	control := settingsGuess(env, f.owner, http.MethodPost, mfaSettingsEmailSmsDisableURL)
	require.Equal(t, http.StatusForbidden, control, "control: one wrong settings guess is a 403")

	for i := 2; i <= stepup.BudgetLimit; i++ {
		w := putMFA(env, f.owner, f.serverID, bodyOff(mfaWrongCode))
		require.Equal(t, http.StatusForbidden, w.Code, "toggle attempt %d: %s", i, w.Body.String())
	}

	w := env.ts.DoRequest(http.MethodPost, mfaSettingsEmailSmsDisableURL,
		map[string]any{"mfa_code": mfaWrongCode}, testhelpers.AuthHeaders(f.owner.AccessToken))
	assert.Equal(t, http.StatusTooManyRequests, w.Code,
		"one settings guess plus four toggle guesses spend the one shared budget: %s", w.Body.String())
	assert.JSONEq(t, mfaErrorBody(stepup.ErrMsgTooManyAttempts), w.Body.String())
}

// MFA-settings guesses spend the budget the toggle then refuses on, even with
// the RIGHT code.
func TestMFAEnforcement_SettingsGuessesSpendTheToggleBudget(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfasb2", true)
	enrollMFATOTP(t, env, f.owner.ID)

	// Three on EmailSmsDisable and two on SetBackupEmail stay under each
	// route's own pre-existing per-route limiter (3/min and 5/min), so the only
	// limit these five can reach is the step-up budget.
	for i := 1; i <= 3; i++ {
		require.Equal(t, http.StatusForbidden,
			settingsGuess(env, f.owner, http.MethodPost, mfaSettingsEmailSmsDisableURL), "email-sms attempt %d", i)
	}
	for i := 1; i <= 2; i++ {
		require.Equal(t, http.StatusForbidden,
			settingsGuess(env, f.owner, http.MethodPut, mfaSettingsBackupEmailURL), "backup-email attempt %d", i)
	}

	w := putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	assert.Equal(t, http.StatusTooManyRequests, w.Code,
		"five settings guesses spend the budget the toggle charges: %s", w.Body.String())
	assert.JSONEq(t, mfaErrorBody(stepup.ErrMsgTooManyAttempts), w.Body.String())
	assert.True(t, readMFAFlag(t, env, f.serverID), "a 429 changes nothing")
	assert.False(t, backupCodeSpent(t, env, f.owner.ID), "a 429 must not reach the verifier")
}
