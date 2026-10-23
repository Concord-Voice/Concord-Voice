package billing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// Checkout session integration tests (#2033 PR-3, plan Tasks 4 and 5; spec
// §8 Checkout). The real handler runs against the test database and the
// Stripe fake; assertions read the form Stripe received, not only the status.

const (
	customerCreatePath = "/v1/customers"
	bodyWingYearly     = `{"plan":"wing","interval":"year"}`
)

type checkoutEnv struct {
	*testEnv
	Now *time.Time
}

func newCheckoutEnv(t *testing.T, billing config.BillingConfig) *checkoutEnv {
	t.Helper()
	fake := newFakeStripe(t)
	fake.seedCatalog()
	env := newTestService(t, fake)
	billing.KeyMode = config.StripeKeyModeTest
	env.Svc.Billing = billing
	now := testCutoff.Add(-24 * time.Hour)
	env.Svc.now = func() time.Time { return now }
	return &checkoutEnv{testEnv: env, Now: &now}
}

func postCheckout(svc *Service, userID uuid.UUID, body string) *httptest.ResponseRecorder {
	return postCheckoutCtx(context.Background(), svc, userID, body)
}

func postCheckoutCtx(ctx context.Context, svc *Service, userID uuid.UUID, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/billing/checkout-session", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID.String())
	svc.CheckoutSession(c)
	return w
}

func requireError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, w.Code, "body: %s", w.Body.String())
	assert.JSONEq(t, `{"error":"`+code+`"}`, w.Body.String())
}

func lastCheckoutAt(t *testing.T, env *checkoutEnv, userID uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	require.NoError(t, env.DB.QueryRow(`SELECT last_checkout_at FROM stripe_customers WHERE user_id = $1`, userID).Scan(&at))
	return at
}

func TestCheckout_CreatesSessionWithExpectedParams(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)

	w := postCheckout(env.Svc, user, bodyWingYearly)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var got map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Len(t, got, 2)
	assert.True(t, strings.HasPrefix(got["url"], "https://checkout.stripe.test/c/pay/"))
	assert.Equal(t, env.Now.Add(35*time.Minute).UTC().Format(time.RFC3339), got["expires_at"])

	creates := env.Fake.requestsTo(http.MethodPost, customerCreatePath)
	require.Len(t, creates, 1)
	assert.Equal(t, "concord-customer-v1:"+user.String(), creates[0].IdempotencyKey)
	assert.Equal(t, map[string]string{subMetaUserID: user.String(), subMetaEnv: testEnvironment}, formMetadata(creates[0].Form))
	for key := range creates[0].Form {
		assert.NotContains(t, []string{"email", "name"}, key, "the Customer carries no email or name")
	}
	cus := mappedCustomer(t, env.DB, user)
	assert.Equal(t, "cus_1", cus)
	assert.NotNil(t, lastCheckoutAt(t, env, user))

	sessions := env.Fake.requestsTo(http.MethodPost, sessionsListPath)
	require.Len(t, sessions, 1)
	f := sessions[0].Form
	want := map[string]string{
		"mode":                            "subscription",
		"customer":                        cus,
		"client_reference_id":             user.String(),
		"line_items[0][price]":            "price_WY",
		"line_items[0][quantity]":         "1",
		"allowed_payment_method_types[0]": "card",
		"adaptive_pricing[enabled]":       "false",
		"subscription_data[metadata][concord_user_id]": user.String(),
		"subscription_data[metadata][concord_env]":     testEnvironment,
		"subscription_data[metadata][concord_plan]":    PlanWing,
		"success_url":          ReturnURLCheckoutSuccess,
		"cancel_url":           ReturnURLCheckoutCancel,
		"discounts[0][coupon]": CouponFoundersID,
	}
	want["expires_at"] = strconv.FormatInt(env.Now.Add(35*time.Minute).Unix(), 10)
	for k, v := range want {
		assert.Equal(t, v, f.Get(k), "form field %s", k)
	}
	for _, absent := range []string{"allow_promotion_codes", "automatic_tax[enabled]", "billing_address_collection", "customer_update[address]"} {
		assert.False(t, f.Has(absent), "%s is not sent", absent)
	}
}

