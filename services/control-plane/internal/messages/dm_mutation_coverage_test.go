package messages_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMMutationsRejectStaleCredentialEpoch(t *testing.T) {
	for _, action := range []string{"pin", "unpin", "reaction"} {
		t.Run(action, func(t *testing.T) {
			ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
			user := ts.CreateTestUser(t, "dmstale_mutator")
			peer := ts.CreateTestUser(t, "dmstale_peer")
			conversationID := ts.CreateDMConversation(t, user.ID, peer.ID)
			messageID := insertDMMessageDirect(t, ts, conversationID, user.ID, "stale mutation")
			if action == "unpin" {
				_, err := ts.DB.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = $1`, messageID, user.ID)
				require.NoError(t, err)
			}
			var originalContent string
			var originalVersion int
			require.NoError(t, ts.DB.QueryRow(`SELECT content, key_version FROM dm_messages WHERE id = $1`, messageID).Scan(&originalContent, &originalVersion))
			method, path := http.MethodPost, pinAPIMsg+messageID+pinPath
			var body interface{}
			switch action {
			case "unpin":
				method = http.MethodDelete
			case "reaction":
				method, path, body = http.MethodPut, reactURL(messageID), emojiBody("👍")
			}
			require.NoError(t, boundary.Arm(user.AccessToken))
			boundary.Hook.ArmArg([]string{messageEpochGuardSQL}, user.ID, nil, nil)
			w := ts.DoRequest(method, path, body, testhelpers.AuthHeaders(user.AccessToken))
			assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			requireMessageStaleBoundary(t, boundary)

			var content string
			var version, reactions int
			var pinned bool
			require.NoError(t, ts.DB.QueryRow(`SELECT content, key_version, pinned_at IS NOT NULL FROM dm_messages WHERE id = $1`, messageID).Scan(&content, &version, &pinned))
			assert.Equal(t, originalContent, content)
			assert.Equal(t, originalVersion, version)
			assert.Equal(t, action == "unpin", pinned, "a refused unpin must retain its pin")
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_message_reactions WHERE message_id = $1`, messageID).Scan(&reactions))
			assert.Zero(t, reactions)
		})
	}
}
