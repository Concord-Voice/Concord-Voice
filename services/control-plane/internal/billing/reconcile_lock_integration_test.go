package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// Lock-lifecycle and error-path tests for Reconcile (#2033 PR-2, Task 3).
// They pin the property the session advisory lock depends on: a connection
// that might still hold a key never returns to the pool.

// grantedAdvisoryLocks counts sessions holding the advisory key. The halves
// are compared separately (see dbtest.AdvisoryKeyHalves).
func grantedAdvisoryLocks(t *testing.T, probe *sql.DB, key int64) int {
	t.Helper()
	classID, objID := dbtest.AdvisoryKeyHalves(key)
	return scalarInt(t, probe,
		`SELECT COUNT(*) FROM pg_locks
		 WHERE locktype = 'advisory' AND granted AND objsubid = 1
		   AND classid::bigint = $1 AND objid::bigint = $2`, classID, objID)
}

// A reconcile whose wait for the key is canceled returns a retryable error,
// writes nothing, and leaves no session holding the key: once the holder
// releases it, the next reconcile proceeds.
func TestReconcile_CanceledLockWaitLeavesNoHolder(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_cw")
	seedFakeSub(t, fake, "sub_cw", "cus_cw", nil)

	probe := openProbeDB(t)
	key := reconcileAdvisoryKey(t, probe, "sub_cw")
	holder, err := openProbeDB(t).Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Close() }()
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	changed, err := env.Svc.Reconcile(waitCtx, "sub_cw", "evt_cw", "customer.subscription.updated")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnactionable), "a lock wait that runs out is retried, never acknowledged")
	assert.False(t, changed)
	assert.Empty(t, fake.requestsTo("GET", subscriptionGetPath+"sub_cw"), "no fetch without the lock")
	assert.Equal(t, 0, countSubsBySubID(t, env.DB, "sub_cw"))
	assert.Equal(t, 0, dedupeCount(t, env.DB, "evt_cw"))

	var unlocked bool
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked))
	require.True(t, unlocked)
	assert.Equal(t, 0, grantedAdvisoryLocks(t, probe, key), "the canceled waiter holds nothing")

	_, err = env.Svc.Reconcile(ctx, "sub_cw", "", "")
	require.NoError(t, err)
	assert.Equal(t, 1, countSubsBySubID(t, env.DB, "sub_cw"))
	assert.Equal(t, 0, grantedAdvisoryLocks(t, probe, key), "a finished reconcile releases its key")
}

// An unlock that reports the key was not held discards the connection instead
// of pooling it, and logs at error with its own message (C4).
func TestUnlockSubscription_FailureDiscardsConnection(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)

	conn, err := env.DB.Conn(ctx)
	require.NoError(t, err)
	var pid int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	env.Svc.unlockSubscription(ctx, conn, "sub_never_locked")

	// A pooled connection keeps its backend alive (idle); a discarded one's
	// backend exits once the client closes the socket.
	probe := openProbeDB(t)
	require.Eventually(t, func() bool {
		return scalarInt(t, probe, `SELECT COUNT(*) FROM pg_stat_activity WHERE pid = $1`, pid) == 0
	}, 10*time.Second, 20*time.Millisecond, "the session was discarded, not returned to the pool")
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "billing: advisory lock was not held at unlock; discarding the connection")
	assert.NotContains(t, logs, "advisory unlock failed", "the statement ran; only the key was missing")
	assert.Contains(t, logs, "sub_never_locked")
}

// A failure while the transaction is open rolls it back and still releases
// the key: here the dedupe row violates its CHECK after the row was upserted.
func TestReconcile_DedupeFailureRollsBackAndReleases(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_dfail")
	seedFakeSub(t, fake, "sub_dfail", "cus_dfail", nil)

	changed, err := env.Svc.Reconcile(ctx, "sub_dfail", "not_an_event_id", "customer.subscription.updated")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnactionable))
	assert.False(t, changed)
	assert.Equal(t, 0, countSubsBySubID(t, env.DB, "sub_dfail"), "the upsert rolled back with the dedupe row")
	assert.Empty(t, env.Changer.Calls())

	probe := openProbeDB(t)
	assert.Equal(t, 0, grantedAdvisoryLocks(t, probe, reconcileAdvisoryKey(t, probe, "sub_dfail")))
}

func TestInsertDedupe_RejectsMalformedEventID(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	require.Error(t, insertDedupe(reconcileCtx(t), env.DB, "not_an_event_id", "customer.subscription.updated"))
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_webhook_events"))
}

// In live mode an unmapped customer whose user is absent is an error for
// operator review; test mode logs the same case at warn (shared account).
func TestReconcile_UnmappedUserAbsentLiveModeLogsError(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	env.Svc.Billing.KeyMode = config.StripeKeyModeLive
	ctx := reconcileCtx(t)
	seedFakeSub(t, fake, "sub_live_ua", "cus_live_ua", map[string]string{
		subMetaUserID: uuid.NewString(), subMetaEnv: testEnvironment,
	})

	_, err := env.Svc.Reconcile(ctx, "sub_live_ua", "", "")
	require.ErrorIs(t, err, ErrUnactionable)
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
	assert.Empty(t, env.Fake.writes())
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "billing: subscription metadata names no user here; nothing written")
	assert.Contains(t, logs, "sub_live_ua")
	assert.Contains(t, logs, "cus_live_ua")
}
