package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Reconcile integration tests (#2033 PR-2, Task 3). They run against the real
// test database, the Stripe fake and a recording TierChanger. Assertions are
// on rows, fake requests, changer calls and the captured log's level and
// message.

const subscriptionGetPath = "/v1/subscriptions/"

func reconcileCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestReconcile_InsertsActiveRow(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_ins")
	seedFakeSub(t, fake, "sub_ins", "cus_ins", nil)

	changed, err := env.Svc.Reconcile(ctx, "sub_ins", "evt_ins_1", "customer.subscription.created")
	require.NoError(t, err)
	assert.True(t, changed)

	row := loadSubRow(t, env.DB, "sub_ins")
	assert.Equal(t, user, row.UserID)
	assert.Equal(t, "stripe", row.Source)
	assert.Equal(t, "active", row.Status)
	assert.Equal(t, entitlements.TierPremium, row.Tier)
	assert.Equal(t, PlanWing, row.Plan.String)
	assert.Equal(t, "year", row.Interval.String)
	assert.Equal(t, "cus_ins", row.CustomerID.String)
	require.True(t, row.PeriodEnd.Valid)
	assert.Equal(t, testPeriodEnd, row.PeriodEnd.Time.Unix())
	assert.False(t, row.FoundersRate)
	assert.False(t, row.CancelAtPeriodEnd)
	assert.False(t, row.CancelAt.Valid)

	assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierFree, New: entitlements.TierPremium}},
		env.Changer.Calls())
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_ins_1"), "the dedupe row commits with the entitlement row")

	// The consumer of the fetch: state is re-fetched with the price and the
	// discounts expanded, and never with the customer expanded.
	reqs := env.Fake.requestsTo("GET", subscriptionGetPath+"sub_ins")
	require.Len(t, reqs, 1)
	assert.True(t, requestsExpand(reqs[0].Form, "items.data.price"))
	assert.True(t, requestsExpand(reqs[0].Form, "discounts"))
	assert.False(t, requestsExpand(reqs[0].Form, "customer"))
}

func TestReconcile_NoEventIDWritesNoDedupeRow(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_nd")
	seedFakeSub(t, fake, "sub_nd", "cus_nd", nil)

	_, err := env.Svc.Reconcile(ctx, "sub_nd", "", "")
	require.NoError(t, err)
	assert.Equal(t, 1, countSubsBySubID(t, env.DB, "sub_nd"))
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_webhook_events"), "drift and discovery pass no event")
}

func TestInsertDedupe_IdempotentOnConflict(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)

	require.NoError(t, insertDedupe(ctx, env.DB, "evt_dd_1", "customer.subscription.updated"))
	require.NoError(t, insertDedupe(ctx, env.DB, "evt_dd_1", "customer.subscription.deleted"),
		"a second insert of the same event id is ON CONFLICT DO NOTHING, not an error")
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_dd_1"))

	var typ string
	require.NoError(t, env.DB.QueryRow(
		`SELECT event_type FROM stripe_webhook_events WHERE event_id = 'evt_dd_1'`).Scan(&typ))
	assert.Equal(t, "customer.subscription.updated", typ, "the first writer's type stands")
}

func TestReconcile_SameStateTwiceIsIdempotent(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_idem")
	seedFakeSub(t, fake, "sub_idem", "cus_idem", nil)

	changed, err := env.Svc.Reconcile(ctx, "sub_idem", "evt_idem_1", "customer.subscription.created")
	require.NoError(t, err)
	require.True(t, changed)
	first := loadSubRow(t, env.DB, "sub_idem")

	changed, err = env.Svc.Reconcile(ctx, "sub_idem", "evt_idem_2", "customer.subscription.updated")
	require.NoError(t, err)
	assert.False(t, changed, "identical Stripe state changes nothing")

	second := loadSubRow(t, env.DB, "sub_idem")
	assert.Equal(t, first.ID, second.ID, "the upsert updates the same row")
	assert.True(t, second.UpdatedAt.After(first.UpdatedAt), "updated_at advances on every reconcile (F27)")
	assert.Equal(t, first.CreatedAt, second.CreatedAt)
	assert.Len(t, env.Changer.Calls(), 1, "an unchanged tier with no change pushes nothing")
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_idem_2"))
}

