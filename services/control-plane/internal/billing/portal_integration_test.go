package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Portal session endpoint integration tests (#2033 PR-2, Task 6; spec §8
// Portal). They run the real handler through a Gin test context against the
// test database and the Stripe fake. The caller's identity is set the way
// middleware.AuthRequired sets it: c.Set("user_id", <canonical uuid string>).

const (
	portalConfigsPath  = "/v1/billing_portal/configurations"
	portalSessionsPath = "/v1/billing_portal/sessions"
	portalSessionBase  = "https://billing.stripe.test/p/session/"
	portalV1ConfigID   = "bpc_v1"

	flowPaymentMethodUpdate = "payment_method_update"
	flowSubscriptionCancel  = "subscription_cancel"

	// portalBodyCap is the 4 KiB request cap the plan fixes.
	portalBodyCap = 4 << 10

	errNoBillingAccount  = "no_billing_account"
	errBillingUnavail    = "billing_unavailable"
	errInvalidRequest    = "invalid_request"
	paramBodyFlowPayment = `{"flow":"payment_method_update"}`
)

// portalEnv is a Service whose fake holds the one owned (concord_portal=v1)
// configuration, and one payer mapped to a Stripe Customer.
type portalEnv struct {
	*testEnv
	User     uuid.UUID
	Customer string
}

func newPortalEnv(t *testing.T) *portalEnv {
	t.Helper()
	fake := newFakeStripe(t)
	fake.seedPortal(portalV1ConfigID, map[string]string{PortalMetadataKey: PortalMetadataVersion})
	env := newTestService(t, fake)
	const customer = "cus_portal"
	return &portalEnv{testEnv: env, User: newPayer(t, env, customer), Customer: customer}
}

// postPortal posts body to svc.PortalSession as userID and returns the recorder.
func postPortal(svc *Service, userID uuid.UUID, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/billing/portal-session", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID.String())
	svc.PortalSession(c)
	return w
}

// portalURL decodes a 201 body, requires it to hold exactly {"url": ...}, and
// returns the url.
func portalURL(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "body is JSON: %q", w.Body.String())
	require.Len(t, got, 1, "the body is exactly {url}")
	u, ok := got["url"].(string)
	require.True(t, ok, "url is a string")
	return u
}

// portalErrorClass returns the "error" class of a non-2xx body.
func portalErrorClass(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "error bodies are JSON: %q", w.Body.String())
	return got.Error
}

// onlyPortalSession returns the one portal-session create the fake received.
func onlyPortalSession(t *testing.T, fake *fakeStripe) fakeRequest {
	t.Helper()
	reqs := fake.requestsTo(http.MethodPost, portalSessionsPath)
	require.Len(t, reqs, 1, "exactly one Portal session create")
	return reqs[0]
}

// stripeRequestCount is how many requests the fake has received, read under its lock.
func stripeRequestCount(fake *fakeStripe) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.requests)
}

// hasFlowData reports whether any form key belongs to flow_data.
func hasFlowData(req fakeRequest) bool {
	for k := range req.Form {
		if strings.HasPrefix(k, "flow_data") {
			return true
		}
	}
	return false
}

func TestPortal_NoMapping404(t *testing.T) {
	env := newPortalEnv(t)
	stranger := dbtest.CreateUser(t, env.DB) // a real user with no stripe_customers row

	w := postPortal(env.Svc, stranger, `{}`)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, errNoBillingAccount, portalErrorClass(t, w))
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, portalSessionsPath), "no Portal session for a user with no Customer")
	assert.Empty(t, env.Fake.writes(), "a Portal visit never creates a Customer or anything else")
}

