package dm_test

// Reproduction for the own-rule password defect on DM Clear (#3509, Codex
// security P1; design spec "Developer decisions, 2026-10-01", T-1, T-4). The
// DM message delete is in message_delete_password_token_repro_test.go.
//
// Under the decision the account password reaches only the mint endpoint
// (POST /api/v1/auth/step-up/password). Clear takes a single-use
// step_up_token and REFUSES a body that still carries current_password, with a
// 400. Today stepup.VerifyOwnRuleTx's password arm verifies
// in.CurrentPassword directly, so the correct password clears the history.
//
// Oracle: a request carrying the correct current_password is refused with 400
// and no clear range is written.
//
// TestDMClearOwnRule_RefusesCurrentPassword_Repro3509 failed before the fix
// and passes after it. The pre-fix control that asserted the old contract was
// deleted with the fix, as T-4 requires; the MFA-account control below holds
// before and after.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// armedClear is a conversation whose next Clear meets the own-rule arm: no
// privacy_settings row (the own rule reads TRUE), and a password account
// unless mfa asks for a TOTP factor instead.
type armedClear struct {
	ts     *testhelpers.TestServer
	actor  testhelpers.TestUser
	convID string
}

func armOwnRuleClear(t *testing.T, mfa bool) armedClear {
	t.Helper()
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_pw_actor")
	peer := ts.CreateTestUser(t, "clear_pw_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	if mfa {
		enableVisibilityTOTP(t, ts, actor)
	}
	return armedClear{ts: ts, actor: actor, convID: convID}
}

func (a armedClear) clear(body map[string]string) (int, map[string]any) {
	w := a.ts.DoRequest("POST", pathDMConversationsPrefix+a.convID+"/clear", body, testhelpers.AuthHeaders(a.actor.AccessToken))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out) // a non-JSON body leaves out nil, and the assertions on it fail
	return w.Code, out
}

func (a armedClear) ranges(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, a.ts.DB.QueryRow(
		`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`,
		a.actor.ID, a.convID).Scan(&n))
	return n
}

// regression for #3509 (Codex P1)
func TestDMClearOwnRule_RefusesCurrentPassword_Repro3509(t *testing.T) {
	a := armOwnRuleClear(t, false)

	// Precondition, and the arm being reached: with no step-up field Clear
	// answers the own-rule password prompt. That holds before and after the
	// fix, so it proves the fixture is on the own rule for an account with no
	// MFA, without depending on how the password is later refused.
	status, body := a.clear(map[string]string{})
	require.Equal(t, http.StatusForbidden, status, "precondition: %v", body)
	require.Equal(t, true, body["password_required"], "precondition: %v", body)
	require.Zero(t, a.ranges(t), "precondition: the prompt must not clear")

	status, body = a.clear(map[string]string{"current_password": testhelpers.TestAuthPlaintext})

	assert.Equal(t, http.StatusBadRequest, status,
		"a body carrying current_password must be refused with 400: the password reaches only the mint endpoint; got %d %v",
		status, body)
	assert.Zero(t, a.ranges(t), "a request that carried the account password must not clear the history")
}

// Control, valid before and after: an account with inline MFA, sent no step-up
// field, gets the MFA refusal and never the password prompt.
func TestDMClearOwnRule_MFAAccountRefusalShape(t *testing.T) {
	a := armOwnRuleClear(t, true)

	status, body := a.clear(map[string]string{})

	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, true, body["mfa_required"])
	assert.NotContains(t, body, "password_required")
	assert.Zero(t, a.ranges(t))
}
