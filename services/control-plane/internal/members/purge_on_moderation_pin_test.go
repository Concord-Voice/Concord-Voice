package members_test

// Kick and ban purges keep the target's pinned messages unless the request
// sets include_pinned (#3458). Absent and false must behave identically, since
// a client older than the option never sends it.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestModerationPurge_IncludePinned(t *testing.T) {
	cases := []struct {
		name          string
		includePinned any // nil = omitted from the body
		wantPinKept   bool
	}{
		{"absent keeps pins", nil, true},
		{"false keeps pins", false, true},
		{"true deletes pins", true, false},
	}
	for _, reason := range []string{"ban", "kick"} {
		for _, tc := range cases {
			t.Run(reason+"/"+tc.name, func(t *testing.T) {
				ts := setupTS(t)
				owner := ts.CreateTestUser(t, "pinmod_owner")
				victim := ts.CreateTestUser(t, "pinmod_victim")
				serverID := ts.CreateTestServer(t, owner.ID, "S")
				ch := ts.CreateTestChannel(t, serverID, "general")
				ts.AddMemberToServer(t, serverID, victim.ID, "member")
				pinned := ts.CreateTestMessage(t, ch, victim, "pinned")
				ts.CreateTestMessage(t, ch, victim, "plain")
				_, err := ts.DB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = $1`, pinned, owner.ID)
				require.NoError(t, err)

				body := map[string]interface{}{"purge_messages": true}
				if tc.includePinned != nil {
					body["include_pinned"] = tc.includePinned
				}
				method, path := http.MethodPost, banPath(serverID, victim.ID)
				if reason == "kick" {
					method, path = http.MethodDelete, memberPath(serverID, victim.ID)
				}
				w := ts.DoRequest(method, path, body, testhelpers.AuthHeaders(owner.AccessToken))

				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var resp purgeRespBody
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				require.NotNil(t, resp.Purge)
				assert.Equal(t, "completed", resp.Purge.Status)

				var remaining int
				require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = $1`, pinned).Scan(&remaining))
				assert.Equal(t, tc.wantPinKept, remaining == 1, "pinned message kept")
				assert.Equal(t, boolToInt(tc.wantPinKept), countChannelMessages(t, ts, ch), "the plain message is always purged")

				var audited bool
				require.NoError(t, ts.DB.QueryRow(`SELECT include_pinned FROM message_purges
					WHERE server_id = $1 AND target_user_id = $2 AND reason = $3`, serverID, victim.ID, reason).Scan(&audited))
				assert.Equal(t, !tc.wantPinKept, audited, "the audit row records the choice")
			})
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