func TestPortal_DefaultFlow201(t *testing.T) {
	env := newPortalEnv(t)

	w := postPortal(env.Svc, env.User, `{}`)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, portalSessionBase+"bps_1", portalURL(t, w), "the body url is the fake's session url")

	lists := env.Fake.requestsTo(http.MethodGet, portalConfigsPath)
	require.Len(t, lists, 1)
	assert.Equal(t, "true", lists[0].Form.Get("active"), "only active configurations are listed")

	req := onlyPortalSession(t, env.Fake)
	assert.Equal(t, portalV1ConfigID, req.Form.Get("configuration"))
	assert.Equal(t, env.Customer, req.Form.Get("customer"))
	assert.Equal(t, ReturnURLPortal, req.Form.Get("return_url"))
	assert.False(t, hasFlowData(req), "no flow requested, no flow_data sent: %v", req.Form)
}

func TestPortal_PicksOwnedConfigurationAmongSeveral(t *testing.T) {
	fake := newFakeStripe(t)
	fake.seedPortal("bpc_dashboard_default", nil)
	fake.seedPortal(portalV1ConfigID, map[string]string{PortalMetadataKey: PortalMetadataVersion})
	fake.seedPortal("bpc_other_version", map[string]string{PortalMetadataKey: "v2"})
	env := newTestService(t, fake)
	user := newPayer(t, env, "cus_several")

	w := postPortal(env.Svc, user, `{}`)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, portalV1ConfigID, onlyPortalSession(t, fake).Form.Get("configuration"),
		"only the configuration marked concord_portal=v1 is ours, whatever its position")
}

func TestPortal_PaymentMethodUpdateFlow(t *testing.T) {
	env := newPortalEnv(t)

	w := postPortal(env.Svc, env.User, paramBodyFlowPayment)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.NotEmpty(t, portalURL(t, w))
	req := onlyPortalSession(t, env.Fake)
	assert.Equal(t, flowPaymentMethodUpdate, req.Form.Get("flow_data[type]"))
	assert.Equal(t, env.Customer, req.Form.Get("customer"))
	assert.Equal(t, portalV1ConfigID, req.Form.Get("configuration"))
	assert.Equal(t, ReturnURLPortal, req.Form.Get("return_url"))
	assert.Empty(t, req.Form.Get("flow_data[subscription_cancel][subscription]"),
		"a card update names no subscription")
	// payment_method_update needs no subscription row, so none was needed to get here.
	assert.Equal(t, 0, countTable(t, env.DB, "subscriptions"))
}

func TestPortal_CancelFlowCarriesSubscription(t *testing.T) {
	env := newPortalEnv(t)
	earlier := time.Unix(testPeriodEnd, 0).UTC().AddDate(-1, 0, 0)
	later := time.Unix(testPeriodEnd, 0).UTC()
	latest := later.AddDate(1, 0, 0)
	seedStripeSub(t, env.DB, env.User, "sub_older_live", env.Customer, "active", earlier)
	seedStripeSub(t, env.DB, env.User, "sub_newer_live", env.Customer, "active", later)
	env.Fake.seedSubscription("sub_newer_live", env.Customer, PlanWing, "year", later.Unix())
	// A canceled row with the latest period end is not live and is never chosen.
	seedStripeSub(t, env.DB, env.User, "sub_dead_latest", env.Customer, "canceled", latest)
	// A live code row has no Stripe subscription to cancel in the Portal.
	seedLiveCodeSub(t, env.DB, env.User)

	w := postPortal(env.Svc, env.User, `{"flow":"subscription_cancel"}`)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.NotEmpty(t, portalURL(t, w))
	req := onlyPortalSession(t, env.Fake)
	assert.Equal(t, flowSubscriptionCancel, req.Form.Get("flow_data[type]"))
	assert.Equal(t, "sub_newer_live", req.Form.Get("flow_data[subscription_cancel][subscription]"),
		"the live own Stripe row with the latest current_period_end")
	assert.Equal(t, env.Customer, req.Form.Get("customer"))
	assert.Equal(t, portalV1ConfigID, req.Form.Get("configuration"))
	assert.Equal(t, ReturnURLPortal, req.Form.Get("return_url"))
}