func TestReconcile_OutOfOrderConverges(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_ooo")
	seedFakeSub(t, fake, "sub_ooo", "cus_ooo", nil)

	// Reconcile trusts the fetch, not the event: whatever order the events
	// arrive in, the row converges on what Stripe says now.
	_, err := env.Svc.Reconcile(ctx, "sub_ooo", "evt_ooo_created", "customer.subscription.created")
	require.NoError(t, err)
	editFakeSub(t, fake, "sub_ooo", func(s *fakeSubscription) { s.Status = "canceled" })

	_, err = env.Svc.Reconcile(ctx, "sub_ooo", "evt_ooo_updated", "customer.subscription.updated")
	require.NoError(t, err)
	require.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_ooo").Status)

	// The older event arrives last.
	changed, err := env.Svc.Reconcile(ctx, "sub_ooo", "evt_ooo_created_late", "customer.subscription.created")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_ooo").Status, "a late older event cannot resurrect the row")
	assert.Equal(t, []tierCall{
		{UserID: user, Old: entitlements.TierFree, New: entitlements.TierPremium},
		{UserID: user, Old: entitlements.TierPremium, New: entitlements.TierFree},
	}, env.Changer.Calls())
}

func TestReconcile_ConcurrentSameSubSerializes(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_conc")
	seedFakeSub(t, fake, "sub_conc", "cus_conc", nil)

	probe := openProbeDB(t)
	key := reconcileAdvisoryKey(t, probe, "sub_conc")

	// Hold the advisory key on a dedicated session, as a running Reconcile of
	// the same subscription would.
	holder, err := openProbeDB(t).Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Close() }() // closing the session also releases the lock
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key)
	require.NoError(t, err)

	type result struct {
		changed bool
		err     error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			changed, err := env.Svc.Reconcile(ctx, "sub_conc", "", "")
			results <- result{changed, err}
		}()
	}

	dbtest.WaitForAdvisoryLockWaiter(t, probe, key)
	assert.Equal(t, 0, countSubsBySubID(t, env.DB, "sub_conc"), "nothing is written while the key is held")
	assert.Empty(t, env.Fake.requestsTo("GET", subscriptionGetPath+"sub_conc"),
		"the lock is taken before the Stripe fetch, so a blocked Reconcile has not fetched")

	var unlocked bool
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked))
	require.True(t, unlocked)

	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.Equal(t, 1, countSubsBySubID(t, env.DB, "sub_conc"), "serialized reconciles converge on one row")
	assert.True(t, first.changed != second.changed, "exactly one of the two inserted; the other saw their row")
}

func TestReconcile_TwoLiveStripeRowsBothEntitle(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_two")
	seedFakeSub(t, fake, "sub_two_a", "cus_two", nil)
	fake.seedSubscription("sub_two_b", "cus_two", PlanSupersonic, "month", testPeriodEnd)

	_, err := env.Svc.Reconcile(ctx, "sub_two_a", "", "")
	require.NoError(t, err)
	_, err = env.Svc.Reconcile(ctx, "sub_two_b", "", "")
	require.NoError(t, err, "a second live Stripe subscription is legal, never a 23505")

	assert.Equal(t, 2, countUserSubs(t, env.DB, user))
	assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, env.DB, user.String()))
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR", "two live Stripe rows for one payer are logged as an error")
	assert.Contains(t, logs, "billing: payer holds more than one live Stripe subscription")
}

func TestReconcile_ResourceMissingWritesNothing(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_gone")
	seedStripeSub(t, env.DB, user, "sub_gone", "cus_gone", "active", time.Unix(testPeriodEnd, 0).UTC())
	before := loadSubRow(t, env.DB, "sub_gone")

	// The fake holds no sub_gone: Stripe answers 404 resource_missing (R36).
	changed, err := env.Svc.Reconcile(ctx, "sub_gone", "evt_gone", "customer.subscription.updated")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnactionable)
	assert.False(t, changed)

	assert.Equal(t, before, loadSubRow(t, env.DB, "sub_gone"), "the stored row is byte-identical, updated_at included")
	assert.Empty(t, env.Changer.Calls())
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "billing: subscription is missing in Stripe; nothing written")
	assert.Contains(t, logs, "sub_gone", "the error log names the subscription id")
}

