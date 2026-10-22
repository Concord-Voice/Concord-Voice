package billing

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// Phase-8 fixes to Reconcile and customer.deleted (PR #4019 triage C1, C3-C6,
// E5). Each test fails if its fix is reverted.

// barrierLockingUser opens a transaction on db that holds userID's users row
// FOR UPDATE, as an erasure in progress would, and returns it with its txid.
func barrierLockingUser(t *testing.T, db *sql.DB, userID uuid.UUID) (*sql.Tx, int64) {
	t.Helper()
	barrier, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = barrier.Rollback() })
	var txID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&txID))
	var one int
	require.NoError(t, barrier.QueryRow(`SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&one))
	return barrier, txID
}

// waitForLockedBackends blocks until at least n backends of the test database
// are waiting on a heavyweight lock.
func waitForLockedBackends(t *testing.T, probe *sql.DB, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return scalarInt(t, probe, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`) >= n
	}, 10*time.Second, 10*time.Millisecond, "%d backends waiting on a lock", n)
}

// terminateRowLockWaiter ends the backend parked on the barrier transaction
// txID, as a server restart or an administrator would.
func terminateRowLockWaiter(t *testing.T, probe *sql.DB, txID int64) {
	t.Helper()
	dbtest.WaitForRowLockWaiter(t, probe, txID)
	var terminated bool
	require.NoError(t, probe.QueryRow(`SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text::bigint = $1`,
		dbtest.TransactionIDForLockProbe(txID)).Scan(&terminated))
	require.True(t, terminated)
}

// C1: the upsert keeps a row with its first user, so when the payer is now
// someone else the write moves the owner's tier. The owner is pushed too, or
// a canceled owner keeps premium until the tier cache expires.
func TestReconcile_PushesRowOwnerWhenNotPayer(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	owner := dbtest.CreateUser(t, env.DB)
	payer := newPayer(t, env, "cus_own")
	seedStripeSub(t, env.DB, owner, "sub_own", "cus_own", "active", time.Unix(testPeriodEnd, 0).UTC())
	seedFakeSub(t, fake, "sub_own", "cus_own", nil)
	editFakeSub(t, fake, "sub_own", func(s *fakeSubscription) { s.Status = "canceled" })
	require.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, env.DB, owner.String()))

	changed, err := env.Svc.Reconcile(ctx, "sub_own", "", "")
	require.NoError(t, err)
	require.True(t, changed)

	assert.Equal(t, owner, loadSubRow(t, env.DB, "sub_own").UserID, "the row stays with its owner")
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, env.DB, owner.String()))
	assert.ElementsMatch(t, []tierCall{
		{UserID: owner, Old: entitlements.TierPremium, New: entitlements.TierFree},
		{UserID: payer, Old: entitlements.TierFree, New: entitlements.TierFree},
	}, env.Changer.Calls(), "the owner, whose tier the write moved, is pushed as well as the payer")

	// Nothing differs on a second pass, so neither user is pushed again.
	changed, err = env.Svc.Reconcile(ctx, "sub_own", "", "")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Len(t, env.Changer.Calls(), 2)
}

// C5: Checkout stamps concord_env on every subscription it creates, so in live
// mode an unmapped subscription without one is a paying customer Concord
// cannot place: an error for the operator. Test mode keeps it a warning.
func TestReconcile_MissingEnvironmentMetadata(t *testing.T) {
	const msg = "billing: unmapped subscription has no environment metadata; nothing written"
	cases := []struct {
		name      string
		mode      config.StripeKeyMode
		wantLevel string
		notLevel  string
	}{
		{"live mode logs an error", config.StripeKeyModeLive, "level=ERROR", "level=WARN"},
		{"test mode logs a warning", config.StripeKeyModeTest, "level=WARN", "level=ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStripe(t)
			env := newTestService(t, fake)
			env.Svc.Billing.KeyMode = tc.mode
			user := dbtest.CreateUser(t, env.DB) // a real user, so only the missing env can refuse
			seedFakeSub(t, fake, "sub_noenv", "cus_noenv", map[string]string{subMetaUserID: user.String()})

			_, err := env.Svc.Reconcile(reconcileCtx(t), "sub_noenv", "", "")
			require.ErrorIs(t, err, ErrUnactionable)
			assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
			assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
			logs := env.Logs.String()
			assert.Contains(t, logs, msg)
			assert.Contains(t, logs, tc.wantLevel)
			assert.NotContains(t, logs, tc.notLevel)
			assert.NotContains(t, logs, "belongs to another environment", "a missing env is not a foreign one")
		})
	}
}