func TestPortal_CancelFlowWithOnlyCodeRow404(t *testing.T) {
	env := newPortalEnv(t)
	seedLiveCodeSub(t, env.DB, env.User)

	w := postPortal(env.Svc, env.User, `{"flow":"subscription_cancel"}`)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, errNoBillingAccount, portalErrorClass(t, w))
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, portalSessionsPath))
}

// RF4: the Customer outlives its subscription, and so does the Portal.
func TestPortal_EndedSubscription(t *testing.T) {
	env := newPortalEnv(t)
	seedStripeSub(t, env.DB, env.User, "sub_ended", env.Customer, "canceled", time.Unix(testPeriodEnd, 0).UTC())

	t.Run("subscription_cancel has nothing to cancel", func(t *testing.T) {
		w := postPortal(env.Svc, env.User, `{"flow":"subscription_cancel"}`)

		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Equal(t, errNoBillingAccount, portalErrorClass(t, w))
		assert.Empty(t, env.Fake.requestsTo(http.MethodPost, portalSessionsPath), "no session is created for a dead subscription")
	})

	t.Run("no flow still opens the Portal for invoices", func(t *testing.T) {
		w := postPortal(env.Svc, env.User, `{}`)

		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.NotEmpty(t, portalURL(t, w))
		req := onlyPortalSession(t, env.Fake)
		assert.Equal(t, env.Customer, req.Form.Get("customer"))
		assert.False(t, hasFlowData(req), "no flow_data: %v", req.Form)
	})
}

func TestPortal_UnknownFlow400(t *testing.T) {
	cases := []struct{ name, body string }{
		{"unknown name", `{"flow":"bogus"}`},
		{"a Portal flow Concord does not offer", `{"flow":"subscription_update"}`},
		{"case differs", `{"flow":"PAYMENT_METHOD_UPDATE"}`},
		{"not a string", `{"flow":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPortalEnv(t)

			w := postPortal(env.Svc, env.User, tc.body)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, errInvalidRequest, portalErrorClass(t, w))
			assert.Zero(t, stripeRequestCount(env.Fake), "a rejected request makes no Stripe call")
		})
	}
}

func TestPortal_InvalidJSON400(t *testing.T) {
	cases := []struct{ name, body string }{
		{"truncated", `{"flow":`},
		{"not JSON", `not json`},
		{"an array", `[]`},
		{"a bare string", `"payment_method_update"`},
		{"valid document then trailing garbage", `{"flow":"payment_method_update"} trailing`},
		{"two documents", `{} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPortalEnv(t)

			w := postPortal(env.Svc, env.User, tc.body)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, errInvalidRequest, portalErrorClass(t, w))
			assert.Zero(t, stripeRequestCount(env.Fake), "a rejected request makes no Stripe call")
		})
	}
}

func TestPortal_BodyOverCap413(t *testing.T) {
	cases := []struct{ name, body string }{
		{"valid JSON over the cap", `{"flow":"payment_method_update","pad":"` + strings.Repeat("x", portalBodyCap) + `"}`},
		{"valid document then an oversized suffix", `{}` + strings.Repeat(" ", portalBodyCap)},
		{"oversized and not JSON: 413 before 400", strings.Repeat("x", portalBodyCap+1)},
		{"one byte over the cap", `{}` + strings.Repeat(" ", portalBodyCap-2+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPortalEnv(t)

			w := postPortal(env.Svc, env.User, tc.body)

			assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			assert.NotEmpty(t, portalErrorClass(t, w))
			assert.Zero(t, stripeRequestCount(env.Fake), "an oversized request makes no Stripe call")
		})
	}
}

func TestPortal_BodyAtCapIsAccepted(t *testing.T) {
	env := newPortalEnv(t)
	body := `{}` + strings.Repeat(" ", portalBodyCap-2)
	require.Len(t, body, portalBodyCap)

	w := postPortal(env.Svc, env.User, body)

	assert.Equal(t, http.StatusCreated, w.Code, "exactly 4 KiB is within the cap: %s", w.Body.String())
}

