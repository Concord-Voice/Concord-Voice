package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Webhook handler integration tests (#2033 PR-2, Task 5): the real test
// database, the Stripe fake and a recording TierChanger. Status codes, rows,
// the response's error class and the log's message are the contract.

const (
	eventSubscriptionUpdated = "customer.subscription.updated"
	eventCheckoutCompleted   = "checkout.session.completed"
)

// deliverSigned signs body with the Service's own secret at the current time
// and delivers it.
func deliverSigned(env *testEnv, body []byte) *httptest.ResponseRecorder {
	return deliver(env.Svc, body, signHeader(body, testWebhookSecret, time.Now()))
}

// requireReceived asserts a 200 with the {"received":true} body.
func requireReceived(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, map[string]any{"received": true}, got)
}

func subscriptionEvent(t *testing.T, eventID, eventType, subID, customerID string) []byte {
	t.Helper()
	return eventBody(t, eventID, eventType, false, stripe.APIVersion, subscriptionObject(subID, customerID))
}

func TestWebhook_SubscriptionUpdatedReconciles200(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	user := newPayer(t, env, "cus_wh1")
	seedFakeSub(t, fake, "sub_wh1", "cus_wh1", nil)

	w := deliverSigned(env, subscriptionEvent(t, "evt_wh1", eventSubscriptionUpdated, "sub_wh1", "cus_wh1"))

	requireReceived(t, w)
	row := loadSubRow(t, env.DB, "sub_wh1")
	assert.Equal(t, user, row.UserID)
	assert.Equal(t, "stripe", row.Source)
	assert.Equal(t, entitlements.TierPremium, row.Tier)
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_wh1"))
	assert.Equal(t, []tierCall{{UserID: user, Old: entitlements.TierFree, New: entitlements.TierPremium}},
		env.Changer.Calls())
	assert.Empty(t, env.Svc.sem, "the semaphore token is released when the handler returns")
}

func TestWebhook_DuplicateEvent200WithoutRefetch(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_dup")
	seedFakeSub(t, fake, "sub_dup", "cus_dup", nil)
	body := subscriptionEvent(t, "evt_dup", eventSubscriptionUpdated, "sub_dup", "cus_dup")

	requireReceived(t, deliverSigned(env, body))
	requireReceived(t, deliverSigned(env, body))

	assert.Len(t, fake.requestsTo("GET", subscriptionGetPath+"sub_dup"), 1, "the second delivery is acknowledged from the dedupe table")
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_dup"))
	assert.Len(t, env.Changer.Calls(), 1)
}

func TestWebhook_CheckoutCompletedReconcilesItsSubscription(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_co")
	seedFakeSub(t, fake, "sub_co", "cus_co", nil)
	body := eventBody(t, "evt_co", eventCheckoutCompleted, false, stripe.APIVersion,
		map[string]any{"id": "cs_co", "object": "checkout.session", "customer": "cus_co", "subscription": "sub_co"})

	requireReceived(t, deliverSigned(env, body))

	assert.Equal(t, entitlements.TierPremium, loadSubRow(t, env.DB, "sub_co").Tier)
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_co"))
}

func TestWebhook_CheckoutWithoutSubscriptionIsDeduped200(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	body := eventBody(t, "evt_cons", eventCheckoutCompleted, false, stripe.APIVersion,
		map[string]any{"id": "cs_none", "object": "checkout.session", "customer": "cus_x", "subscription": nil})

	requireReceived(t, deliverSigned(env, body))

	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_cons"))
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
	assert.Empty(t, fake.requestsTo("GET", subscriptionGetPath+"cs_none"))
}

func TestWebhook_CustomerDeletedReconcilesAndDropsMapping(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	seedDeletedCustomerSubs(t, env, "cus_whd", "sub_whd_a", "sub_whd_b")
	body := eventBody(t, "evt_whd", customerDeletedType, false, stripe.APIVersion,
		map[string]any{"id": "cus_whd", "object": "customer", "deleted": true})

	requireReceived(t, deliverSigned(env, body))

	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_whd_a").Status)
	assert.Equal(t, "canceled", loadSubRow(t, env.DB, "sub_whd_b").Status)
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"))
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_whd"))
}

func TestWebhook_UnknownType200Deduped(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	body := eventBody(t, "evt_unk", "invoice.paid", false, stripe.APIVersion,
		map[string]any{"id": "in_1", "object": "invoice", "subscription": "sub_unk"})

	requireReceived(t, deliverSigned(env, body))

	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_unk"), "recorded so a retry is a cheap no-op")
	assert.Empty(t, fake.requestsTo("GET", subscriptionGetPath+"sub_unk"), "a type we do not subscribe to never reaches Stripe")
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
}