// C6: two subscriptions of one unmapped Customer, reconciled at once, both read
// "no mapping" and then queue on the payer's row. The second one's insert
// conflicts with the mapping the first just wrote; that is the same mapping,
// not a conflict, and must not be logged as one.
func TestReconcile_SiblingSubscriptionsHealOneMappingQuietly(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := dbtest.CreateUser(t, env.DB)
	meta := map[string]string{subMetaUserID: user.String(), subMetaEnv: testEnvironment}
	seedFakeSub(t, fake, "sub_sib_a", "cus_sib", meta)
	seedFakeSub(t, fake, "sub_sib_b", "cus_sib", meta)

	probe := openProbeDB(t)
	barrier, _ := barrierLockingUser(t, openProbeDB(t), user)

	errs := make(chan error, 2)
	for _, sub := range []string{"sub_sib_a", "sub_sib_b"} {
		go func() {
			_, err := env.Svc.Reconcile(ctx, sub, "", "")
			errs <- err
		}()
	}
	// Both have looked the Customer up and found no mapping: they wait on
	// the users row, which lookupPayer reads past and lockUser locks.
	waitForLockedBackends(t, probe, 2)
	require.NoError(t, barrier.Rollback())

	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	assert.Equal(t, "cus_sib", mappedCustomer(t, env.DB, user))
	assert.Equal(t, 2, countUserSubs(t, env.DB, user))
	logs := env.Logs.String()
	assert.NotContains(t, logs, "already mapped to a different customer")
	assert.NotContains(t, logs, "already mapped to a different user")
}

// C6, the other conflict: while the reconcile waited for the payer's row, the
// Customer was mapped to another user. The payer has no mapping at all, so the
// log must say whose the Customer is, not claim the payer has another one.
func TestReconcile_SelfHealCustomerHeldByAnotherUser(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := dbtest.CreateUser(t, env.DB)
	other := dbtest.CreateUser(t, env.DB)
	seedFakeSub(t, fake, "sub_held", "cus_held", map[string]string{
		subMetaUserID: user.String(), subMetaEnv: testEnvironment,
	})

	probe := openProbeDB(t)
	barrier, txID := barrierLockingUser(t, env.DB, user)
	_, err := barrier.ExecContext(ctx,
		`INSERT INTO stripe_customers (user_id, stripe_customer_id) VALUES ($1, 'cus_held')`, other)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := env.Svc.Reconcile(ctx, "sub_held", "", "")
		done <- err
	}()
	dbtest.WaitForRowLockWaiter(t, probe, txID)
	require.NoError(t, barrier.Commit())

	require.NoError(t, <-done, "F24: the conflict is logged and the reconcile continues")
	assert.Equal(t, "", mappedCustomer(t, env.DB, user), "the payer gains no mapping")
	assert.Equal(t, "cus_held", mappedCustomer(t, env.DB, other), "the other user's mapping is kept")
	logs := env.Logs.String()
	assert.Contains(t, logs, "billing: customer is already mapped to a different user; mapping kept")
	assert.NotContains(t, logs, "already mapped to a different customer")
}

// E5: a subscription with no customer cannot be placed; nothing is written.
func TestReconcile_NoCustomerWritesNothing(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_nocus")
	seedFakeSub(t, fake, "sub_nocus", "cus_nocus", nil)
	editFakeSub(t, fake, "sub_nocus", func(s *fakeSubscription) { s.Customer = "" })

	changed, err := env.Svc.Reconcile(reconcileCtx(t), "sub_nocus", "evt_nocus", "customer.subscription.updated")
	require.ErrorIs(t, err, ErrUnactionable)
	assert.False(t, changed)
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
	assert.Equal(t, 0, dedupeCount(t, env.DB, "evt_nocus"), "the webhook, not Reconcile, records an unactionable event")
	assert.Empty(t, env.Changer.Calls())
	logs := env.Logs.String()
	assert.Contains(t, logs, "billing: subscription has no customer; nothing written")
	assert.Contains(t, logs, "sub_nocus")
}

// C2 and C3: a reconcile whose session dies mid-transaction returns an error
// tagged with the statement that failed, and the failed deferred rollback is
// logged instead of discarded.
func TestReconcile_SessionLostMidTransaction(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_lost")
	seedFakeSub(t, fake, "sub_lost", "cus_lost", nil)

	probe := openProbeDB(t)
	_, txID := barrierLockingUser(t, openProbeDB(t), user)

	done := make(chan error, 1)
	go func() {
		_, err := env.Svc.Reconcile(ctx, "sub_lost", "evt_lost", "customer.subscription.updated")
		done <- err
	}()
	terminateRowLockWaiter(t, probe, txID)

	err := <-done
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnactionable, "a lost session is retried")
	assert.Equal(t, stageLockPayer, errorStage(err))
	assert.Equal(t, 0, countSubsBySubID(t, env.DB, "sub_lost"))

	logs := env.Logs.String()
	assert.Contains(t, logs, "billing: reconcile rollback failed")
	assert.Contains(t, logs, "stage="+stageRollback)
	assert.Contains(t, logs, "billing: advisory unlock failed; discarding the connection")
	assert.Equal(t, 0, grantedAdvisoryLocks(t, probe, reconcileAdvisoryKey(t, probe, "sub_lost")))
}