// The customer and the return URL are the server's to choose (spec §11): a
// body that names its own is either ignored or refused, never obeyed.
func TestPortal_RequestCannotSteerCustomerOrReturnURL(t *testing.T) {
	env := newPortalEnv(t)

	w := postPortal(env.Svc, env.User,
		`{"customer":"cus_evil","return_url":"https://evil.test/","configuration":"bpc_evil"}`)

	// Unknown fields are ignored, so the request succeeds with the server's own
	// choices; a 400 here would leave the assertions below with nothing to check.
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	req := onlyPortalSession(t, env.Fake)
	assert.Equal(t, env.Customer, req.Form.Get("customer"))
	assert.Equal(t, ReturnURLPortal, req.Form.Get("return_url"))
	assert.Equal(t, portalV1ConfigID, req.Form.Get("configuration"))
}

func TestPortal_ConfigCachedTenMinutes(t *testing.T) {
	env := newPortalEnv(t)
	clock := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	env.Svc.now = func() time.Time { return clock }
	listCount := func() int { return len(env.Fake.requestsTo(http.MethodGet, portalConfigsPath)) }

	require.Equal(t, http.StatusCreated, postPortal(env.Svc, env.User, `{}`).Code)
	require.Equal(t, http.StatusCreated, postPortal(env.Svc, env.User, `{}`).Code)
	assert.Equal(t, 1, listCount(), "two calls at one instant list the configurations once")

	clock = clock.Add(10*time.Minute - time.Second)
	require.Equal(t, http.StatusCreated, postPortal(env.Svc, env.User, `{}`).Code)
	assert.Equal(t, 1, listCount(), "9m59s later the cached id still stands")

	clock = clock.Add(2 * time.Second) // now 10m01s after the first list
	require.Equal(t, http.StatusCreated, postPortal(env.Svc, env.User, `{}`).Code)
	assert.Equal(t, 2, listCount(), "past ten minutes the configurations are listed again")

	require.Equal(t, http.StatusCreated, postPortal(env.Svc, env.User, `{}`).Code)
	assert.Equal(t, 2, listCount(), "the refreshed id is cached for a new ten minutes")

	for _, req := range env.Fake.requestsTo(http.MethodPost, portalSessionsPath) {
		assert.Equal(t, portalV1ConfigID, req.Form.Get("configuration"))
	}
}

func TestPortal_NoOwnedConfig503(t *testing.T) {
	fake := newFakeStripe(t)
	fake.seedPortal("bpc_dashboard_default", nil) // someone else's: no concord_portal marker
	fake.seedPortal("bpc_other_version", map[string]string{PortalMetadataKey: "v2"})
	env := newTestService(t, fake)
	user := newPayer(t, env, "cus_noconfig")

	w := postPortal(env.Svc, user, `{}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
	assert.Empty(t, fake.requestsTo(http.MethodPost, portalSessionsPath),
		"a Dashboard-default configuration is never used: it is not ours to rely on")
	assert.Equal(t, "cus_noconfig", mappedCustomer(t, env.DB, user), "the mapping is untouched")
	requireLogLine(t, env, "ERROR",
		"billing: no active Customer Portal configuration is marked for Concord; run the Stripe bootstrap",
		"marker_key="+PortalMetadataKey, "marker_value="+PortalMetadataVersion)
}

func TestPortal_ConfigListErrorIs503(t *testing.T) {
	env := newPortalEnv(t)
	env.Fake.failOn(http.MethodGet, portalConfigsPath, http.StatusInternalServerError, serverErrorBody)

	w := postPortal(env.Svc, env.User, `{}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
	assert.Empty(t, env.Fake.requestsTo(http.MethodPost, portalSessionsPath))
	assert.Equal(t, env.Customer, mappedCustomer(t, env.DB, env.User))
	requireLogLine(t, env.testEnv, "ERROR", "billing: Customer Portal configuration lookup failed",
		"error_class=api_error", "stage=list_portal_configurations")
}

// A failed lookup is not remembered: once the operator bootstraps, the next
// visit works without waiting for a cache to lapse.
func TestPortal_MissingConfigIsNotCached(t *testing.T) {
	fake := newFakeStripe(t)
	env := newTestService(t, fake)
	user := newPayer(t, env, "cus_late_config")
	clock := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	env.Svc.now = func() time.Time { return clock }

	require.Equal(t, http.StatusServiceUnavailable, postPortal(env.Svc, user, `{}`).Code)

	fake.seedPortal(portalV1ConfigID, map[string]string{PortalMetadataKey: PortalMetadataVersion})
	clock = clock.Add(time.Second)
	w := postPortal(env.Svc, user, `{}`)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, portalV1ConfigID, onlyPortalSession(t, fake).Form.Get("configuration"))
}

