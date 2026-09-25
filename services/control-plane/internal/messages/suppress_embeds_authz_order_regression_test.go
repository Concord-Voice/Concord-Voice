package messages_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendSuppressibleTestMessage sends a message on behalf of the owner and
// returns its ID. The server must already have allow_embedded_content = TRUE
// so the message starts with embeds_suppressed = false.
func sendSuppressibleTestMessage(t *testing.T, ts *testhelpers.TestServer, owner testhelpers.TestUser, channelID string) string {
	t.Helper()

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, "fixture setup: message send must succeed")

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg, ok := body["message"].(map[string]interface{})
	require.True(t, ok, "fixture setup: response must carry a message object")
	id, ok := msg["id"].(string)
	require.True(t, ok, "fixture setup: message must carry an id")
	return id
}

// embedsSuppressedFor reads the current embeds_suppressed column for a message.
func embedsSuppressedFor(t *testing.T, ts *testhelpers.TestServer, messageID string) bool {
	t.Helper()

	var suppressed bool
	err := ts.DB.QueryRow(`SELECT embeds_suppressed FROM messages WHERE id = $1`, messageID).Scan(&suppressed)
	require.NoError(t, err, "fixture guard: message row must exist")
	return suppressed
}

// TestSuppressEmbeds_UnprivilegedCaller_CannotObserveSuppressedState pins the
// property that whether a message is already embed-suppressed must never be
// observable to a caller who lacks PermManageAllMessages: the response for an
// already-suppressed message and an unsuppressed one must be identical, and
// neither may ever be 200 for an unprivileged caller.
//
// regression: authorize before branching on embeds_suppressed (Semgrep AI Logic Flaw, router:1049)
func TestSuppressEmbeds_UnprivilegedCaller_CannotObserveSuppressedState(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sfxauthzowner")
	member := ts.CreateTestUser(t, "sfxauthzmember")
	nonMember := ts.CreateTestUser(t, "sfxauthznonmem")
	serverID := ts.CreateTestServer(t, owner.ID, "SuppressAuthzOrder Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Enable embeds so both messages start unsuppressed.
	_, err := ts.DB.Exec(`UPDATE servers SET allow_embedded_content = TRUE WHERE id = $1`, serverID)
	require.NoError(t, err)

	msg1ID := sendSuppressibleTestMessage(t, ts, owner, channelID)
	msg2ID := sendSuppressibleTestMessage(t, ts, owner, channelID)

	// Positive control: the owner holds PermManageAllMessages via server
	// ownership, so the authorized suppress request must reach 200 and this
	// is the fixture's proof the path is reachable at all.
	controlResp := ts.DoRequest("POST", pathAPIMsgSlash+msg1ID+pathSuppressEmbeds, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, controlResp.Code, "positive control: owner must be able to suppress embeds")

	// Fixture-validity guard: msg1 is now suppressed, msg2 is not.
	require.True(t, embedsSuppressedFor(t, ts, msg1ID), "fixture guard: msg1 must be suppressed after the owner's request")
	require.False(t, embedsSuppressedFor(t, ts, msg2ID), "fixture guard: msg2 must remain unsuppressed")

	cases := []struct {
		name  string
		token string
	}{
		{"member", member.AccessToken},
		{"non_member", nonMember.AccessToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := testhelpers.AuthHeaders(tc.token)

			// msg1 is already suppressed; msg2 is not. Neither caller has
			// PermManageAllMessages.
			respMsg1 := ts.DoRequest("POST", pathAPIMsgSlash+msg1ID+pathSuppressEmbeds, nil, headers)
			respMsg2 := ts.DoRequest("POST", pathAPIMsgSlash+msg2ID+pathSuppressEmbeds, nil, headers)

			assert.Equal(t, respMsg2.Code, respMsg1.Code,
				"a caller without PermManageAllMessages must get the same status for an already-suppressed message as for an unsuppressed one")
			assert.Equal(t, respMsg2.Body.String(), respMsg1.Body.String(),
				"a caller without PermManageAllMessages must get the same body for an already-suppressed message as for an unsuppressed one")
			assert.NotEqual(t, http.StatusOK, respMsg1.Code,
				"a caller without PermManageAllMessages must never receive 200 by probing an already-suppressed message")

			// msg2 must remain unsuppressed: this caller was never authorized
			// to suppress it.
			assert.False(t, embedsSuppressedFor(t, ts, msg2ID),
				"an unauthorized suppress request must not suppress embeds")
		})
	}
}