func TestCheckout_ReusesMappingAndStampsIt(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.seedCustomer("cus_existing")
	user := newPayer(t, env.testEnv, "cus_existing")
	require.Nil(t, lastCheckoutAt(t, env, user))

	require.Equal(t, http.StatusCreated, postCheckout(env.Svc, user, bodyWingYearly).Code)
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, customerCreatePath), "an existing mapping is reused")
	sessions := env.Fake.requestsTo(http.MethodPost, sessionsListPath)
	require.Len(t, sessions, 1)
	assert.Equal(t, "cus_existing", sessions[0].Form.Get("customer"))
	assert.NotNil(t, lastCheckoutAt(t, env, user))
}

func TestCheckout_ModeOffAndNotAllowlistedAreIdentical(t *testing.T) {
	off := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOff))
	offW := postCheckout(off.Svc, dbtest.CreateUser(t, off.DB), bodyWingYearly)
	listed := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeAllowlist, uuid.NewString()))
	listedW := postCheckout(listed.Svc, dbtest.CreateUser(t, listed.DB), bodyWingYearly)

	requireError(t, offW, http.StatusServiceUnavailable, "purchase_unavailable")
	assert.Equal(t, offW.Body.Bytes(), listedW.Body.Bytes())
	assert.Equal(t, offW.Code, listedW.Code)
	assert.Equal(t, offW.Header(), listedW.Header())
	assert.Empty(t, off.Fake.writes())
	assert.Empty(t, listed.Fake.writes())
	assert.Empty(t, off.Fake.requestsTo(http.MethodGet, pricesPath), "the gate runs before Stripe is asked")
}

func TestCheckout_AllowlistedUserCanBuy(t *testing.T) {
	env := newCheckoutEnv(t, config.BillingConfig{PurchaseMode: config.BillingPurchaseModeAllowlist})
	user := dbtest.CreateUser(t, env.DB)
	env.Svc.Billing.PurchaseAllowlist = []string{strings.ToUpper(user.String())}
	assert.Equal(t, http.StatusCreated, postCheckout(env.Svc, user, bodyWingYearly).Code)
}

func TestCheckout_FoundersBoundary(t *testing.T) {
	// Subtests, so each one's database cleanup runs before the next: every
	// fake numbers its first Customer cus_1, and the mapping is unique.
	for offset, want := range map[time.Duration]bool{-time.Second: true, 0: false, time.Second: false} {
		t.Run(offset.String(), func(t *testing.T) {
			env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
			*env.Now = testCutoff.Add(offset)
			require.Equal(t, http.StatusCreated, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), bodyWingYearly).Code)
			sessions := env.Fake.requestsTo(http.MethodPost, sessionsListPath)
			require.Len(t, sessions, 1)
			assert.Equal(t, want, sessions[0].Form.Has("discounts[0][coupon]"))
		})
	}
}

func TestCheckout_AutomaticTaxOnlyWhenConfigured(t *testing.T) {
	cfg := foundersConfig(config.BillingPurchaseModeOn)
	cfg.AutomaticTax = true
	env := newCheckoutEnv(t, cfg)
	require.Equal(t, http.StatusCreated, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), bodyWingYearly).Code)
	f := env.Fake.requestsTo(http.MethodPost, sessionsListPath)[0].Form
	assert.Equal(t, "true", f.Get("automatic_tax[enabled]"))
	assert.Equal(t, "required", f.Get("billing_address_collection"))
	assert.Equal(t, "auto", f.Get("customer_update[address]"))
}

func TestCheckout_ManageExistingIs409(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := newPayer(t, env.testEnv, "cus_payer")
	seedStripeSub(t, env.DB, user, "sub_live", "cus_payer", "active", time.Unix(testPeriodEnd, 0))

	requireError(t, postCheckout(env.Svc, user, bodyWingYearly), http.StatusConflict, "manage_existing")
	assert.Empty(t, env.Fake.writes(), "no Customer and no session")
}

func TestCheckout_EndedSubscriptionDoesNotBlock(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.seedCustomer("cus_payer")
	user := newPayer(t, env.testEnv, "cus_payer")
	seedStripeSub(t, env.DB, user, "sub_old", "cus_payer", "canceled", time.Now().Add(-48*time.Hour))
	assert.Equal(t, http.StatusCreated, postCheckout(env.Svc, user, bodyWingYearly).Code)
}

