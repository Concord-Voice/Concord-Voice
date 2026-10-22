package billing

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
)

// Task 9 (spec section 11): the drift worker reconciles subscriptions whose
// webhooks may have been lost, a discovery pass finds subscriptions whose
// creation webhook never arrived, and pruning bounds stripe_webhook_events.

const subListPath = "/v1/subscriptions"

func driftCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// driftSub is a seeded Stripe-sourced row plus the matching fake subscription.
type driftSub struct {
	UserID     uuid.UUID
	SubID      string
	CustomerID string
}

// seedDriftSub seeds a user with a mapping, a source='stripe' row and a fake
// subscription that agrees with the row (same Wing yearly plan and period end,
// whole seconds so the stored and fetched values compare equal), so a Reconcile
// of it changes nothing unless the test then edits the fake. updatedAgo backdates
// the row's updated_at.
func seedDriftSub(t *testing.T, env *testEnv, name, status string, periodEnd time.Time, updatedAgo time.Duration) driftSub {
	t.Helper()
	d := driftSub{SubID: "sub_" + name, CustomerID: "cus_" + name}
	d.UserID = newPayer(t, env, d.CustomerID)
	periodEnd = periodEnd.Truncate(time.Second)
	seedStripeSub(t, env.DB, d.UserID, d.SubID, d.CustomerID, status, periodEnd)
	env.Fake.seedSubscription(d.SubID, d.CustomerID, PlanWing, "year", periodEnd.Unix())
	if status != "active" {
		editFakeSub(t, env.Fake, d.SubID, func(s *fakeSubscription) { s.Status = status })
	}
	_, err := env.DB.Exec(
		`UPDATE subscriptions SET updated_at = NOW() - make_interval(secs => $2) WHERE stripe_subscription_id = $1`,
		d.SubID, updatedAgo.Seconds())
	require.NoError(t, err)
	return d
}

func subFetches(fake *fakeStripe, subID string) int {
	return len(fake.requestsTo("GET", subscriptionGetPath+subID))
}

// seedDriftRowOnly seeds the local row and mapping of seedDriftSub but no fake
// subscription, so Stripe answers resource_missing and Reconcile returns
// ErrUnactionable.
func seedDriftRowOnly(t *testing.T, env *testEnv, name, status string, periodEnd time.Time, updatedAgo time.Duration) driftSub {
	t.Helper()
	d := driftSub{SubID: "sub_" + name, CustomerID: "cus_" + name}
	d.UserID = newPayer(t, env, d.CustomerID)
	seedStripeSub(t, env.DB, d.UserID, d.SubID, d.CustomerID, status, periodEnd.Truncate(time.Second))
	backdateSub(t, env, d.SubID, updatedAgo)
	return d
}

func backdateSub(t *testing.T, env *testEnv, subID string, ago time.Duration) {
	t.Helper()
	_, err := env.DB.Exec(
		`UPDATE subscriptions SET updated_at = NOW() - make_interval(secs => $2) WHERE stripe_subscription_id = $1`,
		subID, ago.Seconds())
	require.NoError(t, err)
}

// updatedAgo is how long ago the row's updated_at is, by the database clock.
func updatedAgo(t *testing.T, env *testEnv, subID string) time.Duration {
	t.Helper()
	var secs float64
	require.NoError(t, env.DB.QueryRow(
		`SELECT EXTRACT(EPOCH FROM (NOW() - updated_at)) FROM subscriptions WHERE stripe_subscription_id = $1`,
		subID).Scan(&secs))
	return time.Duration(secs * float64(time.Second))
}

func TestDrift_Constants(t *testing.T) {
	assert.Equal(t, 15*time.Minute, driftInterval)
	assert.Equal(t, 50, driftBatch)
	assert.Equal(t, 20, discoveryBatch)
	assert.Equal(t, 30*24*time.Hour, eventRetention)
}

// F27: the batch of 50 is filled urgent-first, so a flood of rows that merely
// renew soon cannot starve a lapsed or past_due row.
func TestDrift_UrgentBucketsFirst(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := driftCtx(t)
	now := time.Now()

	const renewing = 60
	for i := range renewing {
		seedDriftSub(t, env, fmt.Sprintf("renew%02d", i), "active", now.Add(12*time.Hour), 16*time.Minute)
	}
	lapsed := seedDriftSub(t, env, "lapsed", "active", now.Add(-2*time.Hour), 16*time.Minute)
	pastDue := seedDriftSub(t, env, "pastdue", "past_due", now.Add(30*24*time.Hour), time.Minute)

	report, err := env.Svc.RunDrift(ctx)
	require.NoError(t, err)

	assert.Equal(t, driftBatch, report.Reconciled, "one pass reconciles at most driftBatch rows")
	assert.Equal(t, 1, subFetches(fake, lapsed.SubID), "the lapsed row is in the batch")
	assert.Equal(t, 1, subFetches(fake, pastDue.SubID), "the past_due row is in the batch")
	renewFetched := 0
	for i := range renewing {
		renewFetched += subFetches(fake, fmt.Sprintf("sub_renew%02d", i))
	}
	assert.Equal(t, driftBatch-2, renewFetched, "the rest of the batch is renewing-soon rows")
}

