package messages_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// An administrator has ManageAllMessages, so the transaction-local mutation
// fence must still verify authorship before allowing an edit.
func TestUpdateMessageManageAllCannotEditForeignMessage(t *testing.T) {
	ts, _, serverID, _, messageID := setupWithMessage(t)
	admin := ts.CreateTestUser(t, "messages-coverage-admin")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	var original string
	require.NoError(t, ts.DB.QueryRow(`SELECT content FROM messages WHERE id = $1`, messageID).Scan(&original))

	w := ts.DoRequest(http.MethodPatch, "/api/v1/messages/"+messageID, map[string]interface{}{
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	var content string
	require.NoError(t, ts.DB.QueryRow(`SELECT content FROM messages WHERE id = $1`, messageID).Scan(&content))
	require.Equal(t, original, content, "foreign edit must not mutate the message")
}
