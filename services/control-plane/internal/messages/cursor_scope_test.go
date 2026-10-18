package messages_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A `before` cursor naming a message in a channel the caller cannot read must
// behave like an unknown id. If the cursor lookup were not scoped to the
// requested channel, the page returned would reveal whether the foreign
// message exists and roughly when it was sent.
func TestGetMessagesCursorIgnoresForeignMessage(t *testing.T) {
	ts := setupTS(t)
	member := ts.CreateTestUser(t, "cursormember")
	stranger := ts.CreateTestUser(t, "cursorstranger")
	channelID := ts.CreateTestChannel(t, ts.CreateTestServer(t, member.ID, "Cursor Server"), "general")
	foreignChannelID := ts.CreateTestChannel(t, ts.CreateTestServer(t, stranger.ID, "Foreign Server"), "general")

	seed := func(channelID, userID, age string) string {
		var id string
		require.NoError(t, ts.DB.QueryRow(
			`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, 'cursor msg', 1, NOW() - $3::interval, NOW())
			 RETURNING id`,
			channelID, userID, age,
		).Scan(&id))
		return id
	}
	seed(channelID, member.ID, "3 hours")
	seed(channelID, member.ID, "2 hours")
	newest := seed(channelID, member.ID, "1 minute")
	foreign := seed(foreignChannelID, stranger.ID, "1 hour")

	for _, path := range []string{bulkPath, "/messages"} {
		t.Run(path, func(t *testing.T) {
			page := func(before string) int {
				w := ts.DoRequest("GET", channelsPrefix+channelID+path+"?before="+before, nil,
					testhelpers.AuthHeaders(member.AccessToken))
				require.Equal(t, http.StatusOK, w.Code)
				var resp struct {
					Messages []json.RawMessage `json:"messages"`
				}
				testhelpers.ParseJSON(t, w, &resp)
				return len(resp.Messages)
			}
			// Positive control: an in-channel cursor still paginates.
			assert.Equal(t, 2, page(newest))
			assert.Equal(t, 0, page(foreign))
		})
	}
}
