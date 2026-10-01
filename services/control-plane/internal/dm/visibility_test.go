package dm_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMVisibility_HideIsPrivateAndUnhideRestoresListEntry(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_actor")
	peer := ts.CreateTestUser(t, "visibility_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	path := pathDMConversationsPrefix + convID + "/hide"

	w := ts.DoRequest("POST", path, nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var first map[string]interface{}
	testhelpers.ParseJSON(t, w, &first)
	firstHiddenAt := testhelpers.JSONField[string](t, first, "hidden_at")

	w = ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(actor.AccessToken))
	var actorList map[string]interface{}
	testhelpers.ParseJSON(t, w, &actorList)
	assert.Empty(t, testhelpers.JSONField[[]interface{}](t, actorList, "conversations"))

	w = ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(peer.AccessToken))
	var peerList map[string]interface{}
	testhelpers.ParseJSON(t, w, &peerList)
	assert.Len(t, testhelpers.JSONField[[]interface{}](t, peerList, "conversations"), 1)

	w = ts.DoRequest("POST", path, nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var retry map[string]interface{}
	testhelpers.ParseJSON(t, w, &retry)
	assert.Equal(t, firstHiddenAt, testhelpers.JSONField[string](t, retry, "hidden_at"), "hide retry preserves timestamp")

	w = ts.DoRequest("DELETE", path, nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var unhidden map[string]interface{}
	testhelpers.ParseJSON(t, w, &unhidden)
	value, present := unhidden["hidden_at"]
	assert.True(t, present)
	assert.Nil(t, value)

	w = ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(actor.AccessToken))
	var restored map[string]interface{}
	testhelpers.ParseJSON(t, w, &restored)
	assert.Len(t, testhelpers.JSONField[[]interface{}](t, restored, "conversations"), 1)
}

func TestDMVisibility_HideRejectsMalformedAndUnauthorizedRequests(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_invalid")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/hide", nil, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = ts.DoRequest("POST", pathDMConversationsPrefix+"00000000-0000-0000-0000-000000000001/hide", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestDMVisibility_ClearRejectsMalformedRequest(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_clear_invalid")
	body, err := json.Marshal(map[string]string{"password": "unused"})
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/clear", body, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDMVisibility_ClearIsPrivateAndHidesOwnAndPeerHistory(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_actor")
	peer := ts.CreateTestUser(t, "clear_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	old := time.Now().UTC().Add(-time.Hour)
	for _, message := range []struct{ user, body string }{{actor.ID, "actor-old"}, {peer.ID, "peer-old"}} {
		_, err := ts.DB.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at) VALUES ($1, $2, $3, 'text', $4)`, convID, message.user, message.body, old)
		require.NoError(t, err)
	}
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, false) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = false`, actor.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages", nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var actorBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &actorBody)
	assert.Empty(t, testhelpers.JSONField[[]interface{}](t, actorBody, "messages"))

	w = ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages", nil, testhelpers.AuthHeaders(peer.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var peerBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &peerBody)
	assert.Len(t, testhelpers.JSONField[[]interface{}](t, peerBody, "messages"), 2)

	var includesOwn bool
	require.NoError(t, ts.DB.QueryRow(`SELECT includes_own FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&includesOwn))
	assert.True(t, includesOwn)
}

func TestDMVisibility_ClearRequiresFactorWhenSettingMissing(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_requires_factor")
	peer := ts.CreateTestUser(t, "clear_requires_factor_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`DELETE FROM privacy_settings WHERE user_id = $1`, actor.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "factor rejection must not create a range")
}

func TestDMVisibility_ClearUsesUniformNotFoundForUnknownAndNonMember(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_notfound_actor")
	peer := ts.CreateTestUser(t, "clear_notfound_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	nonMember := ts.CreateTestUser(t, "clear_notfound_nonmember")

	unknown := ts.DoRequest("POST", pathDMConversationsPrefix+"00000000-0000-0000-0000-000000000001/clear", map[string]string{}, testhelpers.AuthHeaders(nonMember.AccessToken))
	nonMemberResponse := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(nonMember.AccessToken))
	assert.Equal(t, http.StatusNotFound, unknown.Code)
	assert.Equal(t, http.StatusNotFound, nonMemberResponse.Code)
	assert.Equal(t, `{"error":"Conversation not found"}`, unknown.Body.String())
	assert.Equal(t, unknown.Body.String(), nonMemberResponse.Body.String(), "unknown and non-member Clear responses must be indistinguishable")
}

func TestDMVisibility_UnhideUsesUniformNotFoundForUnknownAndNonMember(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "unhide_notfound_actor")
	peer := ts.CreateTestUser(t, "unhide_notfound_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	nonMember := ts.CreateTestUser(t, "unhide_notfound_nonmember")
	auth := testhelpers.AuthHeaders(nonMember.AccessToken)

	unknown := ts.DoRequest("DELETE", pathDMConversationsPrefix+"00000000-0000-0000-0000-000000000001/hide", nil, auth)
	nonMemberResponse := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+"/hide", nil, auth)
	assert.Equal(t, http.StatusNotFound, unknown.Code)
	assert.Equal(t, http.StatusNotFound, nonMemberResponse.Code)
	assert.Equal(t, `{"error":"Conversation not found"}`, unknown.Body.String())
	assert.Equal(t, unknown.Body.String(), nonMemberResponse.Body.String(), "unknown and non-member Unhide responses must be indistinguishable")
}

func enableVisibilityTOTP(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) func() string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Concord Voice", AccountName: user.Email})
	require.NoError(t, err)
	sealed, nonce, err := mfa.EncryptSecret([]byte(key.Secret()), make([]byte, 32))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE users SET mfa_enabled = true, mfa_methods = ARRAY['totp']::text[] WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed) VALUES ($1, $2, $3, 1, true, true)`, user.ID, sealed, nonce)
	require.NoError(t, err)
	return func() string {
		code, err := totp.GenerateCode(key.Secret(), time.Now())
		require.NoError(t, err)
		return code
	}
}

// mintPasswordStepUp exchanges the user's password for a password step-up
// token at the real mint endpoint (#3509), and returns the response.
func mintPasswordStepUp(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, password, purpose string) (int, map[string]interface{}) {
	t.Helper()
	w := ts.DoRequest("POST", "/api/v1/auth/step-up/password",
		map[string]string{"current_password": password, "purpose": purpose}, testhelpers.AuthHeaders(user.AccessToken))
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	return w.Code, body
}

// mintClearToken mints a dm.clear password token with the user's real
// password, and fails the test if the mint does not succeed.
func mintClearToken(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) string {
	t.Helper()
	status, body := mintPasswordStepUp(t, ts, user, testhelpers.TestAuthPlaintext, "dm.clear")
	require.Equal(t, http.StatusOK, status, "mint: %v", body)
	return testhelpers.JSONField[string](t, body, "step_up_token")
}

// Rewritten for #3509: the password reaches only the mint endpoint, so the
// case that paired a wrong MFA code with the correct password now pairs it
// with a valid password step-up token, minted before TOTP was enrolled (the
// mint refuses an MFA account). Either way the MFA account's code governs.
func TestDMVisibility_ClearMFAReplacesThePasswordToken(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withToken  bool
		wrongCode  bool
		wantStatus int
	}{
		{name: "valid MFA without token", wantStatus: http.StatusOK},
		{name: "valid MFA with password token", withToken: true, wantStatus: http.StatusOK},
		{name: "wrong MFA with password token", withToken: true, wrongCode: true, wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			actor := ts.CreateTestUser(t, "clear_mfa_"+tc.name[:3]+tc.name[len(tc.name)-3:])
			peer := ts.CreateTestUser(t, "clear_mfa_peer_"+tc.name[:3]+tc.name[len(tc.name)-3:])
			ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
			convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
			body := map[string]string{}
			if tc.withToken {
				body["step_up_token"] = mintClearToken(t, ts, actor)
			}
			body["mfa_code"] = enableVisibilityTOTP(t, ts, actor)()
			if tc.wrongCode {
				body["mfa_code"] = "000000"
			}
			w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", body, testhelpers.AuthHeaders(actor.AccessToken))
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
		})
	}
}

// Rewritten for #3509: a password account clears with a token the mint
// endpoint issued for the correct password; a wrong password is refused at
// the mint, and a token that matches nothing re-prompts at Clear.
func TestDMVisibility_ClearPasswordOnlyRequiresAMintedToken(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_password_actor")
	peer := ts.CreateTestUser(t, "clear_password_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	clearHistory := func(body map[string]string) (int, map[string]interface{}) {
		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", body, testhelpers.AuthHeaders(actor.AccessToken))
		var out map[string]interface{}
		testhelpers.ParseJSON(t, w, &out)
		return w.Code, out
	}

	status, body := mintPasswordStepUp(t, ts, actor, "wrong-password", "dm.clear")
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "Invalid password", body["error"])

	status, body = clearHistory(map[string]string{"step_up_token": strings.Repeat("0", 43)}) // never minted
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, true, body["password_required"])
	assert.Equal(t, true, body["step_up_token_invalid"])

	token := mintClearToken(t, ts, actor)
	status, body = clearHistory(map[string]string{"step_up_token": token})
	assert.Equal(t, http.StatusOK, status, "%v", body)
}