// C3 for customer.deleted: the mapping delete's session dies while it waits on
// the mapping row; the rollback failure is logged and the error is staged.
func TestDropCustomerMapping_SessionLostMidTransaction(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_droplost")

	probe := openProbeDB(t)
	barrier, err := openProbeDB(t).BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = barrier.Rollback() })
	var txID int64
	require.NoError(t, barrier.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&txID))
	_, err = barrier.ExecContext(ctx,
		`SELECT 1 FROM stripe_customers WHERE stripe_customer_id = 'cus_droplost' FOR UPDATE`)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- env.Svc.dropCustomerMapping(ctx, "cus_droplost", "evt_droplost", customerDeletedType) }()
	terminateRowLockWaiter(t, probe, txID)

	err = <-done
	require.Error(t, err)
	assert.Equal(t, stageDeleteMapping, errorStage(err))
	require.NoError(t, barrier.Rollback())
	assert.Equal(t, 1, countTable(t, env.DB, "stripe_customers"), "nothing was dropped")
	assert.Equal(t, 0, dedupeCount(t, env.DB, "evt_droplost"))
	logs := env.Logs.String()
	assert.Contains(t, logs, "billing: customer deleted rollback failed")
	assert.Contains(t, logs, "stage="+stageRollback)
}

// C4: an unlock whose statement fails is told apart from one that ran and
// found the key not held.
func TestUnlockSubscription_StatementFailureDiscardsConnection(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)

	conn, err := env.DB.Conn(ctx)
	require.NoError(t, err)
	var pid int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	probe := openProbeDB(t)
	var terminated bool
	require.NoError(t, probe.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)

	env.Svc.unlockSubscription(ctx, conn, "sub_dead_session")

	logs := env.Logs.String()
	assert.Contains(t, logs, "billing: advisory unlock failed; discarding the connection")
	assert.Contains(t, logs, "stage="+stageAdvisoryUnlock)
	assert.NotContains(t, logs, "was not held", "a failed statement says nothing about the key")
	assert.Contains(t, logs, "sub_dead_session")
}

// C2 caller-level: the webhook's retry log names the failing stage.
func TestWebhook_RetryLogNamesTheStage(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_stage")
	seedFakeSub(t, fake, "sub_stage", "cus_stage", nil)
	fake.failOn("GET", subscriptionGetPath+"sub_stage", 500, serverErrorBody)

	w := deliverSigned(env, subscriptionEvent(t, "evt_stage", eventSubscriptionUpdated, "sub_stage", "cus_stage"))

	require.Equal(t, 500, w.Code)
	assert.Equal(t, webhookErrInternal, errorClassOf(t, w))
	logs := env.Logs.String()
	assert.Contains(t, logs, "billing: webhook reconcile failed; Stripe will retry")
	assert.Contains(t, logs, "error_class=api_error")
	assert.Contains(t, logs, "stage="+stageFetchSub)
}

// R-a, pinned deterministically: the self-heal's insert into stripe_customers
// runs only after the payer's users row is locked. Were it first, the insert's
// foreign key check would hold FOR KEY SHARE on the row and the later FOR
// UPDATE would be an upgrade that deadlocks against an erasure queued between
// them. The deadlock probe races this but its window is microseconds wide, so
// this test reads the parked reconcile's locks instead: while it waits for the
// users row it must not yet hold the insert's RowExclusiveLock.
func TestReconcile_SelfHealWaitsForPayerLockBeforeInserting(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := dbtest.CreateUser(t, env.DB)
	seedFakeSub(t, fake, "sub_order", "cus_order", map[string]string{
		subMetaUserID: user.String(), subMetaEnv: testEnvironment,
	})

	probe := openProbeDB(t)
	barrier, err := openProbeDB(t).Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = barrier.Rollback() })
	var txID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&txID))
	// FOR SHARE blocks lockUser's FOR UPDATE but not a foreign key's KEY SHARE,
	// so a reconcile that inserted first would get through the insert.
	_, err = barrier.Exec(`SELECT 1 FROM users WHERE id = $1 FOR SHARE`, user)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := env.Svc.Reconcile(ctx, "sub_order", "", "")
		done <- err
	}()
	dbtest.WaitForRowLockWaiter(t, probe, txID)
	var pid int
	require.NoError(t, probe.QueryRow(`SELECT pid FROM pg_locks
		WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text::bigint = $1`,
		dbtest.TransactionIDForLockProbe(txID)).Scan(&pid))
	assert.Equal(t, 0, scalarInt(t, probe, `SELECT COUNT(*) FROM pg_locks
		WHERE pid = $1 AND locktype = 'relation' AND granted AND mode = 'RowExclusiveLock'
		  AND relation = 'stripe_customers'::regclass`, pid),
		"the mapping insert must wait behind the payer's users lock")

	require.NoError(t, barrier.Rollback())
	require.NoError(t, <-done)
	assert.Equal(t, "cus_order", mappedCustomer(t, env.DB, user))
}