func TestReconcile_UnmappedCustomerUserAbsent(t *testing.T) {
	const (
		msgNoUserHere = "billing: subscription metadata names no user here; nothing written"
		msgNoUserID   = "billing: unmapped subscription has no usable user id; nothing written"
	)
	cases := []struct {
		name     string
		metadata func() map[string]string
		wantMsg  string
	}{
		{"user id of a deleted account", func() map[string]string {
			return map[string]string{subMetaUserID: uuid.NewString(), subMetaEnv: testEnvironment}
		}, msgNoUserHere},
		{"user id is not a uuid", func() map[string]string {
			return map[string]string{subMetaUserID: "not-a-uuid", subMetaEnv: testEnvironment}
		}, msgNoUserID},
		{"no user id metadata", func() map[string]string {
			return map[string]string{subMetaEnv: testEnvironment}
		}, msgNoUserID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStripe(t)
			env := newTestService(t, fake)
			ctx := reconcileCtx(t)
			seedFakeSub(t, fake, "sub_ua", "cus_ua", tc.metadata())

			changed, err := env.Svc.Reconcile(ctx, "sub_ua", "evt_ua", "customer.subscription.created")
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnactionable)
			assert.False(t, changed)

			assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
			assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
			assert.Equal(t, 0, countTable(t, env.DB, "billing_erasure_outbox"), "R31: no outbox row for an unmapped customer")
			assert.Empty(t, env.Fake.requestsTo("DELETE", "/v1/customers/cus_ua"), "R31: no Stripe Customer delete")
			assert.Empty(t, env.Fake.writes(), "R31: Reconcile makes no Stripe write")
			assert.Empty(t, env.Changer.Calls())
			logs := env.Logs.String()
			assert.Contains(t, logs, "level=WARN", "test mode logs the unresolvable payer as a warning")
			assert.Contains(t, logs, tc.wantMsg)
			assert.NotContains(t, logs, "level=ERROR")
		})
	}
}

func TestReconcile_SelfHealsMapping(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := dbtest.CreateUser(t, env.DB)
	seedFakeSub(t, fake, "sub_heal", "cus_heal", map[string]string{
		subMetaUserID: user.String(), subMetaEnv: testEnvironment,
	})

	changed, err := env.Svc.Reconcile(ctx, "sub_heal", "evt_heal", "customer.subscription.created")
	require.NoError(t, err)
	assert.True(t, changed)

	assert.Equal(t, "cus_heal", mappedCustomer(t, env.DB, user), "a lost checkout.session.completed is healed from metadata")
	assert.Equal(t, user, loadSubRow(t, env.DB, "sub_heal").UserID)
	assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierFree, New: entitlements.TierPremium}},
		env.Changer.Calls())
}

func TestReconcile_SelfHealKeepsExistingDifferentMapping(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_first")
	seedFakeSub(t, fake, "sub_second", "cus_second", map[string]string{
		subMetaUserID: user.String(), subMetaEnv: testEnvironment,
	})

	_, err := env.Svc.Reconcile(ctx, "sub_second", "", "")
	require.NoError(t, err, "F24: a user already mapped to another Customer does not fail the reconcile")

	assert.Equal(t, "cus_first", mappedCustomer(t, env.DB, user), "the existing mapping is not replaced")
	assert.Equal(t, user, loadSubRow(t, env.DB, "sub_second").UserID, "the row is still written")
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "billing: payer is already mapped to a different customer; mapping kept")
}

