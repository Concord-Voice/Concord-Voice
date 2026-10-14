package channels_test

// #3459 red-team F2. UpdateExpiration used to validate :id with uuid.Parse and then forward
// the RAW spelling, so the expiration_event frame named the channel as the request spelled
// it. The desktop store matches channels by exact string and dropped the frame, and since
// #3459 that frame is the only live carrier of include_pinned: members kept reading "except
// pinned messages" after pins began to expire. These cases pin the canonical spelling on the
// wire for each spelling PostgreSQL folds onto the same row.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestChannelExpiration_FrameCarriesCanonicalChannelIDForEverySpelling(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "exp_spell_owner")
	member := ts.CreateTestUser(t, "exp_spell_member")
	serverID := ts.CreateTestServer(t, owner.ID, "exp-spell")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	auth := testhelpers.AuthHeaders(owner.AccessToken)
	tap := dialChannelFrameObserver(t, ts, member, serverID, channelID)

	spellings := []struct {
		name string
		id   string
	}{
		{"canonical", channelID},
		{"uppercase", strings.ToUpper(channelID)},
		{"braced", "{" + channelID + "}"},
	}
	// Alternate the pin setting so every request is a real policy change and emits a frame.
	include := false
	for _, spelling := range spellings {
		include = !include
		tap.reset()
		w := ts.DoRequest(http.MethodPatch, "/api/v1/channels/"+spelling.id+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only", "include_pinned": include}, auth)
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", spelling.name, w.Body.String())
		require.Eventually(t, func() bool { return len(tap.ofTypes("expiration_event")) == 1 },
			3*time.Second, 20*time.Millisecond, "%s: the member must receive the frame", spelling.name)
		frame := tap.ofTypes("expiration_event")[0]
		assert.Equal(t, channelID, frame.Data["channel_id"],
			"%s: the frame must name the channel canonically, or the member's store drops it", spelling.name)
		assert.Equal(t, include, frame.Data["include_pinned"], spelling.name)
	}
}