func TestDrift_BucketOrderIsLapsedThenPastDueThenRenewingThenStale(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	now := time.Now()
	// More lapsed rows than the batch holds: they must crowd out every lower bucket.
	const lapsedRows = driftBatch + 2
	for i := range lapsedRows {
		seedDriftSub(t, env, fmt.Sprintf("lap%02d", i), "active", now.Add(-time.Hour), time.Hour)
	}
	lowers := []driftSub{
		seedDriftSub(t, env, "lowpastdue", "past_due", now.Add(30*24*time.Hour), time.Minute),
		seedDriftSub(t, env, "lowrenew", "active", now.Add(6*time.Hour), time.Hour),
		seedDriftSub(t, env, "lowstale", "active", now.Add(30*24*time.Hour), 48*time.Hour),
	}

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, driftBatch, report.Reconciled)
	for _, low := range lowers {
		assert.Zero(t, subFetches(fake, low.SubID), "%s is below the lapsed bucket and the batch is full", low.SubID)
	}
}

// A5: bucket 1 outranks bucket 2 on its own, not through the updated_at
// tiebreak. The past_due rows are older than the lapsed row, so with the two
// buckets merged the lapsed row would sort last and miss the batch.
func TestDrift_LapsedOutranksOlderPastDue(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	now := time.Now()
	for i := range driftBatch {
		seedDriftSub(t, env, fmt.Sprintf("pd%02d", i), "past_due", now.Add(30*24*time.Hour), 2*time.Hour)
	}
	lapsed := seedDriftSub(t, env, "lapsednew", "active", now.Add(-time.Hour), time.Minute)

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, driftBatch, report.Fetched)
	assert.Equal(t, 1, subFetches(fake, lapsed.SubID), "the lapsed row is fetched ahead of older past_due rows")
}

// A5: bucket 3 outranks bucket 4 on its own. The stale rows are a day older
// than the renewing row and fill the batch, so with the two buckets merged the
// renewing row would sort last and miss it.
func TestDrift_RenewingOutranksOlderStaleRows(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	now := time.Now()
	for i := range driftBatch {
		seedDriftSub(t, env, fmt.Sprintf("st%02d", i), "active", now.Add(30*24*time.Hour), 48*time.Hour)
	}
	renewing := seedDriftSub(t, env, "renewsoon", "active", now.Add(6*time.Hour), 20*time.Minute)

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, driftBatch, report.Fetched)
	assert.Equal(t, 1, subFetches(fake, renewing.SubID), "the row renewing soon is fetched ahead of older stale rows")
}

// A1: unpaid, incomplete and paused rows are never live, so they reach the
// worker only through bucket 4 (not touched for 24 h). A1/A2: a live row
// lapsed past the 6 h buffer is not live either and falls to bucket 4 too.
func TestDrift_NonLiveStatusesAndLapsedRowsReachOnlyTheDailyBucket(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	now := time.Now()

	picked := []driftSub{
		seedDriftSub(t, env, "unpaidstale", "unpaid", now.Add(10*24*time.Hour), 25*time.Hour),
		seedDriftSub(t, env, "incompletestale", "incomplete", now.Add(10*24*time.Hour), 25*time.Hour),
		seedDriftSub(t, env, "pausedstale", "paused", now.Add(10*24*time.Hour), 25*time.Hour),
		seedDriftSub(t, env, "lapsedoldstale", "active", now.Add(-7*time.Hour), 25*time.Hour),
	}
	skipped := map[string]driftSub{
		"an unpaid row lapsed inside the buffer but touched recently": seedDriftSub(t, env, "unpaidfresh", "unpaid", now.Add(-time.Hour), time.Minute),
		"an unpaid row renewing soon but touched recently":            seedDriftSub(t, env, "unpaidsoon", "unpaid", now.Add(6*time.Hour), 20*time.Minute),
		"a canceled row, however stale":                               seedDriftSub(t, env, "canceledstale", "canceled", now.Add(10*24*time.Hour), 72*time.Hour),
		"an active row lapsed past the buffer, touched recently":      seedDriftSub(t, env, "lapsedold", "active", now.Add(-7*time.Hour), 20*time.Minute),
		"a past_due row lapsed past the buffer, touched recently":     seedDriftSub(t, env, "pastdueold", "past_due", now.Add(-7*time.Hour), time.Minute),
	}

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	for _, d := range picked {
		assert.Equal(t, 1, subFetches(fake, d.SubID), "%s is untouched for 24 h and is picked", d.SubID)
	}
	for why, d := range skipped {
		assert.Zero(t, subFetches(fake, d.SubID), "%s is not picked", why)
	}
	assert.Equal(t, len(picked), report.Fetched)
	assert.Equal(t, "unpaid", loadSubRow(t, env.DB, "sub_unpaidstale").Status, "the reconcile kept the agreeing status")
}