// A dead subscription must not resurrect its Customer's mapping. After
// customer.deleted removes the mapping, a late subscription.updated or
// .deleted event for one of that Customer's now-canceled subscriptions still
// names a real user in its metadata; healing from it would point the user at a
// Customer Stripe no longer has (the Portal and Checkout would then 404 on it).
// Only a subscription that can still entitle may heal the mapping. The row
// itself is still written, so the end of the subscription reaches the user.
func TestReconcile_TerminalSubscriptionDoesNotSelfHealMapping(t *testing.T) {
	for _, status := range []string{"canceled", "incomplete_expired"} {
		t.Run(status, func(t *testing.T) {
			fake := newFakeStripe(t)
			env := newTestService(t, fake)
			ctx := reconcileCtx(t)
			user := dbtest.CreateUser(t, env.DB) // exists, and env matches: only the status can refuse the heal
			// The row the subscription left while it was live; its mapping is
			// already gone, as after customer.deleted.
			seedStripeSub(t, env.DB, user, "sub_term", "cus_term", "active", time.Unix(testPeriodEnd, 0).UTC())
			require.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, env.DB, user.String()))
			seedFakeSub(t, fake, "sub_term", "cus_term", map[string]string{
				subMetaUserID: user.String(), subMetaEnv: testEnvironment,
			})
			editFakeSub(t, fake, "sub_term", func(s *fakeSubscription) { s.Status = status })

			changed, err := env.Svc.Reconcile(ctx, "sub_term", "evt_term", "customer.subscription.deleted")
			require.NoError(t, err, "a terminal subscription of a known payer is written, not refused")
			assert.True(t, changed)

			assert.Equal(t, status, loadSubRow(t, env.DB, "sub_term").Status, "the end of the subscription is stored")
			assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, env.DB, user.String()))
			assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierPremium, New: entitlements.TierFree}},
				env.Changer.Calls())
			assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_term"))
			assert.Equal(t, "", mappedCustomer(t, env.DB, user), "no mapping is created from a terminal subscription")
			assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
			assert.Equal(t, 0, countTable(t, env.DB, "billing_erasure_outbox"), "R31: Reconcile never enqueues a Customer delete")
			assert.Empty(t, env.Fake.writes(), "Reconcile makes no Stripe write")
		})
	}
}

// RF1: dev boxes share one Stripe test account.
func TestReconcile_EnvMismatchWritesNothing(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := dbtest.CreateUser(t, env.DB) // a real user, so only the env can refuse
	seedFakeSub(t, fake, "sub_env", "cus_env", map[string]string{
		subMetaUserID: user.String(), subMetaEnv: "some-other-box",
	})

	changed, err := env.Svc.Reconcile(ctx, "sub_env", "evt_env", "customer.subscription.created")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnactionable)
	assert.False(t, changed)

	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"), "no row")
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"), "no mapping")
	assert.Equal(t, 0, countTable(t, env.DB, "billing_erasure_outbox"))
	assert.Empty(t, env.Fake.writes(), "no Stripe write: another box's Customer is not ours to touch")
	assert.Empty(t, env.Changer.Calls())
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=WARN")
	assert.Contains(t, logs, "billing: subscription belongs to another environment; nothing written")
	assert.NotContains(t, logs, "level=ERROR", "a foreign environment is expected on a shared account, not an error")
}

// The payer's users row is locked FOR UPDATE; an erasure that commits while
// Reconcile waits for it leaves nothing to write against.
func TestReconcile_PayerErasedWritesNothing(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_erased")
	seedFakeSub(t, fake, "sub_erased", "cus_erased", nil)

	probe := openProbeDB(t)
	barrier, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	var txID int64
	require.NoError(t, barrier.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&txID))
	var locked int
	require.NoError(t, barrier.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, user).Scan(&locked))

	done := make(chan error, 1)
	go func() {
		_, err := env.Svc.Reconcile(ctx, "sub_erased", "evt_erased", "customer.subscription.created")
		done <- err
	}()
	dbtest.WaitForRowLockWaiter(t, probe, txID) // Reconcile is parked on the users row

	_, err = barrier.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, user) // erasure; the mapping cascades
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())

	err = <-done
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnactionable)
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
	assert.Equal(t, 0, countTable(t, env.DB, "billing_erasure_outbox"), "the erasure path owns the outbox row, not Reconcile")
	assert.Empty(t, env.Changer.Calls())
}

