package messages_test

// A self-purge's password step-up token is bound to ITS route (#3509): a token
// minted for the channel purge is refused by the server purge without being
// consumed, and the server purge's own token confirms it.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// Mutant killed: passing PurposeChannelPurge (or any purpose but its own)
// from PurgeServer's own-rule confirmation — the channel token would confirm
// the server purge.
func TestSelfPurge_ServerOwnRuleTakesOnlyItsOwnToken(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.seed(t, w.channelID, w.owner, 16)
	self := func(token string) map[string]any {
		return map[string]any{"range": "all", "target_user_id": w.owner.ID, "step_up_token": token}
	}
	channelToken := s.mintToken(t, w.owner.ID, stepup.PurposeChannelPurge)

	refused := s.purgeServer(t, w.owner.ID, w.serverID, self(channelToken))
	body := requireSoftLockRefusal(t, refused, "step_up_token_invalid")
	assert.Equal(t, true, body["password_required"])
	assert.Equal(t, 16, s.countBy(t, w.owner.ID), "a refused self-purge deletes nothing")
	assert.Zero(t, s.auditRows(t, w.serverID))

	ok := s.purgeServer(t, w.owner.ID, w.serverID, self(s.mintToken(t, w.owner.ID, stepup.PurposeServerPurge)))
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Zero(t, s.countBy(t, w.owner.ID))
	assert.Empty(t, s.verifier.calls(), "an account without MFA never reaches the MFA verifier")

	// The refusal did not consume the channel token: it still confirms the
	// route it was minted for.
	s.seed(t, w.channelID, w.owner, 16)
	channel := s.purgeChannel(t, w.owner.ID, w.channelID, map[string]any{"range": "all", "step_up_token": channelToken})
	require.Equal(t, http.StatusOK, channel.Code, channel.Body.String())
}
