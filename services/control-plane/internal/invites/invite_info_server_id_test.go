package invites_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// The AUTHENTICATED invite preview carries `server_id` (#2372).
//
// It is what lets the renderer recognise an invite to a server the user is
// already in and render "Joined" instead of a Join button whose only possible
// outcome is a 409. Without it the client has the server's NAME and nothing it
// can compare against its own membership list.
//
// Its counterpart lives next door and is the more important of the two:
// TestGetPublicInvitePreviewValidMinimalFields asserts the ANONYMOUS
// /invites/{code}/preview response does NOT carry `server_id`. That response is
// privacy-trimmed by design, and an opaque server identifier there is a
// correlation handle for an unauthenticated caller holding only a code. The two
// tests are a pair: this one would stay green if someone "helpfully" added the
// field to both handlers, and that one is what refuses it.
func TestGetInviteInfoCarriesServerID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "inviteidowner")
	member := ts.CreateTestUser(t, "inviteidmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Invite ID Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	// Read as a NON-member: this is the case the field exists for, and the one
	// where a membership-shaped response would be wrong.
	w := ts.DoRequest("GET", "/api/v1/invites/"+code, nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	assert.Equal(t, serverID, testhelpers.JSONField[string](t, body, "server_id"),
		"server_id must be the invited server's own id, not the invite's or the code's")
	assert.Equal(t, "Invite ID Server", testhelpers.JSONField[string](t, body, "server_name"))
	assert.True(t, testhelpers.JSONField[bool](t, body, "valid"))

	// The server does NOT answer the membership question itself. That is
	// deliberate and worth pinning: `useInvitePreview` caches this response for
	// five minutes, so a cached `is_member: false` would show a Join button on a
	// server the user had joined thirty seconds earlier. The renderer derives
	// membership from its own live `serverStore` instead.
	assert.NotContains(t, body, "is_member")
	assert.NotContains(t, body, "already_member")
}
