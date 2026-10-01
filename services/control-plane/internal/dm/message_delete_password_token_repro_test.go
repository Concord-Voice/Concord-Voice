package dm

// Reproduction for the own-rule password defect on the DM message delete
// (#3509, Codex security P1; design spec "Developer decisions, 2026-10-01",
// T-1, T-4). DM Clear is in clear_password_token_repro_test.go.
//
// Under the decision the account password reaches only the mint endpoint
// (POST /api/v1/auth/step-up/password). The route takes a single-use
// step_up_token and REFUSES a body that still carries current_password, with a
// 400. Today stepup.VerifyOwnRuleTx's password arm verifies
// in.CurrentPassword directly, so the correct password confirms the delete.
//
// Oracle: a request carrying the correct current_password is refused with 400
// and nothing is deleted.
//
// TestDMDeleteOwnRule_RefusesCurrentPassword_Repro3509 failed before the fix
// and passes after it. The pre-fix control that asserted the old contract was
// deleted with the fix, as T-4 requires; the MFA-account control below holds
// before and after.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// armOwnRuleDelete builds the state in which the next delete meets the
// own-rule arm: own rule on, soft-lock tripped, and a password account unless
// mfa asks for an inline factor instead.
func armOwnRuleDelete(t *testing.T, mfa bool) (hs *softLockHarness, messageID string) {
	t.Helper()
	hs = newSoftLockHarness(t)
	hs.setOwnRule(t, true)
	hs.setPassword(t)
	if mfa {
		hs.enrollMFA(t)
	}
	hs.overThreshold(t)
	return hs, hs.message(t)
}

// regression for #3509 (Codex P1)
func TestDMDeleteOwnRule_RefusesCurrentPassword_Repro3509(t *testing.T) {
	hs, messageID := armOwnRuleDelete(t, false)

	// Precondition, and the arm being reached: with no step-up field the route
	// answers the own-rule password prompt. That holds before and after the
	// fix, so it proves the fixture is tripped, on the own rule, for an account
	// with no MFA, without depending on how the password is later refused.
	// (A refused request never resets the counters, so the request below meets
	// the same state.)
	assertSoftLockRefusal(t, hs.delete(t, messageID, ""), dmMessageDeleteStepUpCopy.CredentialRequired, softLockRetryAfter30)
	require.True(t, hs.messageExists(t, messageID), "precondition: the prompt must not delete")

	w := hs.delete(t, messageID, passwordBody(softLockPlaintext))

	assert.Equal(t, http.StatusBadRequest, w.Code,
		"a body carrying current_password must be refused with 400: the password reaches only the mint endpoint; got %d %s",
		w.Code, w.Body.String())
	assert.True(t, hs.messageExists(t, messageID),
		"a request that carried the account password must not delete anything")
}

// Control, valid before and after: an account with inline MFA, sent no step-up
// field, gets the MFA refusal and never the password prompt.
func TestDMDeleteOwnRule_MFAAccountRefusalShape(t *testing.T) {
	hs, messageID := armOwnRuleDelete(t, true)

	w := hs.delete(t, messageID, "")

	body := assertSoftLockRefusal(t, w, "MFA verification required", softLockRetryAfter30)
	assert.Equal(t, true, body["mfa_required"])
	assert.NotContains(t, body, "password_required")
	assert.True(t, hs.messageExists(t, messageID))
}