func TestCheckout_ExpiresOpenSessionsFirst(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.seedCustomer("cus_payer")
	user := newPayer(t, env.testEnv, "cus_payer")
	env.Fake.seedSession("cs_old", "cus_payer", "open")
	env.Fake.seedSession("cs_done", "cus_payer", "complete")

	require.Equal(t, http.StatusCreated, postCheckout(env.Svc, user, bodyWingYearly).Code)
	assert.Equal(t, "expired", env.Fake.sessionStatus("cs_old"))
	assert.Equal(t, "complete", env.Fake.sessionStatus("cs_done"))
	writes := env.Fake.writes()
	require.Len(t, writes, 2)
	assert.Equal(t, sessionsListPath+"/cs_old/expire", writes[0].Path)
	assert.Equal(t, sessionsListPath, writes[1].Path, "the new session is created after the old one expired")
}

func TestCheckout_ArchivedPriceIs503(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.archivePrice(LookupKeyWingYearly)
	requireError(t, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
	assert.Empty(t, env.Fake.writes())
	// The other SKUs still sell.
	assert.Equal(t, http.StatusCreated, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), `{"plan":"wing","interval":"month"}`).Code)
}

func TestCheckout_FoundersCouponDriftIs503(t *testing.T) {
	for field, edit := range couponDrifts {
		t.Run(field, func(t *testing.T) {
			env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
			driftCoupon(env.Fake, edit)
			requireError(t, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
			assert.Empty(t, env.Fake.writes(), "no Customer and no session")
		})
	}
}

// Checkout reads the coupon itself: a /plans response cached before the drift
// does not let a purchase through.
func TestCheckout_FoundersCouponCheckedUncached(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	require.Equal(t, http.StatusOK, getPlans(env.Svc, uuid.New()).Code)
	driftCoupon(env.Fake, couponDrifts["percent_off"])
	requireError(t, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
}

func TestCheckout_FoundersEndedIgnoresCoupon(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	*env.Now = testCutoff
	driftCoupon(env.Fake, nil)
	assert.Equal(t, http.StatusCreated, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), bodyWingYearly).Code)
}

func TestCheckout_DeletedCustomerSelfHeals(t *testing.T) {
	for name, body := range map[string]string{
		"param customer": customerMissingParamBody,
		"no param":       customerMissingNoParamBody,
	} {
		t.Run(name, func(t *testing.T) { checkDeletedCustomerSelfHeals(t, body) })
	}
}

func checkDeletedCustomerSelfHeals(t *testing.T, refusal string) {
	t.Helper()
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := newPayer(t, env.testEnv, "cus_old")
	env.Fake.seedDeletedCustomer("cus_old")
	env.Fake.refuseSessionWith("cus_old", refusal)

	require.Equal(t, http.StatusCreated, postCheckout(env.Svc, user, bodyWingYearly).Code)
	creates := env.Fake.requestsTo(http.MethodPost, customerCreatePath)
	require.Len(t, creates, 1)
	assert.Equal(t, "concord-customer-v1:"+user.String()+":after:cus_old", creates[0].IdempotencyKey)
	assert.Equal(t, "cus_1", mappedCustomer(t, env.DB, user))
	sessions := env.Fake.requestsTo(http.MethodPost, sessionsListPath)
	require.Len(t, sessions, 2, "one refused create, one retry")
	assert.Equal(t, "cus_1", sessions[1].Form.Get("customer"))
	assert.NotNil(t, lastCheckoutAt(t, env, user))
}

func TestCheckout_UnconfirmedMissingCustomerKeepsMapping(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.seedCustomer("cus_live")
	user := newPayer(t, env.testEnv, "cus_live")
	env.Fake.refuseSessionFor("cus_live")

	requireError(t, postCheckout(env.Svc, user, bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
	assert.Equal(t, "cus_live", mappedCustomer(t, env.DB, user))
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, customerCreatePath))
	assert.Contains(t, env.Logs.String(), classCustomerUnverifiable)
	assert.Contains(t, env.Logs.String(), "cus_live", "the log names the Customer to look up")
}

