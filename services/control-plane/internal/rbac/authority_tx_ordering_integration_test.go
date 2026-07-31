package rbac

import (
	"context"
	"database/sql"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	_ "github.com/lib/pq" // registers the PostgreSQL driver used by this fixture
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openOrderingProbeDB opens a connection pool for the ordering lock. It runs no
// migrations and touches no fixture table: withAuthorityCapture's transaction
// half only needs BeginTx, pg_advisory_xact_lock, a trivial write, and Commit,
// and the presence seam is stubbed. Keeping it schema-free is what lets this
// lock live in Task 3 instead of waiting on the Task 6 harness.
func openOrderingProbeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	if err != nil {
		t.Skipf("ordering lock needs PostgreSQL: %v", err)
	}
	if pingErr := db.Ping(); pingErr != nil {
		_ = db.Close()
		t.Skipf("ordering lock needs PostgreSQL: %v", pingErr)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// advisoryLockIsHeld reports whether the derived per-server visibility-capture
// advisory lock is currently granted anywhere in the cluster. It runs on a
// SEPARATE pool so it can observe a lock a different session holds.
//
// The two 32-bit halves are compared separately rather than reassembled: a key
// with its high bit set does not survive a (classid << 32) | objid expression
// inside PostgreSQL's signed bigint.
func advisoryLockIsHeld(t *testing.T, probe *sql.DB, serverID string) bool {
	t.Helper()
	key, err := ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	//nolint:gosec // bit-preserving split of the signed advisory key into the
	// two 32-bit halves PostgreSQL stores in pg_locks.classid/objid.
	unsignedKey := uint64(key)
	classID := int64(unsignedKey >> 32)
	objID := int64(unsignedKey & 0xFFFFFFFF)

	var held bool
	require.NoError(t, probe.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory'
			  AND granted
			  AND objsubid = 1
			  AND classid::bigint = $1
			  AND objid::bigint = $2
		)
	`, classID, objID).Scan(&held))
	return held
}

// ORDERING REGRESSION LOCK (transaction half). PrepareCapture precedes BeginTx;
// CaptureVisibility runs inside the transaction, after the advisory lock and
// before the write; Execute runs only after Commit.
func TestWithAuthorityCapture_PhaseOrdering_IsPrepareThenLockThenVisibilityThenWrite(t *testing.T) {
	probe := openOrderingProbeDB(t)
	handlerDB := openOrderingProbeDB(t)
	serverID := uuid.New().String()

	stub := &presenceRecheckStub{plan: &presenceRecheckPlanStub{work: true}}
	h := &Handler{db: handlerDB}
	h.SetPresenceRecheck(stub)

	var (
		lockHeldDuringVisibility bool
		lockHeldDuringPrepare    bool
	)
	stub.prepareProbe = func() {
		lockHeldDuringPrepare = advisoryLockIsHeld(t, probe, serverID)
	}
	stub.visibilityProbe = func(tx *sql.Tx) {
		lockHeldDuringVisibility = advisoryLockIsHeld(t, probe, serverID)
		require.NotNil(t, tx, "CaptureVisibility receives the caller's transaction")
	}

	plan, err := h.withAuthorityCapture(
		context.Background(), serverID, []string{uuid.New().String()}, nil,
		func(ctx context.Context, tx *sql.Tx) error {
			stub.sequence = append(stub.sequence, "Write")
			_, execErr := tx.ExecContext(ctx, `SELECT 1`)
			return execErr
		},
	)

	require.NoError(t, err)
	require.NotNil(t, plan)
	h.presenceExecute(plan)

	assert.Equal(t,
		[]string{"PrepareCapture", "CaptureVisibility", "Write", "Execute"},
		stub.sequence,
	)
	assert.False(t, lockHeldDuringPrepare,
		"phase 1 runs BEFORE the transaction opens, so the lock is not yet held")
	assert.True(t, lockHeldDuringVisibility,
		"phase 2 runs under the advisory lock; phase 1 does not")
	assert.False(t, advisoryLockIsHeld(t, probe, serverID),
		"the lock is released at Commit")
}

// The capture failure path is a rollback: the write never runs, the transaction
// never commits, and the advisory lock is released.
func TestWithAuthorityCapture_CaptureVisibilityError_RollsBackAndReleasesTheLock(t *testing.T) {
	probe := openOrderingProbeDB(t)
	handlerDB := openOrderingProbeDB(t)
	serverID := uuid.New().String()

	stub := &presenceRecheckStub{
		plan:          &presenceRecheckPlanStub{work: true},
		visibilityErr: assert.AnError,
	}
	h := &Handler{db: handlerDB}
	h.SetPresenceRecheck(stub)
	wrote := false

	plan, err := h.withAuthorityCapture(
		context.Background(), serverID, []string{uuid.New().String()}, nil,
		func(context.Context, *sql.Tx) error { wrote = true; return nil },
	)

	require.Error(t, err)
	assert.Nil(t, plan)
	assert.False(t, wrote, "the permission write never happens (spec section 8, class 2)")
	assert.Equal(t, []string{"PrepareCapture", "CaptureVisibility"}, stub.sequence,
		"no Execute and no Abandon: nothing committed, so nothing to reconcile")
	assert.False(t, advisoryLockIsHeld(t, probe, serverID),
		"rollback releases the advisory lock")
}