// A2: an unactionable row moves its updated_at, so it leaves buckets 3 and 4
// for a day instead of heading the batch on every tick. A retryable failure
// does not, so it is retried on the next tick.
func TestDrift_UnactionableRowIsDeferredForADay(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	now := time.Now()
	stale := seedDriftRowOnly(t, env, "goneStale", "active", now.Add(10*24*time.Hour), 25*time.Hour)
	renewing := seedDriftRowOnly(t, env, "goneRenewing", "active", now.Add(6*time.Hour), 20*time.Minute)
	retryable := seedDriftSub(t, env, "flaky", "active", now.Add(10*24*time.Hour), 25*time.Hour)
	fake.failOn("GET", subscriptionGetPath+retryable.SubID, http.StatusServiceUnavailable, serverErrorBody)

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 3, report.Fetched)
	assert.Equal(t, 3, report.Failed)
	for _, d := range []driftSub{stale, renewing} {
		assert.Less(t, updatedAgo(t, env, d.SubID), time.Minute, "%s is marked as just looked at", d.SubID)
	}
	assert.Greater(t, updatedAgo(t, env, retryable.SubID), 24*time.Hour, "a retryable failure leaves updated_at alone")
	assert.Equal(t, "active", loadSubRow(t, env.DB, stale.SubID).Status, "nothing else about the row changes")

	report, err = env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 1, subFetches(fake, stale.SubID), "the stale unactionable row is not picked on the next tick")
	assert.Equal(t, 1, subFetches(fake, renewing.SubID), "nor is the renewing one")
	assert.Equal(t, 2, subFetches(fake, retryable.SubID), "the retryable row is")
	assert.Equal(t, 1, report.Fetched)
}

// A2: a lapsed row stays in bucket 1 only for the 6 h buffer. Past it, an
// unactionable lapsed row can no longer head the batch on every tick.
func TestDrift_LapsedBucketEndsAtTheBuffer(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	now := time.Now()
	inBuffer := seedDriftRowOnly(t, env, "goneInBuffer", "active", now.Add(-5*time.Hour), time.Minute)
	pastBuffer := seedDriftRowOnly(t, env, "gonePastBuffer", "active", now.Add(-7*time.Hour), time.Minute)

	_, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)
	_, err = env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, 2, subFetches(fake, inBuffer.SubID), "inside the buffer a lapsed row is picked every tick")
	assert.Zero(t, subFetches(fake, pastBuffer.SubID), "past the buffer it waits for the daily bucket")
}

func TestDrift_SelectsOnlyLiveStripeRowsAndSkipsFreshOnes(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	now := time.Now()

	fresh := seedDriftSub(t, env, "fresh", "active", now.Add(10*24*time.Hour), time.Minute)
	staleDay := seedDriftSub(t, env, "staleday", "active", now.Add(10*24*time.Hour), 25*time.Hour)
	soonButRecent := seedDriftSub(t, env, "soonrecent", "active", now.Add(6*time.Hour), time.Minute)
	ended := seedDriftSub(t, env, "ended", "canceled", now.Add(-48*time.Hour), 72*time.Hour)

	codeUser := newPayer(t, env, "cus_codeonly")
	seedLiveCodeSub(t, env.DB, codeUser)

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Zero(t, subFetches(fake, fresh.SubID), "updated 1 min ago and renewing in 10 days is not reconciled")
	assert.Zero(t, subFetches(fake, soonButRecent.SubID), "renewing soon but updated 1 min ago is not reconciled")
	assert.Equal(t, 1, subFetches(fake, staleDay.SubID), "a row untouched for 24 h+ is reconciled")
	assert.Zero(t, subFetches(fake, ended.SubID), "a canceled row is not live and not reconciled")
	assert.Equal(t, 1, report.Reconciled)
	assert.Zero(t, report.Corrections)
	assert.Empty(t, fake.requestsTo("GET", subscriptionGetPath+"sub_codeonly"), "source='code' rows are never drift candidates")
}

