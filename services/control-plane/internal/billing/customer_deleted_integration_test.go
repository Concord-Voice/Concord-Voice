package billing

import (
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// customer.deleted integration tests (#2033 PR-2, Task 4). Deleting a Stripe
// Customer cancels its subscriptions at once, so the handler reconciles every
// row the customer owns (each fetch returns canceled) and only then drops the
// mapping and writes the dedupe row, in one final transaction (F24).

const customerDeletedType = "customer.deleted"

// clearFailure removes a failure installed with failOn, so a retry can succeed.
func clearFailure(fake *fakeStripe, method, path string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	delete(fake.failures, method+" "+path)
}

// seedDeletedCustomerSubs gives cusID a mapped payer and two stored active rows
// whose Stripe subscriptions now read canceled, as they do after the Customer
// is deleted. It returns the payer.
func seedDeletedCustomerSubs(t *testing.T, env *testEnv, cusID, subA, subB string) (payer string) {
	t.Helper()
	user := newPayer(t, env, cusID)
	end := time.Unix(testPeriodEnd, 0).UTC()
	for _, subID := range []string{subA, subB} {
		seedStripeSub(t, env.DB, user, subID, cusID, "active", end)
		seedFakeSub(t, env.Fake, subID, cusID, nil)
		editFakeSub(t, env.Fake, subID, func(s *fakeSubscription) { s.Status = "canceled" })
	}
	return user.String()
}

func TestCustomerDeleted_ReconcilesRowsThenDropsMapping(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	payer := seedDeletedCustomerSubs(t, env, "cus_cd", "sub_cd_a", "sub_cd_b")

	err := env.Svc.reconcileCustomerDeleted(ctx, "cus_cd", "evt_cd_1", customerDeletedType)
	require.NoError(t, err)

	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_cd_a").Status)
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_cd_b").Status)
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"), "the mapping is dropped")
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_cd_1"))
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, env.DB, payer),
		"with every row canceled the payer is no longer entitled")

	calls := env.Changer.Calls()
	require.NotEmpty(t, calls, "the tier change is pushed after the rows commit")
	assert.Equal(t, entitlements.TierFree, calls[len(calls)-1].New)
	assert.Equal(t, payer, calls[len(calls)-1].UserID.String())
}

func TestCustomerDeleted_PartialFailureRetriesInFull(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	seedDeletedCustomerSubs(t, env, "cus_cdp", "sub_cdp_a", "sub_cdp_b")
	failPath := subscriptionGetPath + "sub_cdp_b"
	fake.failOn("GET", failPath, 500, serverErrorBody)

	err := env.Svc.reconcileCustomerDeleted(ctx, "cus_cdp", "evt_cdp_1", customerDeletedType)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnactionable), "a Stripe 5xx is retried by the webhook, never acknowledged")
	assert.Equal(t, 1, countTable(t, env.DB, "stripe_customers"), "the mapping stays until every row is reconciled")
	assert.Equal(t, 0, dedupeCount(t, env.DB, "evt_cdp_1"), "no dedupe row: Stripe's retry must reprocess the whole event")

	clearFailure(fake, "GET", failPath)
	err = env.Svc.reconcileCustomerDeleted(ctx, "cus_cdp", "evt_cdp_1", customerDeletedType)
	require.NoError(t, err)
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_cdp_a").Status)
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_cdp_b").Status)
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_cdp_1"))
}

// A row whose Stripe subscription is gone (ErrUnactionable) must not block the
// deletion: only a retryable error stops the pass.
func TestCustomerDeleted_UnactionableRowDoesNotBlock(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_cdu")
	end := time.Unix(testPeriodEnd, 0).UTC()
	seedStripeSub(t, env.DB, user, "sub_cdu_gone", "cus_cdu", "active", end) // never seeded in the fake: resource_missing
	seedStripeSub(t, env.DB, user, "sub_cdu_ok", "cus_cdu", "active", end)
	seedFakeSub(t, fake, "sub_cdu_ok", "cus_cdu", nil)
	editFakeSub(t, fake, "sub_cdu_ok", func(s *fakeSubscription) { s.Status = "canceled" })

	err := env.Svc.reconcileCustomerDeleted(ctx, "cus_cdu", "evt_cdu_1", customerDeletedType)
	require.NoError(t, err)
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_cdu_ok").Status)
	assert.Equal(t, "active", loadSubRow(t, env.DB, "sub_cdu_gone").Status, "an unfetchable row is left as stored")
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_cdu_1"))
}

// A customer that owns no subscription rows still has its mapping dropped and
// the event recorded.
func TestCustomerDeleted_NoRowsStillDropsMapping(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_cdn")

	err := env.Svc.reconcileCustomerDeleted(ctx, "cus_cdn", "evt_cdn_1", customerDeletedType)
	require.NoError(t, err)
	assert.Equal(t, "", mappedCustomer(t, env.DB, user))
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_cdn_1"))
	assert.Equal(t, 0, countUserSubs(t, env.DB, user))
}

// A database fault is retryable: nothing is acknowledged, so Stripe redelivers.
func TestCustomerDeleted_DatabaseFailureIsRetryable(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://closed.invalid/none?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	svc := NewService(Deps{DB: db, Log: logger.NewWithWriter(io.Discard)})
	ctx := reconcileCtx(t)

	err = svc.reconcileCustomerDeleted(ctx, "cus_dbdown", "evt_dbdown", customerDeletedType)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnactionable))

	err = svc.dropCustomerMapping(ctx, "cus_dbdown", "evt_dbdown", customerDeletedType)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnactionable))
}