func TestWebhook_MissingObjectIDIsDedupedWithErrorLog(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	body := eventBody(t, "evt_noid", eventSubscriptionUpdated, false, stripe.APIVersion,
		map[string]any{"object": "subscription"})

	requireReceived(t, deliverSigned(env, body))

	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_noid"), "a retry could never succeed, so it is acknowledged")
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "billing: webhook event names no object id; recorded and acknowledged")
	assert.Empty(t, fake.writes())
}

// RF1 (R36 path): dev boxes share one Stripe test account, so an event can name
// a subscription of another environment whose customer is unmapped here.
func TestWebhook_UnactionableIsDedupedAnd200(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	user := dbtest.CreateUser(t, env.DB)
	seedFakeSub(t, fake, "sub_rf1", "cus_rf1", map[string]string{
		subMetaUserID: user.String(), subMetaEnv: "some-other-box",
	})

	w := deliverSigned(env, subscriptionEvent(t, "evt_rf1", eventSubscriptionUpdated, "sub_rf1", "cus_rf1"))

	requireReceived(t, w)
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_rf1"), "acknowledged and recorded: Stripe must stop retrying")
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"), "no row")
	assert.Equal(t, 0, countTable(t, env.DB, "stripe_customers"), "no mapping")
	assert.Empty(t, env.Changer.Calls())
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=WARN")
	assert.Contains(t, logs, "billing: subscription belongs to another environment; nothing written")
	assert.Contains(t, logs, "billing: webhook event is unactionable; recorded and acknowledged")
	assert.NotContains(t, logs, "level=ERROR", "a foreign environment is expected on a shared account")
}

func TestWebhook_MissingSubscriptionIsDeduped200(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake) // the fake holds no sub_gone: resource_missing

	requireReceived(t, deliverSigned(env, subscriptionEvent(t, "evt_gone", "customer.subscription.deleted", "sub_gone", "cus_gone")))

	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_gone"))
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
}

func TestWebhook_StripeOutage500NoDedupe(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_out")
	seedFakeSub(t, fake, "sub_out", "cus_out", nil)
	fake.failOn("GET", subscriptionGetPath+"sub_out", http.StatusServiceUnavailable, serverErrorBody)
	body := subscriptionEvent(t, "evt_out", eventSubscriptionUpdated, "sub_out", "cus_out")

	w := deliverSigned(env, body)

	assert.Equal(t, http.StatusInternalServerError, w.Code, "Stripe retries a 5xx")
	assert.Equal(t, webhookErrInternal, errorClassOf(t, w))
	assert.Equal(t, 0, dedupeCount(t, env.DB, "evt_out"), "no dedupe row: the retry must reprocess the event")
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
	logs := env.Logs.String()
	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "billing: webhook reconcile failed; Stripe will retry")
	assert.Empty(t, env.Svc.sem, "the token is released on the error path too")

	clearFailure(fake, "GET", subscriptionGetPath+"sub_out")
	requireReceived(t, deliverSigned(env, body))
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_out"), "the redelivery succeeds once Stripe recovers")
}

func TestWebhook_Saturated503(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_sat")
	seedFakeSub(t, fake, "sub_sat", "cus_sat", nil)
	for range cap(env.Svc.sem) {
		env.Svc.sem <- struct{}{}
	}
	body := subscriptionEvent(t, "evt_sat", eventSubscriptionUpdated, "sub_sat", "cus_sat")

	w := deliverSigned(env, body)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "the acquire never blocks: Stripe retries later")
	assert.Equal(t, webhookErrBusy, errorClassOf(t, w))
	assert.Contains(t, env.Logs.String(), "billing: webhook reconcile capacity full; Stripe will retry")
	assert.Equal(t, 0, dedupeCount(t, env.DB, "evt_sat"))
	assert.Empty(t, fake.requestsTo("GET", subscriptionGetPath+"sub_sat"), "a saturated handler makes no Stripe call")
	assert.Len(t, env.Svc.sem, cap(env.Svc.sem), "a rejected delivery does not release tokens it never took")

	for range cap(env.Svc.sem) {
		<-env.Svc.sem
	}
	requireReceived(t, deliverSigned(env, body))
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_sat"))
}

