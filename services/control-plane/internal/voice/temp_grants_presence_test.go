//go:build integration

package voice

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Spec §12 test 10, first half: a real temp-grant revoke produces the identical
// precise clear the RBAC path produces.
func TestRevokeTemporaryChannelAccess_TempGrant_ClearsExactlyTheRevokedViewer(t *testing.T) {
	env := newTempGrantPresenceEnv(t)
	defer env.Close()

	senderID := env.joinVoice(t, env.channelID)
	viewer := env.grantTemporaryAccess(t)
	retained := env.addPermanentViewer(t)

	require.NoError(t, env.manager.revokeTemporaryChannelAccess(
		context.Background(), env.serverID, env.channelID, viewer, "system",
	))
	env.waitForDispatch(t)

	assert.True(t, env.wasCleared(senderID, viewer),
		"the revoked temporary viewer loses sight and must be cleared")
	assert.False(t, env.wasCleared(senderID, retained),
		"a viewer retaining sight via a permanent grant is never false-cleared")
}

// Spec §12 test 10, second half: the RowsAffected == 0 permanent-grant branch is
// a TOTAL no-op — no presence work at all.
func TestRevokeTemporaryChannelAccess_PermanentGrantOnly_IsATotalNoOp(t *testing.T) {
	env := newTempGrantPresenceEnv(t)
	defer env.Close()

	senderID := env.joinVoice(t, env.channelID)
	permanent := env.addPermanentViewer(t)

	require.NoError(t, env.manager.revokeTemporaryChannelAccess(
		context.Background(), env.serverID, env.channelID, permanent, "system",
	))
	env.waitForDispatch(t)

	assert.Zero(t, env.refreshCount(senderID))
	assert.Empty(t, env.disconnectedViewers())
	assert.True(t, env.permanentOverrideStillPresent(t, permanent),
		"the is_temporary guard must never remove a permanent grant")
}
