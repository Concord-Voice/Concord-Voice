package servers

import (
	"context"
	"database/sql"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// SetListServersMFAMaskReaderForTest replaces the ListServers MFA-mask read for
// the duration of t, so a test can make the P1 read fail and observe the
// fail-closed branch (#3453). Tests in this package run serially, and the
// original is restored on cleanup.
func SetListServersMFAMaskReaderForTest(
	t testing.TB, f func(ctx context.Context, db *sql.DB, userID string) (rbac.MFAMask, error),
) {
	t.Helper()
	prev := readListServersMFAMask
	readListServersMFAMask = f
	t.Cleanup(func() { readListServersMFAMask = prev })
}

// SetListServersRowScannerForTest replaces ListServers' row scan for the
// duration of t, so a test can make one row fail to scan. Tests in this package
// run serially, and the original is restored on cleanup.
func SetListServersRowScannerForTest(t testing.TB, f func(rows *sql.Rows, dest ...any) error) {
	t.Helper()
	prev := scanListServersRow
	scanListServersRow = f
	t.Cleanup(func() { scanListServersRow = prev })
}

// MFAEnforcementStepUpPrefix exposes the toggle's budget key prefix so a test
// can read the attempt counter the PUT charges (#3453).
const MFAEnforcementStepUpPrefix = mfaEnforcementStepUpPrefix

// SetMFAEnforcementBudgetForTest replaces the toggle's attempt-budget
// constructor for the duration of t, so a test can make the budget
// unevaluable (a nil client DENIES with a 503).
func SetMFAEnforcementBudgetForTest(t testing.TB, f func(rdb *redis.Client) stepup.Budget) {
	t.Helper()
	prev := newMFAEnforcementBudget
	newMFAEnforcementBudget = f
	t.Cleanup(func() { newMFAEnforcementBudget = prev })
}

// WrapMFAEnforcementBeginForTest runs before(ctx) immediately before the PUT's
// transaction begins, so a test can observe what already happened by then.
func WrapMFAEnforcementBeginForTest(t testing.TB, before func(ctx context.Context)) {
	t.Helper()
	prev := beginMFAEnforcementTx
	beginMFAEnforcementTx = func(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
		before(ctx)
		return prev(ctx, db)
	}
	t.Cleanup(func() { beginMFAEnforcementTx = prev })
}

// SetMFAEnforcementGateForTest replaces mfaenforce.LockGateTx in the PUT, so a
// test can inject a lock conflict.
func SetMFAEnforcementGateForTest(t testing.TB, f func(
	ctx context.Context, tx *sql.Tx, serverID, actorID string,
	userLock stepup.Lock, serverLock mfaenforce.ServerLock, tokenEpoch string,
) (mfaenforce.Gate, error)) {
	t.Helper()
	prev := lockMFAEnforcementGate
	lockMFAEnforcementGate = f
	t.Cleanup(func() { lockMFAEnforcementGate = prev })
}

// SetMFAEnforcementCommitForTest replaces the PUT's commit, so a test can make
// the commit fail after every in-transaction write has run.
func SetMFAEnforcementCommitForTest(t testing.TB, f func(tx *sql.Tx) error) {
	t.Helper()
	prev := commitMFAEnforcementTx
	commitMFAEnforcementTx = f
	t.Cleanup(func() { commitMFAEnforcementTx = prev })
}