func TestReconcile_AbsentPeriodKeepsStoredRow(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_np")
	seedStripeSub(t, env.DB, user, "sub_np", "cus_np", "active", time.Unix(testPeriodEnd, 0).UTC())
	seedFakeSub(t, fake, "sub_np", "cus_np", nil)
	editFakeSub(t, fake, "sub_np", func(s *fakeSubscription) { s.Items.Data[0].CurrentPeriodEnd = 0 })
	before := loadSubRow(t, env.DB, "sub_np")

	changed, err := env.Svc.Reconcile(ctx, "sub_np", "evt_np", "customer.subscription.updated")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnactionable)
	assert.False(t, changed)
	assert.Equal(t, before, loadSubRow(t, env.DB, "sub_np"), "a subscription with no period end overwrites nothing")
	assert.Empty(t, env.Changer.Calls())
}

func TestReconcile_TierChangeArguments(t *testing.T) {
	t.Run("purchase", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		user := newPayer(t, env, "cus_arg1")
		seedFakeSub(t, fake, "sub_arg1", "cus_arg1", nil)

		_, err := env.Svc.Reconcile(reconcileCtx(t), "sub_arg1", "", "")
		require.NoError(t, err)
		assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierFree, New: entitlements.TierPremium}},
			env.Changer.Calls())
	})

	t.Run("cancel", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		user := newPayer(t, env, "cus_arg2")
		seedStripeSub(t, env.DB, user, "sub_arg2", "cus_arg2", "active", time.Unix(testPeriodEnd, 0).UTC())
		seedFakeSub(t, fake, "sub_arg2", "cus_arg2", nil)
		editFakeSub(t, fake, "sub_arg2", func(s *fakeSubscription) { s.Status = "canceled" })

		changed, err := env.Svc.Reconcile(reconcileCtx(t), "sub_arg2", "", "")
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierPremium, New: entitlements.TierFree}},
			env.Changer.Calls())
	})

	t.Run("code holder buys", func(t *testing.T) {
		fake := newFakeStripe(t)
		env := newTestService(t, fake)
		user := newPayer(t, env, "cus_arg3")
		seedLiveCodeSub(t, env.DB, user)
		seedFakeSub(t, fake, "sub_arg3", "cus_arg3", nil)

		changed, err := env.Svc.Reconcile(reconcileCtx(t), "sub_arg3", "", "")
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierPremium, New: entitlements.TierPremium}},
			env.Changer.Calls(), "F26: pushed although the tier did not change, so the client sees the new source")
	})
}

// RF3: a user with a live code row buys, then the Stripe subscription ends.
func TestReconcile_CancelWithLiveCodeRowPushesPremiumToPremium(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_rf3")
	seedLiveCodeSub(t, env.DB, user)
	seedStripeSub(t, env.DB, user, "sub_rf3", "cus_rf3", "active", time.Unix(testPeriodEnd, 0).UTC())
	seedFakeSub(t, fake, "sub_rf3", "cus_rf3", nil)
	editFakeSub(t, fake, "sub_rf3", func(s *fakeSubscription) { s.Status = "canceled" })

	changed, err := env.Svc.Reconcile(ctx, "sub_rf3", "evt_rf3", "customer.subscription.deleted")
	require.NoError(t, err)
	assert.True(t, changed)

	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_rf3").Status)
	assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierPremium, New: entitlements.TierPremium}},
		env.Changer.Calls(), "exactly one push, premium to premium: the code row still covers the user, so no downgrade")
	assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, env.DB, user.String()),
		"the enforced tier agrees with the push")
}

func TestReconcile_StripeErrorIsRetryable(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_5xx")
	seedFakeSub(t, fake, "sub_5xx", "cus_5xx", nil)
	fake.failOn("GET", subscriptionGetPath+"sub_5xx", 500, serverErrorBody)

	changed, err := env.Svc.Reconcile(ctx, "sub_5xx", "evt_5xx", "customer.subscription.updated")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnactionable), "a Stripe 5xx is retried by the webhook (500), never acknowledged")
	assert.False(t, changed)
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
	assert.Equal(t, 0, dedupeCount(t, env.DB, "evt_5xx"), "no dedupe row: the retry must reprocess the event")
	assert.Empty(t, env.Changer.Calls())
}

