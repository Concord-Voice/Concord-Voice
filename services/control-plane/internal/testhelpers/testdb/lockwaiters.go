package testdb

import (
	"database/sql"
	"testing"
)

// lockWaiterPollBudget bounds a waiter poll.
//
// It is an ITERATION budget, deliberately not a duration: `[internal]rules/tests.md`
// forbids timing-based sequencing, and every iteration is a real database
// round-trip rather than a spin, so the loop is self-throttling. The budget only
// exists so a genuinely-never-arriving waiter fails with a named cause instead of
// Go's opaque "panic: test timed out after 10m0s".
const lockWaiterPollBudget = 60000

// AdvisoryKeyHalves splits a signed 64-bit advisory key into the two 32-bit
// halves PostgreSQL stores in pg_locks.classid / pg_locks.objid.
//
// The halves MUST be compared separately. A key with its high bit set does not
// survive a `(classid << 32) | objid` reassembly inside PostgreSQL's signed
// bigint, so the naive round-trip silently never matches.
func AdvisoryKeyHalves(key int64) (classID, objID int64) {
	//nolint:gosec // bit-preserving split of a signed advisory key into the two
	// 32-bit halves pg_locks exposes; both results are < 2^32 and non-negative.
	unsigned := uint64(key)
	return int64(unsigned >> 32), int64(unsigned & 0xFFFFFFFF) //nolint:gosec // see above
}

// TransactionIDForLockProbe reduces a txid_current() value to the 32-bit xid
// pg_locks.transactionid actually stores. txid_current() returns an
// epoch-extended 64-bit value, so comparing it directly against pg_locks stops
// matching the first time the transaction counter wraps.
func TransactionIDForLockProbe(txID int64) int64 {
	return txID & 0xFFFFFFFF
}

// advisoryWaiterQuery and rowLockWaiterQuery are shared by the blocking waiter
// helpers and their non-blocking counterparts, so a test that proves a waiter
// APPEARS and a test that proves one NEVER APPEARS are asking pg_locks the same
// question.
const (
	advisoryWaiterQuery = `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory'
			  AND NOT granted
			  AND objsubid = 1
			  AND classid::bigint = $1
			  AND objid::bigint = $2
		)`

	rowLockWaiterQuery = `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'transactionid'
			  AND NOT granted
			  AND transactionid::text::bigint = $1
		)`
)

// AdvisoryLockWaiterExists reports, without blocking, whether any backend is
// currently parked on the advisory lock identified by key.
//
// It is the counterpart to WaitForAdvisoryLockWaiter, for regression locks that
// must prove a waiter NEVER appears — a denial that acquires the per-server
// role-mutation lock is itself the defect (#2721 red-team R1).
func AdvisoryLockWaiterExists(t *testing.T, probe *sql.DB, key int64) bool {
	t.Helper()
	classID, objID := AdvisoryKeyHalves(key)
	return lockPredicateHolds(t, probe, advisoryWaiterQuery, classID, objID)
}

// WaitForRowLockWaiter blocks until at least one backend is waiting on a row
// lock held by the transaction whose xid is txID, then returns.
//
// This is the row-barrier half of the waiter detection used by the RBAC
// role-mutation guard tests (#2721). Pass txID straight from `SELECT
// txid_current()` executed on the barrier connection; the reduction to 32 bits
// happens here.
//
// probe MUST be a pool distinct from the one holding the barrier: a backend
// cannot observe its own pending lock while it is blocked on it.
func WaitForRowLockWaiter(t *testing.T, probe *sql.DB, txID int64) {
	t.Helper()
	waitForLockPredicate(t, probe, "a backend waiting on the barrier transaction's row lock",
		rowLockWaiterQuery, TransactionIDForLockProbe(txID))
}

// WaitForAdvisoryLockWaiter blocks until at least one backend is waiting on the
// advisory lock identified by key, then returns.
//
// key is the signed 64-bit value passed to pg_advisory_lock /
// pg_advisory_xact_lock — e.g. rbac.ServerVisibilityCaptureAdvisoryKey(serverID).
func WaitForAdvisoryLockWaiter(t *testing.T, probe *sql.DB, key int64) {
	t.Helper()
	classID, objID := AdvisoryKeyHalves(key)
	waitForLockPredicate(t, probe, "a backend waiting on the advisory lock",
		advisoryWaiterQuery, classID, objID)
}

// waitForLockPredicate polls query until it reports true. It is the single
// implementation behind every waiter helper in this package, so no test ever
// inlines a pg_locks poll of its own.
func waitForLockPredicate(t *testing.T, probe *sql.DB, what, query string, args ...any) {
	t.Helper()
	for attempt := 0; attempt < lockWaiterPollBudget; attempt++ {
		if lockPredicateHolds(t, probe, query, args...) {
			return
		}
	}
	t.Fatalf("testdb: gave up after %d polls waiting for %s", lockWaiterPollBudget, what)
}

// lockPredicateHolds runs one pg_locks EXISTS probe.
func lockPredicateHolds(t *testing.T, probe *sql.DB, query string, args ...any) bool {
	t.Helper()
	var waiting bool
	if err := probe.QueryRow(query, args...).Scan(&waiting); err != nil {
		t.Fatalf("testdb: failed to poll pg_locks: %v", err)
	}
	return waiting
}