func TestCheckout_OtherInvalidRequestIs503WithParam(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	env.Fake.failOn(http.MethodPost, sessionsListPath, http.StatusBadRequest,
		`{"error":{"type":"invalid_request_error","param":"discounts[0][coupon]","message":"No such coupon"}}`)
	requireError(t, postCheckout(env.Svc, dbtest.CreateUser(t, env.DB), bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
	assert.Contains(t, env.Logs.String(), "discounts[0][coupon]")
	assert.NotContains(t, env.Logs.String(), "No such coupon", "Stripe's message is not logged")
}

func TestCheckout_RejectsBadBodies(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)
	for name, body := range map[string]string{
		"unknown plan":     `{"plan":"platinum","interval":"month"}`,
		"unknown interval": `{"plan":"wing","interval":"week"}`,
		"missing interval": `{"plan":"wing"}`,
		"empty":            ``,
		"two documents":    bodyWingYearly + `{}`,
		"not json":         `plan=wing`,
	} {
		t.Run(name, func(t *testing.T) {
			requireError(t, postCheckout(env.Svc, user, body), http.StatusBadRequest, "invalid_request")
		})
	}
	big := `{"plan":"wing","interval":"year","x":"` + strings.Repeat("a", checkoutMaxBody) + `"}`
	requireError(t, postCheckout(env.Svc, user, big), http.StatusRequestEntityTooLarge, "payload_too_large")
	assert.Empty(t, env.Fake.writes())
}

func TestCheckout_ConcurrentFirstCallsShareOneCustomer(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = postCheckout(env.Svc, user, bodyWingYearly).Code
		}()
	}
	wg.Wait()
	for _, code := range codes {
		assert.Equal(t, http.StatusCreated, code)
	}
	assert.Equal(t, "cus_1", mappedCustomer(t, env.DB, user))
	assert.Equal(t, 1, countTable(t, env.DB, "stripe_customers"))
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"), "the same Customer is not queued for deletion")
	for _, req := range env.Fake.requestsTo(http.MethodPost, sessionsListPath) {
		assert.Equal(t, "cus_1", req.Form.Get("customer"))
	}
}

func TestCheckout_ErasedUserQueuesNewCustomer(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)
	env.Fake.setOnCustomerCreate(func() {
		_, err := env.DB.Exec(`DELETE FROM users WHERE id = $1`, user)
		assert.NoError(t, err)
	})

	requireError(t, postCheckout(env.Svc, user, bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
	assert.Zero(t, countTable(t, env.DB, "stripe_customers"))
	assert.Equal(t, 1, scalarInt(t, env.DB, `SELECT COUNT(*) FROM billing_erasure_outbox WHERE stripe_customer_id = 'cus_1'`))
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, sessionsListPath))
}

// A request that loses the mapping race to a different Customer (a self-heal
// racing a first purchase) uses the stored one and queues its own for deletion.
func TestCheckout_LostMappingRaceQueuesOwnCustomer(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)
	env.Fake.setOnCustomerCreate(func() {
		env.Fake.customers["cus_other"] = true // under the fake's lock
		_, err := env.DB.Exec(`INSERT INTO stripe_customers (user_id, stripe_customer_id) VALUES ($1, 'cus_other')`, user)
		assert.NoError(t, err)
	})

	require.Equal(t, http.StatusCreated, postCheckout(env.Svc, user, bodyWingYearly).Code)
	assert.Equal(t, "cus_other", mappedCustomer(t, env.DB, user))
	assert.Equal(t, "cus_other", env.Fake.requestsTo(http.MethodPost, sessionsListPath)[0].Form.Get("customer"))
	assert.Equal(t, 1, scalarInt(t, env.DB, `SELECT COUNT(*) FROM billing_erasure_outbox WHERE stripe_customer_id = 'cus_1'`))
}

// A client that disconnects while Stripe creates the Customer still leaves the
// Customer mapped, so it is never orphaned (review fix 1).
func TestCheckout_DisconnectDuringCustomerCreateStillMaps(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)
	ctx, cancel := context.WithCancel(context.Background())
	env.Fake.setOnCustomerCreate(cancel)

	assert.Equal(t, http.StatusServiceUnavailable, postCheckoutCtx(ctx, env.Svc, user, bodyWingYearly).Code)
	assert.Equal(t, "cus_1", mappedCustomer(t, env.DB, user), "the Customer is mapped despite the disconnect")
	assert.Zero(t, countTable(t, env.DB, "billing_erasure_outbox"))
}

