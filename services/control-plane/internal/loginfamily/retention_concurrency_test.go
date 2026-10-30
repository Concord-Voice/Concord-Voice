package loginfamily_test

import (
	"context"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/require"
)

// The blocker transaction is synchronously confirmed before the sweep starts.
// A short timeout catches accidental blocking; successful later-unit deletion
// proves that contention did not abort all maintenance or shrink the cohort.
func TestRetentionContendedWholeCohortSkippedAndLaterUnitProgresses(t *testing.T) {
	for _, lock := range []string{"principal", "family", "operation"} {
		t.Run(lock, func(t *testing.T) {
			db := database(t)
			blockedUser := testdb.CreateUser(t, db)
			_, op := retentionCohort(t, db, blockedUser, 3, false)
			retentionComplete(t, db, op)
			laterUser := testdb.CreateUser(t, db)
			_, laterOp := retentionCohort(t, db, laterUser, 2, false)
			retentionComplete(t, db, laterOp)
			blocker := transaction(t, db)
			var observed int
			switch lock {
			case "principal":
				require.NoError(t, blocker.QueryRow(`SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, blockedUser).Scan(&observed))
			case "family":
				require.NoError(t, blocker.QueryRow(`SELECT 1 FROM login_families WHERE user_id=$1 ORDER BY family_id LIMIT 1 FOR UPDATE`, blockedUser).Scan(&observed))
			case "operation":
				require.NoError(t, blocker.QueryRow(`SELECT 1 FROM login_family_revocations WHERE operation_id=$1 FOR UPDATE`, op).Scan(&observed))
			}
			require.Equal(t, 1, observed)
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			got, err := lf.SweepRetiredEvidence(bounded, db, 1)
			require.NoError(t, err)
			require.True(t, got.Incomplete)
			require.EqualValues(t, 2, got.DeletedFamilies)
			retentionEvidence(t, db, blockedUser, 3, 3, 3, 1)
			retentionEvidence(t, db, laterUser, 0, 0, 0, 0)
			require.NoError(t, blocker.Commit())
			got = retentionSweep(t, db, 1)
			require.EqualValues(t, 3, got.DeletedFamilies)
			retentionEvidence(t, db, blockedUser, 0, 0, 0, 0)
		})
	}
}

// This exercises the SQL generation-lock contract and a request-owned copy.
// It does not claim to execute HTTP signing or the operator/native gate.
func TestRetentionGenerationLockKeepsReceiptUntilCopiedAndCommitted(t *testing.T) {
	db := database(t)
	retentionTransportCleanup(t, db)
	_, conn := retentionUnregisteredClosure(t, db)
	receipt := transaction(t, db)
	_, err := receipt.Exec(`SELECT login_family_generation_lock('connection',$1)`, conn)
	require.NoError(t, err)
	var tokenDigest, bodyDigest string
	require.NoError(t, receipt.QueryRow(`SELECT registration_token_sha256,registration_body_sha256 FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn).Scan(&tokenDigest, &bodyDigest))
	got := retentionSweep(t, db, 1)
	require.True(t, got.Incomplete)
	require.Zero(t, got.DeletedConnectionClosures)
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn))
	require.NoError(t, receipt.Commit())
	got = retentionSweep(t, db, 1)
	require.EqualValues(t, 1, got.DeletedConnectionClosures)
	require.Len(t, tokenDigest, 64)
	require.Len(t, bodyDigest, 64)
	require.NotEqual(t, tokenDigest, bodyDigest)
	require.Zero(t, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn))
}

func TestRetentionRoomGenerationContentionRetainsParentClosure(t *testing.T) {
	db := database(t)
	retentionTransportCleanup(t, db)
	user := testdb.CreateUser(t, db)
	_, conn, session := retentionBoundClosures(t, db, user, true)
	blocker := transaction(t, db)
	_, err := blocker.Exec(`SELECT login_family_generation_lock('room',$1)`, session)
	require.NoError(t, err)
	got := retentionSweep(t, db, 1)
	require.True(t, got.Incomplete)
	require.Zero(t, got.DeletedRoomClosures)
	require.Zero(t, got.DeletedConnectionClosures)
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_room_closures WHERE session_generation=$1`, session))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn))
	require.NoError(t, blocker.Commit())
	got = retentionSweep(t, db, 1)
	require.EqualValues(t, 1, got.DeletedRoomClosures)
	require.EqualValues(t, 1, got.DeletedConnectionClosures)
}

func TestRetentionOverlappingSweepReturnsIncompleteWithoutWaiting(t *testing.T) {
	db := database(t)
	db.SetMaxOpenConns(1)
	held, err := db.Conn(ctx)
	require.NoError(t, err)
	released := false
	t.Cleanup(func() {
		if !released {
			require.NoError(t, held.Close())
		}
	})
	firstCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	before := db.Stats().WaitCount
	go func() { _, sweepErr := lf.SweepRetiredEvidence(firstCtx, db, 1); finished <- sweepErr }()
	require.Eventually(t, func() bool { return db.Stats().WaitCount > before }, time.Second, time.Millisecond, "first sweep must own maintenance continuation before second call")
	secondCtx, secondCancel := context.WithTimeout(ctx, time.Second)
	defer secondCancel()
	got, err := lf.SweepRetiredEvidence(secondCtx, db, 1)
	require.NoError(t, err)
	require.True(t, got.Incomplete)
	require.Zero(t, got.DeletedFamilies)
	cancel()
	select {
	case err = <-finished:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled sweep did not return its retained continuation")
	}
	require.NoError(t, held.Close())
	released = true
	require.Equal(t, lf.GCResult{}, retentionSweep(t, db, 1))
}
