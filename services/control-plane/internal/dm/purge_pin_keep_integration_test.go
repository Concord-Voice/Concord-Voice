package dm_test

// DM purge keeps the actor's own pins unless the request sets include_pinned
// (#3458); a peer's pinned message is never hidden, whatever the flag says.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestPurgeConversation_IncludePinned(t *testing.T) {
	for _, tc := range []struct {
		name          string
		includePinned any // nil = omitted
		wantVisible   []string
		wantDeleted   int
		wantHidden    int
	}{
		{"absent keeps both pins", nil, []string{"alice-pin", "bob-pin"}, 1, 1},
		{"false keeps both pins", false, []string{"alice-pin", "bob-pin"}, 1, 1},
		{"true deletes own pins; the peer's stay visible", true, []string{"bob-pin"}, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			alice := ts.CreateTestUser(t, "pinpurge_alice")
			bob := ts.CreateTestUser(t, "pinpurge_bob")
			convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
			insertDMMsg(t, ts, convID, alice.ID, "alice-plain")
			insertDMMsg(t, ts, convID, alice.ID, "alice-pin")
			insertDMMsg(t, ts, convID, bob.ID, "bob-plain")
			insertDMMsg(t, ts, convID, bob.ID, "bob-pin")
			_, err := ts.DB.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = user_id
				WHERE conversation_id = $1 AND content LIKE '%-pin'`, convID)
			require.NoError(t, err)

			body := map[string]any{"range": "all", "current_password": alice.Password}
			if tc.includePinned != nil {
				body["include_pinned"] = tc.includePinned
			}
			w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID), body, testhelpers.AuthHeaders(alice.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			var resp struct {
				DeletedCount int `json:"deleted_count"`
				HiddenCount  int `json:"hidden_count"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantDeleted, resp.DeletedCount)
			assert.Equal(t, tc.wantHidden, resp.HiddenCount)
			assert.ElementsMatch(t, tc.wantVisible, fetchVisibleMessages(t, ts, convID, alice.AccessToken))
		})
	}
}