// A refused delivery writes nothing, whichever check refuses it.
func TestWebhook_RefusedDeliveriesWriteNothing(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_ref")
	seedFakeSub(t, fake, "sub_ref", "cus_ref", nil)

	liveBody := eventBody(t, "evt_live", eventSubscriptionUpdated, true, stripe.APIVersion, subscriptionObject("sub_ref", "cus_ref"))
	w := deliverSigned(env, liveBody)
	assert.Equal(t, http.StatusBadRequest, w.Code, "livemode event on a test-mode key")
	assert.Equal(t, webhookErrModeMismatch, errorClassOf(t, w))

	okBody := subscriptionEvent(t, "evt_badsig", eventSubscriptionUpdated, "sub_ref", "cus_ref")
	w = deliver(env.Svc, okBody, signHeader(okBody, otherWebhookKey, time.Now()))
	assert.Equal(t, http.StatusBadRequest, w.Code, "wrong secret")
	assert.Equal(t, webhookErrInvalidEvent, errorClassOf(t, w))

	assert.Equal(t, 0, countTable(t, env.DB, "stripe_webhook_events"))
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
	assert.Empty(t, fake.requestsTo("GET", subscriptionGetPath+"sub_ref"))
}

// The signed timestamp may sit anywhere inside the tolerance window.
func TestWebhook_TimestampInsideToleranceAccepted(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_tol")
	seedFakeSub(t, fake, "sub_tol", "cus_tol", nil)
	body := subscriptionEvent(t, "evt_tol", eventSubscriptionUpdated, "sub_tol", "cus_tol")

	w := deliver(env.Svc, body, signHeader(body, testWebhookSecret, time.Now().Add(-(webhookTolerance-30*time.Second))))

	requireReceived(t, w)
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_tol"))
}

// The body is only a trigger: the handler re-fetches state at the pinned API
// version, so an event stamped with an older version is still processed.
func TestWebhook_VersionMismatchAccepted(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	newPayer(t, env, "cus_ver")
	seedFakeSub(t, fake, "sub_ver", "cus_ver", nil)
	body := eventBody(t, "evt_ver", eventSubscriptionUpdated, false, "2020-08-27", subscriptionObject("sub_ver", "cus_ver"))

	w := deliverSigned(env, body)

	require.NotEqual(t, http.StatusBadRequest, w.Code, "an API version mismatch must not reject a verified event")
	requireReceived(t, w)
	assert.Equal(t, 1, dedupeCount(t, env.DB, "evt_ver"))
	assert.Equal(t, entitlements.TierPremium, loadSubRow(t, env.DB, "sub_ver").Tier)
}

// RF2: during Stripe's 24 h dual-secret window the header carries two v1
// signatures and only one is made with our secret, in either order.
func TestWebhook_RotationHeaderWithOneValidSignature(t *testing.T) {
	cases := []struct {
		name    string
		suffix  string
		secrets []string
	}{
		{"valid signature second", "2", []string{otherWebhookKey, testWebhookSecret}},
		{"valid signature first", "1", []string{testWebhookSecret, otherWebhookKey}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStripe(t)
			env := newTestService(t, fake)
			cus, sub, evt := "cus_rot"+tc.suffix, "sub_rot"+tc.suffix, "evt_rot"+tc.suffix
			newPayer(t, env, cus)
			seedFakeSub(t, fake, sub, cus, nil)
			body := subscriptionEvent(t, evt, eventSubscriptionUpdated, sub, cus)

			w := deliver(env.Svc, body, rotationHeader(body, time.Now(), tc.secrets...))

			require.NotEqual(t, http.StatusBadRequest, w.Code)
			requireReceived(t, w)
			assert.Equal(t, 1, dedupeCount(t, env.DB, evt))
			assert.Equal(t, 1, countSubsBySubID(t, env.DB, sub))
		})
	}
}

// Spec §20, first criterion: returning from Checkout grants nothing. Only a
// verified webhook (or the drift worker) writes the row that entitles.
func TestWebhook_SuccessRedirectNeverGrants(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	ctx := reconcileCtx(t)
	user := newPayer(t, env, "cus_redir")
	seedFakeSub(t, fake, "sub_redir", "cus_redir", nil) // paid and active at Stripe

	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, env.DB, user.String()),
		"no webhook delivered: the user is still free however Stripe's state looks")
	assert.Equal(t, 0, countUserSubs(t, env.DB, user))

	requireReceived(t, deliverSigned(env, subscriptionEvent(t, "evt_redir", eventSubscriptionUpdated, "sub_redir", "cus_redir")))

	assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, env.DB, user.String()),
		"the verified webhook is what grants")
}
