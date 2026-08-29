package users

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
)

// The #2992 wiring lock for the second hand-wired seam, and like the server
// -delete arm it has to run on a FAILING erasure to mean anything.
//
// A successful erasure advances the epoch with no bracket at all: it completes
// through presencehook.Complete, whose post-commit dispatch reaches
// DisconnectRichPresenceClients. An assertion on the happy path is therefore
// satisfied by machinery that shipped in #2446 and stays green with the defer
// deleted.
//
// A drain failure is the discriminator. deleteAccountTx returns an error, and
// presencehook.Abandon is a NO-OP for CauseWriteFailed -- that cause proves no
// commit happened, so nothing disconnects and nothing else can move the epoch.
// The bracket, taken before BeginTx, is the only candidate left.
//
// Account erasure cannot route through graphpresence.WithGatedTx, so T2's choke
// point does not cover it: delete_account.go documents that it already holds the
// same sender-gate stripe via presenceHistory.WithSender and would self-deadlock
// on a Go channel with no timeout and no detector.
func TestDeleteAccountBracketsTheFenceEvenWhenTheErasureRollsBack(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")

	drainErr := errors.New("forced active-category drain failure")
	service := newAccountServiceWithDrain(t, db, &stubActivePlanDrain{drainErr: drainErr})

	hub := websocket.NewHub(nil, nil)
	require.False(t, service.AudienceFenceWired(),
		"an unwired service must report the fence missing, or the boot guard is a tautology")
	service.SetAudienceFence(hub)
	require.True(t, service.AudienceFenceWired())

	beforeEpoch := hub.PresenceAuthzEpochForTest()
	require.Zero(t, hub.PresenceAuthzOpenForTest(),
		"precondition: no revocation may be in flight before the erasure")

	err := service.DeleteAccount(context.Background(), userID.String())
	require.ErrorIs(t, err, drainErr, "control: the erasure must actually have failed")
	require.Equal(t, 1, countUsers(t, db, userID),
		"control: a rolled-back erasure must leave the user in place")

	assert.Greater(t, hub.PresenceAuthzEpochForTest(), beforeEpoch,
		"the bracket is taken before BeginTx, so it must have advanced the epoch even "+
			"though this erasure rolled back and Abandon disconnected nobody -- if this "+
			"is equal, the defer is missing")
	assert.Zero(t, hub.PresenceAuthzOpenForTest(),
		"a rolled-back erasure must still RELEASE the bracket: a leaked open count "+
			"suppresses base presence hub-wide, permanently")
}

// The service must remain usable unwired. A replica without a hub still erases
// accounts; it simply degrades to the pre-#2992 post-commit signal. Panicking
// there would make the fence a liveness dependency of account deletion.
func TestDeleteAccountToleratesAnUnwiredFence(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)

	service := newAccountServiceWithDrain(t, db, &stubActivePlanDrain{})
	require.False(t, service.AudienceFenceWired())

	assert.NotPanics(t, func() {
		_ = service.DeleteAccount(context.Background(), userID.String())
	})
}