func TestDMVisibility_HiddenMessageCannotBeEditedOrDeleted(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "hidden_mutation_actor")
	peer := ts.CreateTestUser(t, "hidden_mutation_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	msgID := insertDMMessage(t, ts, convID, actor.ID, "hidden mutation")
	_, err := ts.DB.Exec(`UPDATE dm_messages SET created_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, msgID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, false) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = false`, actor.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken)).Code)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{"content": testhelpers.ValidCiphertext(), "key_version": 1}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages?before="+msgID, nil, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var cursorBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &cursorBody)
	assert.Empty(t, testhelpers.JSONField[[]interface{}](t, cursorBody, "messages"), "a hidden cursor cannot reveal history")
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, msgID).Scan(&count))
	assert.Equal(t, 1, count, "hidden message remains persisted")

	visibleID := insertDMMessage(t, ts, convID, actor.ID, "visible mutation")
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+visibleID, map[string]interface{}{"content": testhelpers.ValidCiphertext(), "key_version": 1}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "visible message reaches the edit transaction")
	w = ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+visibleID, nil, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "visible message reaches the delete transaction")
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, visibleID).Scan(&count))
	assert.Zero(t, count)
}

func TestDMVisibility_HiddenCursorDoesNotExposeOlderVisibleMessage(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "hidden_cursor_actor")
	peer := ts.CreateTestUser(t, "hidden_cursor_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)

	now := time.Now().UTC().Truncate(time.Microsecond)
	olderVisibleID := insertDMMessage(t, ts, conversationID, actor.ID, "older visible message")
	hiddenCursorID := insertDMMessage(t, ts, conversationID, peer.ID, "hidden cursor message")
	_, err := ts.DB.Exec(`UPDATE dm_messages SET created_at = $2 WHERE id = $1`, olderVisibleID, now.Add(-2*time.Hour))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE dm_messages SET created_at = $2 WHERE id = $1`, hiddenCursorID, now.Add(-time.Hour))
	require.NoError(t, err)
	// Legacy finite ranges hide peer messages only. The cursor falls inside the
	// range, while the older actor message remains visible without a cursor.
	_, err = ts.DB.Exec(`INSERT INTO dm_message_hidden_ranges
		(user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, $3, $4, FALSE)`, actor.ID, conversationID, now.Add(-90*time.Minute), now.Add(-30*time.Minute))
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodGet, pathDMConversationsPrefix+conversationID+"/messages", nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var allMessages map[string]interface{}
	testhelpers.ParseJSON(t, w, &allMessages)
	visible := testhelpers.JSONField[[]interface{}](t, allMessages, "messages")
	require.Len(t, visible, 1, "the older message proves the cursor response is not vacuously empty")

	w = ts.DoRequest(http.MethodGet, pathDMConversationsPrefix+conversationID+"/messages?before="+hiddenCursorID, nil, testhelpers.AuthHeaders(actor.AccessToken))
	// The established API contract is an empty page for a hidden cursor. It
	// must not use that hidden anchor to return the otherwise-visible history.
	require.Equal(t, http.StatusOK, w.Code)
	var cursorBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &cursorBody)
	assert.Empty(t, testhelpers.JSONField[[]interface{}](t, cursorBody, "messages"))
}

// TestDMVisibility_ClearP1EmailOnlyAccountPasswordAlone locks policy P1 on
// Clear: the MFA branch applies only to an inline-verifiable factor read from
// the factor tables, so an email/SMS-only account proves itself with its
// password — through a minted token since #3509 — rather than being asked for
// a code it has nowhere to enter.
func TestDMVisibility_ClearP1EmailOnlyAccountPasswordAlone(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_p1_email")
	peer := ts.CreateTestUser(t, "clear_p1_email_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{email}' WHERE id = $1`, actor.ID)
	require.NoError(t, err)

	// The mint applies P1 too: an email-only account is not an MFA account,
	// so its password mints a token (#3509).
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear",
		map[string]string{"step_up_token": mintClearToken(t, ts, actor)}, testhelpers.AuthHeaders(actor.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestDMVisibility_ClearP1StaleFlagsStillRequireCode: a confirmed TOTP factor
// takes the MFA branch even when the denormalized flags say MFA is off —
// otherwise a password alone clears a TOTP account's history.
func TestDMVisibility_ClearP1StaleFlagsStillRequireCode(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_p1_stale")
	peer := ts.CreateTestUser(t, "clear_p1_stale_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	enableVisibilityTOTP(t, ts, actor)
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = FALSE, mfa_methods = '{}' WHERE id = $1`, actor.ID)
	require.NoError(t, err)

	// No factor at all (#3509: a password no longer reaches Clear): the P1
	// read alone decides the refusal is the MFA prompt.
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear",
		map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["mfa_required"])
	assert.Equal(t, []interface{}{"totp"}, body["methods"])
}