// missingCustomerBody is the session create refusal for the mapped Customer.
const missingCustomerBody = `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such customer: 'cus_portal'","param":"customer"}}`

func TestPortal_StaleCustomerDropsMappingAnd404(t *testing.T) {
	env := newPortalEnv(t)
	bystander := newPayer(t, env.testEnv, "cus_bystander")
	env.Fake.seedDeletedCustomer(env.Customer)
	env.Fake.failOn(http.MethodPost, portalSessionsPath, http.StatusNotFound, missingCustomerBody)

	w := postPortal(env.Svc, env.User, `{}`)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, errNoBillingAccount, portalErrorClass(t, w))
	assert.Equal(t, "", mappedCustomer(t, env.DB, env.User), "the stale mapping is deleted")
	assert.Equal(t, "cus_bystander", mappedCustomer(t, env.DB, bystander), "only the stale id is deleted")
	assert.Len(t, env.Fake.requestsTo(http.MethodGet, customersPath+env.Customer), 1,
		"the deletion was confirmed by a retrieve before the mapping went")
	for _, req := range env.Fake.writes() {
		assert.Equal(t, portalSessionsPath, req.Path, "a Portal visit never creates or deletes a Customer: %s %s", req.Method, req.Path)
	}
}

// B2: resource_missing on the Customer is what a key from another Stripe
// account gets for a live one. Dropping the mapping then would leave account
// erasure nothing to enqueue, so it goes only when a retrieve confirms
// deleted: true; anything else keeps it and answers as a Stripe failure.
func TestPortal_MissingCustomerWithoutDeletedProofKeepsMapping(t *testing.T) {
	const unverifiableMsg = "billing: Stripe reports the mapped Customer missing but cannot confirm it was deleted; mapping kept. Check that the Stripe key belongs to the account holding the Customer"
	cases := []struct {
		name  string
		seed  func(env *portalEnv)
		msg   string
		attrs []string
	}{
		{
			name:  "a customer this key never held",
			seed:  func(*portalEnv) {}, // retrieve says resource_missing too
			msg:   unverifiableMsg,
			attrs: []string{"stripe_customer_id=cus_portal", "error_class=customer_unverifiable", "stage=retrieve_customer"},
		},
		{
			name:  "a customer that still exists",
			seed:  func(env *portalEnv) { env.Fake.seedCustomer(env.Customer) },
			msg:   unverifiableMsg,
			attrs: []string{"error_class=customer_unverifiable", "stage=retrieve_customer"},
		},
		{
			name: "a failed retrieve",
			seed: func(env *portalEnv) {
				env.Fake.seedDeletedCustomer(env.Customer)
				env.Fake.failOn(http.MethodGet, customersPath+env.Customer, http.StatusServiceUnavailable, serverErrorBody)
			},
			msg:   "billing: confirming a missing Customer failed; mapping kept",
			attrs: []string{"stripe_customer_id=cus_portal", "error_class=api_error", "stage=retrieve_customer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPortalEnv(t)
			tc.seed(env)
			env.Fake.failOn(http.MethodPost, portalSessionsPath, http.StatusNotFound, missingCustomerBody)

			w := postPortal(env.Svc, env.User, `{}`)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
			assert.Equal(t, env.Customer, mappedCustomer(t, env.DB, env.User), "the mapping is kept")
			assert.Len(t, env.Fake.requestsTo(http.MethodGet, customersPath+env.Customer), 1)
			requireLogLine(t, env.testEnv, "ERROR", tc.msg, tc.attrs...)
			assert.Empty(t, logLine(env.Logs.String(), "WARN", "billing: Stripe no longer has the mapped Customer; mapping removed"))
		})
	}
}

