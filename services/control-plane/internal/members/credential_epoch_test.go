package members_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveMemberRejectsStaleCredentialEpoch(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "stale-kick-owner")
	member := ts.CreateTestUser(t, "stale-kick-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Stale Kick Server")
	channelID := ts.CreateTestChannel(t, serverID, "stale-kick-channel")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	_, err := ts.DB.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`, channelID, member.ID, []byte("stale-kick-key"))
	require.NoError(t, err)

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodDelete, memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(staleToken))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var memberExists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`, serverID, member.ID).Scan(&memberExists))
	assert.True(t, memberExists, "stale token must not remove membership")
	assert.Equal(t, 1, memberChannelKeyCount(t, ts, channelID, member.ID), "stale token must not revoke channel keys")
}

func TestBanMemberRejectsStaleCredentialEpoch(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "stale-ban-owner")
	member := ts.CreateTestUser(t, "stale-ban-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Stale Ban Server")
	channelID := ts.CreateTestChannel(t, serverID, "stale-ban-channel")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	_, err := ts.DB.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`, channelID, member.ID, []byte("stale-ban-key"))
	require.NoError(t, err)

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodPost, banPath(serverID, member.ID), nil, testhelpers.AuthHeaders(staleToken))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var memberExists, banExists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`, serverID, member.ID).Scan(&memberExists))
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`, serverID, member.ID).Scan(&banExists))
	assert.True(t, memberExists, "stale token must not remove membership")
	assert.False(t, banExists, "stale token must not create a ban")
	assert.Equal(t, 1, memberChannelKeyCount(t, ts, channelID, member.ID), "stale token must not revoke channel keys")
}

func memberChannelKeyCount(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) int {
	t.Helper()
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, userID).Scan(&count))
	return count
}
