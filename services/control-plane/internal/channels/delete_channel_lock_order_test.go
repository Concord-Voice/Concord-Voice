package channels

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteChannel_LocksBeforeAuthoritativeRereadAndDelete(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "handlers.go"))
	require.NoError(t, err)
	body := string(source)
	start := strings.Index(body, "func (h *Handler) deleteChannelTx(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(body[start:], "\nfunc ")
	require.Greater(t, end, 0)
	body = body[start : start+end]
	guard := strings.Index(body, "credepoch.GuardTx(")
	reread := strings.Index(body, "SELECT server_id, type = 'voice' FROM channels WHERE id = $1 FOR UPDATE")
	deleteStatement := strings.Index(body, "DELETE FROM channels")
	require.GreaterOrEqual(t, guard, 0)
	require.Greater(t, reread, guard)
	require.Greater(t, deleteStatement, reread)
}

func TestDeleteChannelGroup_LocksParentBeforeBoundedChildScan(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "groups.go"))
	require.NoError(t, err)
	body := string(source)
	start := strings.Index(body, "func (h *Handler) lockAndAuthorizeChannelGroupDeleteTx(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(body[start:], "\nfunc ")
	require.Greater(t, end, 0)
	body = body[start : start+end]
	lock := strings.Index(body, "lockChannelGroupForDeleteTx")
	scan := strings.Index(body, "groupedChannelStatesTx")
	require.GreaterOrEqual(t, lock, 0)
	require.GreaterOrEqual(t, scan, 0)
	assert.Less(t, lock, scan,
		"the parent row must be locked before the bounded child scan so grouped CreateChannel FK writes cannot bypass the cap")
}

func TestCreateChannel_RechecksGroupOwnershipInsideCreationTransaction(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "handlers.go"))
	require.NoError(t, err)
	body := string(source)
	start := strings.Index(body, "func (h *Handler) createChannelTx(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(body[start:], "func (h *Handler) authorizeCreateChannelTx(")
	require.Greater(t, end, 0)
	body = body[start : start+end]
	assert.Contains(t, body, "rbac.LockAuthorityPrincipalsTx(c.Request.Context(), tx, preflightMembers)")
	assert.Contains(t, body, "credepoch.GuardTx(c.Request.Context(), tx, userID")
	assert.Contains(t, body, "SELECT id FROM servers WHERE id = $1 FOR UPDATE")
	assert.Contains(t, body, "h.insertChannel(tx, channelID, req, nextPos)")
	assert.NotContains(t, body, "channel_groups")

	authorizeStart := strings.Index(string(source), "func (h *Handler) authorizeCreateChannelTx(")
	require.GreaterOrEqual(t, authorizeStart, 0)
	authorize := string(source)[authorizeStart:]
	assert.Contains(t, authorize, "SELECT id FROM channel_groups WHERE id = $1 AND server_id = $2 FOR KEY SHARE",
		"group ownership must be checked on the creation transaction, not only in preflight")
}