// classify fails closed for a shape Concord never sold: the row is still
// stored (the fetch is the truth) but it grants nothing.
func TestReconcile_MalformedShapeStoresFreeTierAndLogsError(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_mal")
	seedFakeSub(t, fake, "sub_mal", "cus_mal", nil)
	editFakeSub(t, fake, "sub_mal", func(s *fakeSubscription) { s.Items.Data[0].Quantity = 2 })

	changed, err := env.Svc.Reconcile(ctx, "sub_mal", "", "")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, entitlements.TierFree, loadSubRow(t, env.DB, "sub_mal").Tier)
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, env.DB, user.String()))
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR", "the fail-closed reason is logged for the operator")
	assert.Contains(t, logs, "billing: subscription is not a sold plan shape; stored at the free tier")
}

// An upsert never moves a row to another user, whatever the mapping says now.
func TestReconcile_NeverReassignsRowUser(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	owner := dbtest.CreateUser(t, env.DB)
	other := newPayer(t, env, "cus_re")
	seedStripeSub(t, env.DB, owner, "sub_re", "cus_re", "active", time.Unix(testPeriodEnd, 0).UTC())
	seedFakeSub(t, fake, "sub_re", "cus_re", nil)
	editFakeSub(t, fake, "sub_re", func(s *fakeSubscription) { s.Status = "past_due" })

	_, err := env.Svc.Reconcile(ctx, "sub_re", "", "")
	require.NoError(t, err)
	row := loadSubRow(t, env.DB, "sub_re")
	assert.Equal(t, owner, row.UserID, "user_id is never rewritten by the upsert")
	assert.NotEqual(t, other, row.UserID)
	assert.Equal(t, "past_due", row.Status, "the other columns still update")
}

// changed is true when any stored column differs and false when none does.
func TestReconcile_ChangedDetectsEachColumn(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*fakeSubscription)
		wantChanged bool
	}{
		{"nothing differs", func(*fakeSubscription) {}, false},
		{"status", func(s *fakeSubscription) { s.Status = "past_due" }, true},
		{"cancel_at_period_end", func(s *fakeSubscription) { s.CancelAtPeriodEnd = true }, true},
		{"cancel_at", func(s *fakeSubscription) { s.CancelAt = testPeriodEnd - 86400 }, true},
		{"current_period_end", func(s *fakeSubscription) { s.Items.Data[0].CurrentPeriodEnd += 86400 }, true},
		{"founders_rate", func(s *fakeSubscription) { s.couponIDs = []string{CouponFoundersID} }, true},
		{"plan", func(s *fakeSubscription) { s.Items.Data[0].Price.Metadata[PlanMetadataKey] = PlanSupersonic }, true},
		{"billing_interval", func(s *fakeSubscription) { s.Items.Data[0].Price.Recurring.Interval = "month" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStripe(t)
			env := newTestService(t, fake)
			ctx := reconcileCtx(t)
			newPayer(t, env, "cus_chg")
			seedFakeSub(t, fake, "sub_chg", "cus_chg", nil)
			_, err := env.Svc.Reconcile(ctx, "sub_chg", "", "")
			require.NoError(t, err)

			editFakeSub(t, fake, "sub_chg", tc.mutate)
			changed, err := env.Svc.Reconcile(ctx, "sub_chg", "", "")
			require.NoError(t, err)
			assert.Equal(t, tc.wantChanged, changed)
		})
	}
}

// A TierChanger failure is logged, not returned: the row is committed, and the
// webhook must not retry a delivery whose only fault is a push.
func TestReconcile_NotifyErrorIsNotReturned(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	newPayer(t, env, "cus_notify")
	seedFakeSub(t, fake, "sub_notify", "cus_notify", nil)
	env.Changer.err = errors.New("push failed")

	changed, err := env.Svc.Reconcile(ctx, "sub_notify", "", "")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, countSubsBySubID(t, env.DB, "sub_notify"), "the committed row stands")
	assert.Len(t, env.Changer.Calls(), 1)
}