func TestDrift_CorrectionCounted(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	env.Svc.Counters = opsmetrics.NewCounters()
	ctx := driftCtx(t)
	now := time.Now()

	drifted := seedDriftSub(t, env, "drifted", "active", now.Add(-time.Hour), time.Hour)
	editFakeSub(t, fake, drifted.SubID, func(s *fakeSubscription) { s.Status = "canceled" })
	seedDriftSub(t, env, "insync", "active", now.Add(-time.Hour), time.Hour) // fake agrees with the row

	report, err := env.Svc.RunDrift(ctx)
	require.NoError(t, err)

	assert.Equal(t, 2, report.Reconciled)
	assert.Equal(t, 1, report.Corrections, "only the row whose stored state differed from Stripe counts")
	requireLogLine(t, env, "INFO", "billing: drift pass finished", "fetched=2", "corrected=1", "failed=0")
	assert.Equal(t, float64(1), env.Svc.Counters.Snapshot()[opsmetrics.MetricBillingDriftCorrectionsTotal])
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, drifted.SubID).Status, "the row was repaired")
	assert.Contains(t, env.Logs.String(), drifted.SubID, "a correction is logged at warn with the subscription id")

	// The repaired row is no longer live, so a second pass has nothing to fix.
	report, err = env.Svc.RunDrift(ctx)
	require.NoError(t, err)
	assert.Zero(t, report.Corrections)
	assert.Equal(t, float64(1), env.Svc.Counters.Snapshot()[opsmetrics.MetricBillingDriftCorrectionsTotal], "the counter does not move")
}

func TestDrift_RetryableErrorIsLoggedAndTheRunContinues(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake) // Counters stays nil: Increment must be nil-safe
	now := time.Now()
	broken := seedDriftSub(t, env, "broken", "active", now.Add(-time.Hour), 2*time.Hour)
	fake.failOn("GET", subscriptionGetPath+broken.SubID, http.StatusServiceUnavailable, serverErrorBody)
	healthy := seedDriftSub(t, env, "healthy", "active", now.Add(-time.Hour), time.Hour)
	editFakeSub(t, fake, healthy.SubID, func(s *fakeSubscription) { s.Status = "canceled" })

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err, "a per-row retryable error is logged, not returned")

	assert.Equal(t, 1, report.Corrections, "the row after the failing one is still reconciled and corrected")
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, healthy.SubID).Status)
	assert.Equal(t, "active", loadSubRow(t, env.DB, broken.SubID).Status, "the failing row is untouched")
	requireLogLine(t, env, "WARN", "billing: drift could not reconcile a subscription",
		"stripe_subscription_id="+broken.SubID, "error_class=api_error")
	// A3: the pass summary counts the failure, and a failure makes it a warning.
	assert.Equal(t, 2, report.Fetched)
	assert.Equal(t, 1, report.Failed)
	requireLogLine(t, env, "WARN", "billing: drift pass finished", "fetched=2", "corrected=1", "failed=1")
}

func TestDrift_CanceledContextReturnsError(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := env.Svc.RunDrift(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// F12: a subscription created at Stripe whose webhook never arrived has no
// local row. Discovery finds it from the customer mapping Checkout stamped.
func TestDrift_DiscoveryFindsLostCreation(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	user := newPayer(t, env, "cus_lost")
	_, err := env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() - INTERVAL '1 hour' WHERE user_id = $1`, user)
	require.NoError(t, err)
	seedFakeSub(t, fake, "sub_lost", "cus_lost", nil)

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, 1, report.Discovered)
	row := loadSubRow(t, env.DB, "sub_lost")
	assert.Equal(t, user, row.UserID)
	assert.Equal(t, "stripe", row.Source)
	assert.Equal(t, "active", row.Status)
	assert.Equal(t, entitlements.TierPremium, row.Tier)
	assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierFree, New: entitlements.TierPremium}},
		env.Changer.Calls(), "the user is upgraded through the same tier-change path as a webhook")

	lists := fake.requestsTo("GET", subListPath)
	require.Len(t, lists, 1)
	assert.Equal(t, "cus_lost", lists[0].Form.Get("customer"))
	assert.Equal(t, "all", lists[0].Form.Get("status"), "ended subscriptions are listed too, so every state converges")
}

func TestDrift_DiscoveryReconcilesEverySubscriptionOfTheCustomer(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	user := newPayer(t, env, "cus_two")
	_, err := env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() WHERE user_id = $1`, user)
	require.NoError(t, err)
	seedFakeSub(t, fake, "sub_two_live", "cus_two", nil)
	seedFakeSub(t, fake, "sub_two_ended", "cus_two", nil)
	editFakeSub(t, fake, "sub_two_ended", func(s *fakeSubscription) { s.Status = "canceled" })

	_, err = env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, "active", loadSubRow(t, env.DB, "sub_two_live").Status)
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_two_ended").Status)
}