// A resource_missing that is not about the customer (say the cached
// configuration was archived in the Dashboard) must not cost the user a valid
// mapping.
func TestPortal_MissingConfigurationKeepsMapping(t *testing.T) {
	env := newPortalEnv(t)
	clock := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	env.Svc.now = func() time.Time { return clock } // the cache TTL never lapses here
	listCount := func() int { return len(env.Fake.requestsTo(http.MethodGet, portalConfigsPath)) }
	env.Fake.failOn(http.MethodPost, portalSessionsPath, http.StatusNotFound,
		`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such configuration: 'bpc_v1'","param":"configuration"}}`)

	w := postPortal(env.Svc, env.User, `{}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
	assert.Equal(t, env.Customer, mappedCustomer(t, env.DB, env.User), "the Customer is fine; the mapping stays")
	requireLogLine(t, env.testEnv, "ERROR", "billing: portal session create failed",
		"error_class=invalid_request_error", "stage=create_portal_session", "param=configuration")
	assert.Empty(t, env.Fake.requestsTo(http.MethodGet, customersPath+env.Customer),
		"a missing configuration is not a missing Customer")
	require.Equal(t, 1, listCount())

	// E3: the archived id was dropped from the cache, so the next visit lists
	// the configurations again instead of reusing it for ten minutes.
	env.Fake.mu.Lock()
	delete(env.Fake.failures, http.MethodPost+" "+portalSessionsPath)
	env.Fake.mu.Unlock()
	require.Equal(t, http.StatusCreated, postPortal(env.Svc, env.User, `{}`).Code)
	assert.Equal(t, 2, listCount(), "the cleared cache made the second visit list again")
}

func TestPortal_StripeErrorsAre503(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		class  string
		param  string // expected to appear in the error log
	}{
		{"server error", http.StatusInternalServerError, serverErrorBody, "api_error", ""},
		{"permission error", http.StatusForbidden, permissionErrorBody, "invalid_request_error", ""},
		{"invalid request names its param in the log", http.StatusBadRequest,
			`{"error":{"type":"invalid_request_error","message":"Invalid flow.","param":"flow_data[subscription_cancel][subscription]"}}`,
			"invalid_request_error", "flow_data[subscription_cancel][subscription]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPortalEnv(t)
			env.Fake.failOn(http.MethodPost, portalSessionsPath, tc.status, tc.body)

			w := postPortal(env.Svc, env.User, `{}`)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
			assert.Equal(t, env.Customer, mappedCustomer(t, env.DB, env.User), "only resource_missing on the customer drops a mapping")
			line := requireLogLine(t, env.testEnv, "ERROR", "billing: portal session create failed",
				"error_class="+tc.class, "stage=create_portal_session")
			if tc.param != "" {
				assert.Contains(t, line, tc.param, "an invalid_request_error log names the offending param")
			}
			assert.NotContains(t, env.Logs.String(), "Something went wrong", "Stripe's message is never logged")
		})
	}
}

// postPortalCtx is postPortal with a raw user_id value and a request context.
func postPortalCtx(ctx context.Context, svc *Service, userID, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/billing/portal-session", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID)
	svc.PortalSession(c)
	return w
}

// E4: the error branches the behavioural tests above do not reach.
func TestPortal_ErrorBranches(t *testing.T) {
	t.Run("no usable authenticated user id is a 401", func(t *testing.T) {
		env := newPortalEnv(t)

		w := postPortalCtx(context.Background(), env.Svc, "not-a-uuid", `{}`)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "unauthorized", portalErrorClass(t, w))
		assert.Zero(t, stripeRequestCount(env.Fake))
		requireLogLine(t, env.testEnv, "ERROR", "billing: portal request has no usable authenticated user id")
	})

	t.Run("a failed mapping lookup is a 503", func(t *testing.T) {
		env := newPortalEnv(t)
		svc := closedDBService(t, env.testEnv)

		w := postPortal(svc, env.User, `{}`)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
		assert.Zero(t, stripeRequestCount(env.Fake))
		requireLogLine(t, env.testEnv, "ERROR", "billing: portal mapping lookup failed", "stage=portal_mapping")
	})

	t.Run("a failed live-subscription lookup is a 503", func(t *testing.T) {
		env := newPortalEnv(t)
		// Hold subscriptions exclusively so the lookup blocks until the request
		// context ends; the mapping read on stripe_customers is not blocked.
		probe := openProbeDB(t)
		tx, err := probe.Begin()
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		_, err = tx.Exec(`LOCK TABLE subscriptions IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		w := postPortalCtx(ctx, env.Svc, env.User.String(), `{"flow":"subscription_cancel"}`)
		require.NoError(t, tx.Rollback())

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
		assert.Empty(t, env.Fake.requestsTo(http.MethodPost, portalSessionsPath))
		requireLogLine(t, env.testEnv, "ERROR", "billing: portal subscription lookup failed", "stage=portal_live_subscription")
	})

	t.Run("a session create that fails without a Stripe error is a 503", func(t *testing.T) {
		env := newPortalEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"object":"list","url":"/v1/billing_portal/configurations","has_more":false,"data":[{"id":"bpc_v1","object":"billing_portal.configuration","active":true,"metadata":{"` +
					PortalMetadataKey + `":"` + PortalMetadataVersion + `"}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":`)) // a 200 the SDK cannot decode
		}))
		t.Cleanup(srv.Close)
		env.Svc.Stripe = NewClient("sk_test_fake", srv.URL)

		w := postPortal(env.Svc, env.User, `{}`)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
		assert.Equal(t, env.Customer, mappedCustomer(t, env.DB, env.User))
		requireLogLine(t, env.testEnv, "ERROR", "billing: portal session create failed", "stage=create_portal_session")
	})

	t.Run("a failed stale-mapping delete is a 503 and keeps the mapping", func(t *testing.T) {
		env := newPortalEnv(t)
		env.Fake.seedDeletedCustomer(env.Customer)
		env.Fake.failOn(http.MethodPost, portalSessionsPath, http.StatusNotFound, missingCustomerBody)
		block := uniqueSQLName("test_block_mapping_delete")
		_, err := env.DB.Exec(`
			CREATE FUNCTION ` + block + `() RETURNS trigger LANGUAGE plpgsql AS
			$$ BEGIN RAISE EXCEPTION 'delete blocked by test'; END $$;
			CREATE TRIGGER ` + block + ` BEFORE DELETE ON stripe_customers
				FOR EACH ROW EXECUTE FUNCTION ` + block + `();`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, derr := env.DB.Exec(`DROP TRIGGER ` + block + ` ON stripe_customers; DROP FUNCTION ` + block + `()`)
			require.NoError(t, derr)
		})

		w := postPortal(env.Svc, env.User, `{}`)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, errBillingUnavail, portalErrorClass(t, w))
		assert.Equal(t, env.Customer, mappedCustomer(t, env.DB, env.User))
		requireLogLine(t, env.testEnv, "ERROR", "billing: dropping the stale Customer mapping failed",
			"error_class=db_P0001", "stage=drop_stale_mapping")
	})
}
