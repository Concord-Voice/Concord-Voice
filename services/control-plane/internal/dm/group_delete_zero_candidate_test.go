package dm

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// A group DM with NO active call must still fail closed when someone starts one
// between the candidate read and the conversation lock.
//
// Red-team finding, #2448 pre-PR pass. deleteGroupData routes on the
// PRE-TRANSACTION candidate count, and the empty set -- the ordinary state of a
// group DM -- went to a path that never re-read under the lock. A participant
// who started a call inside that window had dm_voice_participants, the C3
// evidence, destroyed by a transaction that recorded no obligation and
// delivered no clear frame. The guarded path's drift check was correct and
// simply not on this route, so the common case was the unprotected one.
//
// The sibling test TestDeleteGroupDataFailsClosedOnCandidateDrift covers the
// non-empty set. This one covers the empty set, which is the reachable case.
func TestDeleteGroupDataZeroCandidateDriftFailsClosed(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	// Seeded with participants but NO voice rows: candidates is empty.
	convID, _ := seedGroupCallWithParticipants(t, db, 0)
	handler, deliverer := newDMHandlerWithRail(t, db, convID)

	var once sync.Once
	handler.afterCandidateReadHook = func() {
		// Someone answers the call in the window between the plain-connection
		// read and the conversation lock.
		once.Do(func() { joinExtraVoiceParticipant(t, db, convID) })
	}

	err := handler.deleteGroupData(context.Background(), convID)
	require.ErrorIs(t, err, errCandidateSetDrifted,
		"a call started inside the window must refuse the deletion, not destroy the evidence")

	require.Equal(t, 1,
		countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID),
		"nothing may be deleted when a call appears mid-flight")
	require.Equal(t, 1,
		countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1`, convID),
		"the C3 evidence must survive the refusal")
	require.Empty(t, deliverer.subjectsCleared())
}

// Control: with no drift, an empty-candidate deletion still succeeds. Without
// this, the fix above could be satisfied by refusing every zero-candidate
// deletion, which would break ordinary group-DM deletion entirely.
func TestDeleteGroupDataZeroCandidateStillDeletesWithoutDrift(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, 0)
	handler, _ := newDMHandlerWithRail(t, db, convID)

	require.NoError(t, handler.deleteGroupData(context.Background(), convID))
	require.Zero(t,
		countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID),
		"an undisturbed deletion must still delete")
}
