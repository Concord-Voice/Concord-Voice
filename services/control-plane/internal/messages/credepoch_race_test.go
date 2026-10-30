package messages_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// #2201 AC-2: an encrypted message write admitted before a destructive key
// reset after committed HTTP admission must be stopped by GuardTx — no message row may
// land under the superseded epoch.
func TestCredEpochRace_MessageSendRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "racemsg")
	serverID := ts.CreateTestServer(t, user.ID, "Race Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	stale := user.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg([]string{messageEpochGuardSQL}, user.ID, nil, nil)

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale send")
	requireMessageStaleBoundary(t, boundary)

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "no message may be recreated under the old epoch")
}

// #2201 AC-2: an encrypted message EDIT admitted before a destructive key
// reset after committed HTTP admission must be stopped by the in-transaction GuardTx —
// the ciphertext must remain unedited under the superseded epoch.
func TestCredEpochRace_MessageEditRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "raceedit")
	serverID := ts.CreateTestServer(t, user.ID, "Race Edit Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	original := testhelpers.ValidCiphertext()
	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":  channelID,
		"content":     original,
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	message, ok := body["message"].(map[string]interface{})
	require.True(t, ok, "created message must be an object")
	msgID, ok := message["id"].(string)
	require.True(t, ok, "created message must have an ID")

	stale := user.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg([]string{messageEpochGuardSQL}, user.ID, nil, nil)

	w = ts.DoRequest("PATCH", "/api/v1/messages/"+msgID, map[string]interface{}{
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale edit")
	requireMessageStaleBoundary(t, boundary)

	var content string
	require.NoError(t, ts.DB.QueryRow(`SELECT content FROM messages WHERE id = $1`, msgID).Scan(&content))
	assert.Equal(t, original, content, "ciphertext must be unedited under the old epoch")
}

const messageEpochGuardSQL = `SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`

func requireMessageStaleBoundary(t *testing.T, boundary *testhelpers.StaleBoundary) {
	t.Helper()
	boundary.RequireRotated(t)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, 1, seen, "the named handler epoch guard must be reached after successful auth commit and rotation")
}
