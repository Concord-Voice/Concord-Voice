//go:build integration

package voice

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Spec §12 test 10, extended to EVERY temporary-SBAC trigger path.
//
// Three independent construction sites each own their own tempGrantManager, so
// wiring the #2445 capture into one of them says nothing about the other two —
// exactly the gap fixed by 3b0add975. One test per path means a fourth
// construction site added unwired fails loudly here instead of silently
// shipping a revocation that never clears a stale Rich Presence badge.
//
//	1. the NATS subscriber      (nats.go            -> NATSSubscriber.SetPresenceRecheck)
//	2. the REST RevokeTempAccess (handlers.go       -> (*Handler).SetPresenceRecheck)
//	3. the nightly orphan sweep  (temp_grant_sweep.go -> (*TempGrantSweeper).SetPresenceRecheck)
//
// Each case asserts the OUTCOME (the losing viewer is cleared through that
// path's own manager), not merely that a field was assigned.

func TestNATSSubscriber_SetPresenceRecheck_WiresItsOwnTempGrantManager(t *testing.T) {
	env := newTempGrantPresenceEnv(t)
	defer env.Close()

	subscriber := NewNATSSubscriber(env.db, env.log, env.hub, nil, env.redis, env.resolver, nil)
	subscriber.SetPresenceRecheck(env.recheck)
	require.NotNil(t, subscriber.tempGrant, "the subscriber owns a tempGrantManager")

	senderID := env.joinVoice(t, env.channelID)
	viewer := env.grantTemporaryAccess(t)

	require.NoError(t, subscriber.tempGrant.revokeTemporaryChannelAccess(
		context.Background(), env.serverID, env.channelID, viewer, "system",
	))
	env.waitForDispatch(t)

	assert.True(t, env.wasCleared(senderID, viewer),
		"the NATS-driven revocation must capture and clear like every other path")
}

func TestVoiceHandler_SetPresenceRecheck_WiresItsOwnTempGrantManager(t *testing.T) {
	env := newTempGrantPresenceEnv(t)
	defer env.Close()

	handler := NewHandler(HandlerDeps{
		DB: env.db, Log: env.log, Hub: env.hub, Resolver: env.resolver,
	})
	handler.SetPresenceRecheck(env.recheck)
	require.NotNil(t, handler.tempGrant, "the REST handler owns a tempGrantManager")

	senderID := env.joinVoice(t, env.channelID)
	viewer := env.grantTemporaryAccess(t)

	require.NoError(t, handler.tempGrant.revokeTemporaryChannelAccess(
		context.Background(), env.serverID, env.channelID, viewer, env.ownerID,
	))
	env.waitForDispatch(t)

	assert.True(t, env.wasCleared(senderID, viewer),
		"RevokeTempAccess must capture and clear like every other path")
}

// The sweep is the strongest of the three: it runs the REAL orphan selection
// query and converges on revokeTemporaryChannelAccess by itself, so nothing
// about the trigger is simulated.
func TestTempGrantSweeper_SetPresenceRecheck_CapturesThroughTheOrphanSweep(t *testing.T) {
	env := newTempGrantPresenceEnv(t)
	defer env.Close()

	sweeper := NewTempGrantSweeper(env.db, env.log, env.hub, env.resolver, nil)
	sweeper.SetPresenceRecheck(env.recheck)

	senderID := env.joinVoice(t, env.channelID)
	// An ORPHAN: a temporary grant whose holder has no live voice_participants
	// row in that channel. grantTemporaryAccess backdates granted_at past the
	// sweeper's 60-second grace, so it is eligible on this pass.
	orphan := env.grantTemporaryAccess(t)
	retained := env.addPermanentViewer(t)

	revoked, err := sweeper.sweepOrphanedTempGrants(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, revoked, 1, "the backdated orphan is eligible for this sweep")
	env.waitForDispatch(t)

	assert.True(t, env.wasCleared(senderID, orphan),
		"a swept orphan loses sight and must be cleared")
	assert.False(t, env.wasCleared(senderID, retained),
		"the sweep never touches a permanent grant, so its holder is never false-cleared")
	assert.True(t, env.permanentOverrideStillPresent(t, retained))
}