// An erasure during the deleted-Customer replacement is logged as the erasure
// it is, not as a session failure (review fix 3).
func TestCheckout_ErasedDuringReplacementLogsAsErasure(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := newPayer(t, env.testEnv, "cus_old")
	env.Fake.seedDeletedCustomer("cus_old")
	env.Fake.refuseSessionFor("cus_old")
	env.Fake.setOnCustomerCreate(func() {
		_, err := env.DB.Exec(`DELETE FROM users WHERE id = $1`, user)
		assert.NoError(t, err)
	})

	requireError(t, postCheckout(env.Svc, user, bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
	assert.Equal(t, 1, scalarInt(t, env.DB, `SELECT COUNT(*) FROM billing_erasure_outbox WHERE stripe_customer_id = 'cus_1'`))
	logs := env.Logs.String()
	assert.Contains(t, logs, "erased mid-request; its new Customer is queued for deletion")
	assert.NotContains(t, logs, "checkout failed")
}

// A key replay that returns a Customer the erasure outbox still holds is never
// mapped: the outbox would delete it under the new mapping. Checkout creates a
// fresh Customer under a key naming the queued one instead (Codex P1).
func TestCheckout_NeverMapsACustomerQueuedForDeletion(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)
	_, err := env.DB.Exec(`INSERT INTO billing_erasure_outbox (stripe_customer_id) VALUES ('cus_1')`)
	require.NoError(t, err)

	require.Equal(t, http.StatusCreated, postCheckout(env.Svc, user, bodyWingYearly).Code)
	assert.Equal(t, "cus_2", mappedCustomer(t, env.DB, user))
	creates := env.Fake.requestsTo(http.MethodPost, customerCreatePath)
	require.Len(t, creates, 2)
	assert.Equal(t, "concord-customer-v1:"+user.String()+":after:cus_1", creates[1].IdempotencyKey)
	assert.Equal(t, "cus_2", env.Fake.requestsTo(http.MethodPost, sessionsListPath)[0].Form.Get("customer"))
}

// An erasure that commits after the Customer is ready but before the session
// exists must not leave a payable session: Checkout expires it and answers 503
// (Codex P1, cycle 3).
func TestCheckout_ErasureDuringSessionCreateExpiresTheSession(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := newPayer(t, env.testEnv, "cus_live")
	env.Fake.setOnSessionCreate(func() {
		_, err := env.DB.Exec(`DELETE FROM users WHERE id = $1`, user)
		assert.NoError(t, err)
	})

	requireError(t, postCheckout(env.Svc, user, bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
	require.Len(t, env.Fake.sessions, 1)
	assert.Equal(t, "expired", env.Fake.sessions[0].Status, "the session is not left payable")
}

// A mapping insert that times out behind an erasure holding the user's row
// waits the erasure out and queues the new Customer (Codex P1, cycle 3).
func TestCheckout_InsertTimeoutBehindErasureQueuesCustomer(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	user := dbtest.CreateUser(t, env.DB)
	saved := mapTimeout
	mapTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mapTimeout = saved })

	tx, err := env.DB.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	committed := make(chan error, 1)
	env.Fake.setOnCustomerCreate(func() {
		// The erasure deletes the user and holds the row until Checkout's
		// re-check is waiting on it, after the insert has timed out.
		_, err := tx.Exec(`DELETE FROM users WHERE id = $1`, user)
		assert.NoError(t, err)
		go func() { committed <- commitWhenWaiting(env.DB, tx, "FOR KEY SHARE") }()
	})

	requireError(t, postCheckout(env.Svc, user, bodyWingYearly), http.StatusServiceUnavailable, "billing_unavailable")
	require.NoError(t, <-committed)
	assert.Equal(t, 1, scalarInt(t, env.DB, `SELECT COUNT(*) FROM billing_erasure_outbox WHERE stripe_customer_id = 'cus_1'`))
	assert.Contains(t, env.Logs.String(), "erased mid-request; its new Customer is queued for deletion")
}

// commitWhenWaiting commits tx once a query containing marker waits on a lock.
func commitWhenWaiting(db *sql.DB, tx *sql.Tx, marker string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%' AND pid <> pg_backend_pid())`, marker).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting {
			return tx.Commit()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("no query waited on the lock")
}
