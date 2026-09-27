package channels_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A topology change committed after preflight must not be replaced by a stale
// authority mutation. The visibility advisory lock makes the interleaving
// deterministic: the request preflights, then waits while this transaction
// removes the authority source.
func holdGroupRemoval(t *testing.T, ts *testhelpers.TestServer, serverID, groupID string) *sql.Tx {
	t.Helper()
	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	_, err = tx.Exec(`DELETE FROM channel_groups WHERE id = $1 AND server_id = $2`, groupID, serverID)
	require.NoError(t, err)
	return tx
}

func holdChannelRemoval(t *testing.T, ts *testhelpers.TestServer, serverID, channelID string) *sql.Tx {
	t.Helper()
	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	_, err = tx.Exec(`DELETE FROM channels WHERE id = $1 AND server_id = $2`, channelID, serverID)
	require.NoError(t, err)
	return tx
}

func TestUpdateChannel_LiveAuthorityGroupRemovalRejectsStaleMove(t *testing.T) {
	ts, actorID, serverID, _, h := newStaleChannelMutationActor(t)
	groupID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'live-update', 0)`, groupID, serverID)
	require.NoError(t, err)
	channelID := ts.CreateTestChannel(t, serverID, "live-update-channel")
	assignChannelToCategory(t, ts, channelID, groupID, true)

	demolition := holdGroupRemoval(t, ts, serverID, groupID)
	completed := invokeStaleChannelMutation(t, actorID, gin.Params{{Key: "id", Value: channelID}}, channels.UpdateChannelRequest{
		Name: "must-not-commit", Type: "voice", GroupID: &groupID,
	}, func(h *channels.Handler, c *gin.Context) { h.UpdateChannel(c) }, h)
	require.NoError(t, demolition.Commit())
	w := <-completed
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var name string
	require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&name))
	assert.Equal(t, "live-update-channel", name)
}

func TestDeleteChannelGroup_LiveAuthorityGroupRemovalRejectsStaleDelete(t *testing.T) {
	ts, actorID, serverID, _, h := newStaleChannelMutationActor(t)
	groupID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'live-delete', 0)`, groupID, serverID)
	require.NoError(t, err)
	channelID := ts.CreateTestChannel(t, serverID, "live-delete-channel")
	assignChannelToCategory(t, ts, channelID, groupID, true)

	demolition := holdGroupRemoval(t, ts, serverID, groupID)
	completed := invokeStaleChannelMutation(t, actorID, gin.Params{{Key: "group_id", Value: groupID}}, nil,
		func(h *channels.Handler, c *gin.Context) { h.DeleteChannelGroup(c) }, h)
	require.NoError(t, demolition.Commit())
	w := <-completed
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var childGroup sql.NullString
	require.NoError(t, ts.DB.QueryRow(`SELECT group_id FROM channels WHERE id = $1`, channelID).Scan(&childGroup))
	assert.False(t, childGroup.Valid)
}

func TestReorderChannels_LiveAuthorityGroupRemovalRejectsStaleMove(t *testing.T) {
	ts, actorID, serverID, _, h := newStaleChannelMutationActor(t)
	groupID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'live-reorder', 0)`, groupID, serverID)
	require.NoError(t, err)
	channelID := ts.CreateTestChannel(t, serverID, "live-reorder-channel")
	assignChannelToCategory(t, ts, channelID, groupID, true)

	demolition := holdGroupRemoval(t, ts, serverID, groupID)
	completed := invokeStaleChannelMutation(t, actorID, gin.Params{{Key: "id", Value: serverID}}, channels.ReorderChannelsRequest{
		Channels: []channels.ChannelPosition{{ChannelID: channelID, GroupID: &groupID, Position: 7}},
	}, func(h *channels.Handler, c *gin.Context) { h.ReorderChannels(c) }, h)
	require.NoError(t, demolition.Commit())
	w := <-completed
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var position int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, channelID).Scan(&position))
	assert.NotEqual(t, 7, position)
}

func TestReorderChannels_LiveAuthorityChannelRemovalRejectsStaleMove(t *testing.T) {
	ts, actorID, serverID, _, h := newStaleChannelMutationActor(t)
	channelID := ts.CreateTestChannel(t, serverID, "live-reorder-deleted-channel")

	demolition := holdChannelRemoval(t, ts, serverID, channelID)
	completed := invokeStaleChannelMutation(t, actorID, gin.Params{{Key: "id", Value: serverID}}, channels.ReorderChannelsRequest{
		Channels: []channels.ChannelPosition{{ChannelID: channelID, Position: 7}},
	}, func(h *channels.Handler, c *gin.Context) { h.ReorderChannels(c) }, h)
	require.NoError(t, demolition.Commit())
	w := <-completed
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