func TestDrift_DiscoverySkipsIneligibleMappings(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := driftCtx(t)

	oldCheckout := newPayer(t, env, "cus_oldco")
	_, err := env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() - INTERVAL '8 days' WHERE user_id = $1`, oldCheckout)
	require.NoError(t, err)
	seedFakeSub(t, fake, "sub_oldco", "cus_oldco", nil)

	newPayer(t, env, "cus_noco") // last_checkout_at stays NULL
	seedFakeSub(t, fake, "sub_noco", "cus_noco", nil)

	haveLive := newPayer(t, env, "cus_haslive")
	_, err = env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() WHERE user_id = $1`, haveLive)
	require.NoError(t, err)
	seedStripeSub(t, env.DB, haveLive, "sub_haslive", "cus_haslive", "active", time.Now().Add(10*24*time.Hour))
	_, err = env.DB.Exec(`UPDATE subscriptions SET updated_at = NOW() WHERE stripe_subscription_id = 'sub_haslive'`)
	require.NoError(t, err)

	report, err := env.Svc.RunDrift(ctx)
	require.NoError(t, err)

	assert.Zero(t, report.Discovered)
	assert.Empty(t, fake.requestsTo("GET", subListPath), "no mapping was eligible, so no Stripe list was made")
	assert.Zero(t, countSubsBySubID(t, env.DB, "sub_oldco"), "a checkout older than 7 days is outside discovery")
	assert.Zero(t, countSubsBySubID(t, env.DB, "sub_noco"), "a mapping that never reached Checkout is outside discovery")
}

func TestDrift_DiscoveryOldestCheckedFirst(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := driftCtx(t)
	const candidates = discoveryBatch + 5
	for i := range candidates {
		user := newPayer(t, env, fmt.Sprintf("cus_disc%02d", i))
		_, err := env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() - INTERVAL '1 hour' WHERE user_id = $1`, user)
		require.NoError(t, err)
	}
	listed := func(from int) map[string]bool {
		out := map[string]bool{}
		for _, req := range fake.requestsTo("GET", subListPath)[from:] {
			out[req.Form.Get("customer")] = true
		}
		return out
	}

	_, err := env.Svc.RunDrift(ctx)
	require.NoError(t, err)
	first := listed(0)
	assert.Len(t, first, discoveryBatch, "one pass checks at most discoveryBatch customers")
	firstRequests := len(fake.requestsTo("GET", subListPath))
	assert.Equal(t, discoveryBatch, firstRequests, "each customer is listed once")

	_, err = env.Svc.RunDrift(ctx)
	require.NoError(t, err)
	second := listed(firstRequests)
	assert.Len(t, second, discoveryBatch)
	for i := range candidates {
		cus := fmt.Sprintf("cus_disc%02d", i)
		if !first[cus] {
			assert.True(t, second[cus], "%s was not checked in pass one, so pass two checks it first", cus)
		}
	}
}

func TestDrift_PrunesOldEvents(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	for id, age := range map[string]string{
		"evt_old":   "31 days",
		"evt_young": "29 days",
		"evt_new":   "0 seconds",
	} {
		_, err := env.DB.Exec(
			`INSERT INTO stripe_webhook_events (event_id, event_type, processed_at) VALUES ($1, 'customer.subscription.updated', NOW() - $2::interval)`,
			id, age)
		require.NoError(t, err)
	}

	report, err := env.Svc.RunDrift(driftCtx(t))
	require.NoError(t, err)

	assert.Equal(t, 1, report.Pruned)
	assert.Zero(t, dedupeCount(t, env.DB, "evt_old"), "older than 30 days is pruned")
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_young"), "29 days is kept")
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_new"))
}

// Two RunDrift calls at once exercise the mutex Task 9 adds around the
// discovery map; under -race an unguarded map access fails here.
func TestDrift_ConcurrentRunsAreRaceFree(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	for i := range 5 {
		user := newPayer(t, env, fmt.Sprintf("cus_race%d", i))
		_, err := env.DB.Exec(`UPDATE stripe_customers SET last_checkout_at = NOW() WHERE user_id = $1`, user)
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = env.Svc.RunDrift(context.Background())
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
}
