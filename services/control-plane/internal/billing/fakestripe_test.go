package billing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v87"
)

const (
	resourceMissingBody = `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such object"}}`
	serverErrorBody     = `{"error":{"type":"api_error","message":"Something went wrong on Stripe's end."}}`
	permissionErrorBody = `{"error":{"type":"invalid_request_error","message":"The provided key does not have the required permissions for this endpoint."}}`
	invalidRequestBody  = `{"error":{"type":"invalid_request_error","message":"This Checkout Session is not open and cannot be expired."}}`
	lookupKeyTakenBody  = `{"error":{"type":"invalid_request_error","message":"A price with this lookup_key already exists. Set transfer_lookup_key to true to move it."}}`
)

// fakeStripe is an in-memory stand-in for the four Stripe resources the
// bootstrap touches. It serializes requests, records each one so tests can
// assert exact form bodies and idempotency keys, and can fail one route.
type fakeStripe struct {
	t        *testing.T
	mu       sync.Mutex
	products []*fakeProduct
	prices   []*fakePrice
	coupons  map[string]*fakeCoupon
	portals  []*fakePortal
	// Runtime resources (PR-2): what the webhook, Reconcile, the outbox and
	// the Portal endpoint read and write.
	subscriptions []*fakeSubscription
	schedules     map[string]*fakeSchedule
	customers     map[string]bool // customer ids that exist
	// deletedCustomers are ids this account once held and deleted: Stripe
	// still retrieves them, as {"deleted": true}.
	deletedCustomers map[string]bool
	sessions         []*fakeSession
	balanceTxns      []map[string]any // customer balance transactions, in creation order
	requests         []fakeRequest
	seq              map[string]int
	failures         map[string]fakeFailure // keyed by "METHOD /path"
	// replayed names create routes that answer as Stripe does when it replays
	// an Idempotency-Key whose object was since deleted: a "created" object
	// that nothing stores, so a later retrieve finds no such object.
	replayed map[string]bool
	// Checkout (PR-3): customerKeys maps an Idempotency-Key to the customer it
	// created, so a replayed create returns the same id, as Stripe does.
	// missingOnSession maps a customer to the resource_missing body its next
	// session create answers. onCustomerCreate, when set, runs
	// after a customer create is stored and before it is answered.
	customerKeys     map[string]string
	missingOnSession map[string]string
	onCustomerCreate func()
	// onSessionCreate, when set, runs after a session create is stored.
	onSessionCreate func()
}

type fakeRequest struct {
	Method         string
	Path           string
	Form           url.Values
	IdempotencyKey string
}

type fakeFailure struct {
	status int
	body   string
}

type fakeProduct struct {
	ID       string            `json:"id"`
	Object   string            `json:"object"`
	Active   bool              `json:"active"`
	Name     string            `json:"name"`
	Metadata map[string]string `json:"metadata"`
}

type fakeRecurring struct {
	Interval      string `json:"interval"`
	IntervalCount int64  `json:"interval_count"`
	UsageType     string `json:"usage_type"`
}

type fakePrice struct {
	ID         string            `json:"id"`
	Object     string            `json:"object"`
	Active     bool              `json:"active"`
	Currency   string            `json:"currency"`
	LookupKey  string            `json:"lookup_key"`
	Metadata   map[string]string `json:"metadata"`
	Product    string            `json:"product"`
	Recurring  fakeRecurring     `json:"recurring"`
	Type       string            `json:"type"`
	UnitAmount int64             `json:"unit_amount"`
}

type fakeCouponAppliesTo struct {
	Products []string `json:"products"`
}

type fakeCoupon struct {
	ID             string               `json:"id"`
	Object         string               `json:"object"`
	AppliesTo      *fakeCouponAppliesTo `json:"applies_to,omitempty"`
	Duration       string               `json:"duration"`
	MaxRedemptions int64                `json:"max_redemptions"`
	PercentOff     float64              `json:"percent_off"`
	RedeemBy       int64                `json:"redeem_by"`
	Valid          bool                 `json:"valid"`
}

type fakePortal struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	Active    bool              `json:"active"`
	IsDefault bool              `json:"is_default"`
	Metadata  map[string]string `json:"metadata"`
}

// fakeSubscription is the wire shape of a Stripe Subscription as the runtime
// code reads it: items carry an expanded price, customer is always an id, and
// discounts are ids unless the request expands them (see retrieveSubscription).
type fakeSubscription struct {
	ID                        string                           `json:"id"`
	Object                    string                           `json:"object"`
	Status                    string                           `json:"status"`
	Customer                  string                           `json:"customer"`
	CancelAt                  int64                            `json:"cancel_at"`
	CancelAtPeriodEnd         bool                             `json:"cancel_at_period_end"`
	Metadata                  map[string]string                `json:"metadata"`
	Items                     fakeSubItems                     `json:"items"`
	Discounts                 []any                            `json:"discounts"`
	Schedule                  string                           `json:"schedule,omitempty"`
	AutomaticTax              *stripe.SubscriptionAutomaticTax `json:"automatic_tax,omitempty"`
	LatestInvoice             any                              `json:"latest_invoice,omitempty"`
	PendingUpdate             map[string]any                   `json:"pending_update,omitempty"`
	couponIDs                 []string                         // test-side source of Discounts
	pendingPaymentStatus      string                           // test-side failed payment status
	pendingPriorPaymentStatus string                           // earlier invoice attempt
	pendingInvoiceURL         string                           // optional test-side hosted invoice override
	pendingPriceID            string                           // test-side pending update target
	pendingMetadata           map[string]string                // test-side metadata awaiting payment
}

type fakeSubItems struct {
	Object string        `json:"object"`
	Data   []fakeSubItem `json:"data"`
}

type fakeSchedule struct {
	ID           string            `json:"id"`
	Object       string            `json:"object"`
	Status       string            `json:"status"`
	Metadata     map[string]string `json:"metadata"`
	CurrentPhase struct {
		StartDate int64 `json:"start_date"`
		EndDate   int64 `json:"end_date"`
	} `json:"current_phase"`
}

type fakeSubItem struct {
	ID               string     `json:"id"`
	Object           string     `json:"object"`
	CurrentPeriodEnd int64      `json:"current_period_end"`
	Quantity         int64      `json:"quantity"`
	Price            *fakePrice `json:"price"`
}

type fakeDiscount struct {
	ID     string             `json:"id"`
	Object string             `json:"object"`
	Source fakeDiscountSource `json:"source"`
}

type fakeDiscountSource struct {
	Type   string     `json:"type"`
	Coupon fakeCoupon `json:"coupon"`
}

type fakeSession struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Customer  string `json:"customer"`
	Status    string `json:"status"`
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

type fakePortalSession struct {
	ID            string         `json:"id"`
	Object        string         `json:"object"`
	Configuration string         `json:"configuration"`
	Customer      string         `json:"customer"`
	ReturnURL     string         `json:"return_url"`
	URL           string         `json:"url"`
	Flow          map[string]any `json:"flow,omitempty"`
}

type fakeDeleted struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

type fakeList struct {
	Object  string `json:"object"`
	URL     string `json:"url"`
	HasMore bool   `json:"has_more"`
	Data    any    `json:"data"`
}

// seededCatalog points at the objects seedCatalog created, so a test can
// break exactly one of them.
type seededCatalog struct {
	supersonic *fakeProduct
	wing       *fakeProduct
	prices     map[string]*fakePrice // by lookup_key
	coupon     *fakeCoupon
	portal     *fakePortal
}

func newFakeStripe(t *testing.T) *fakeStripe {
	t.Helper()
	return &fakeStripe{
		t:         t,
		products:  []*fakeProduct{},
		prices:    []*fakePrice{},
		coupons:   map[string]*fakeCoupon{},
		portals:   []*fakePortal{},
		customers: map[string]bool{},
		schedules: map[string]*fakeSchedule{},
		seq:       map[string]int{},
		failures:  map[string]fakeFailure{},
		replayed:  map[string]bool{},

		deletedCustomers: map[string]bool{},
		customerKeys:     map[string]string{},
		missingOnSession: map[string]string{},
	}
}

// replayDeletedOn makes every create on path answer like a replay of a
// deleted object.
func (f *fakeStripe) replayDeletedOn(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replayed[path] = true
}

// client serves f over httptest and returns a billing client aimed at it.
func (f *fakeStripe) client() *stripe.Client {
	srv := httptest.NewServer(f.handler())
	f.t.Cleanup(srv.Close)
	c := NewClient("sk_test_fake", srv.URL)
	// Network guard, before any request: every backend the client can reach
	// must point at the stand-in, so no test (or later mutant) can reach
	// api.stripe.com. Same four backends Task 11's client_test.go checks.
	requireBackendURL(f.t, "API", c.V1Coupons.B, srv.URL)
	requireBackendURL(f.t, "Connect", c.OAuth.B, srv.URL)
	requireBackendURL(f.t, "Uploads", c.V1Files.BUploads, srv.URL)
	requireBackendURL(f.t, "MeterEvents", c.V2BillingMeterEventStreams.BMeterEvents, srv.URL)
	return c
}

func (f *fakeStripe) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/products", f.listProducts)
	mux.HandleFunc("POST /v1/products", f.createProduct)
	mux.HandleFunc("GET /v1/products/{id}", f.retrieveProduct)
	mux.HandleFunc("GET /v1/prices", f.listPrices)
	mux.HandleFunc("POST /v1/prices", f.createPrice)
	mux.HandleFunc("GET /v1/coupons/{id}", f.retrieveCoupon)
	mux.HandleFunc("POST /v1/coupons", f.createCoupon)
	mux.HandleFunc("GET /v1/billing_portal/configurations", f.listPortals)
	mux.HandleFunc("POST /v1/billing_portal/configurations", f.createPortal)
	mux.HandleFunc("POST /v1/billing_portal/configurations/{id}", f.updatePortal)
	mux.HandleFunc("GET /v1/subscriptions/{id}", f.retrieveSubscription)
	mux.HandleFunc("POST /v1/subscriptions/{id}", f.updateSubscription)
	mux.HandleFunc("POST /v1/invoices/create_preview", f.createInvoicePreview)
	mux.HandleFunc("GET /v1/invoices/{id}", f.retrieveInvoice)
	mux.HandleFunc("POST /v1/subscription_schedules", f.createSchedule)
	mux.HandleFunc("GET /v1/subscription_schedules/{id}", f.retrieveSchedule)
	mux.HandleFunc("POST /v1/subscription_schedules/{id}", f.updateSchedule)
	mux.HandleFunc("POST /v1/subscription_schedules/{id}/release", f.releaseSchedule)
	mux.HandleFunc("GET /v1/subscriptions", f.listSubscriptions)
	mux.HandleFunc("DELETE /v1/customers/{id}", f.deleteCustomer)
	mux.HandleFunc("GET /v1/customers/{id}", f.retrieveCustomer)
	mux.HandleFunc("GET /v1/checkout/sessions", f.listSessions)
	mux.HandleFunc("POST /v1/checkout/sessions/{id}/expire", f.expireSession)
	mux.HandleFunc("POST /v1/billing_portal/sessions", f.createPortalSession)
	mux.HandleFunc("POST /v1/customers", f.createCustomer)
	mux.HandleFunc("POST /v1/checkout/sessions", f.createSession)
	mux.HandleFunc("POST /v1/customers/{id}/balance_transactions", f.createBalanceTransaction)
	mux.HandleFunc("GET /v1/customers/{id}/balance_transactions", f.listBalanceTransactions)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := r.ParseForm(); err != nil {
			f.t.Errorf("fake stripe: parse form: %v", err)
			f.writeRaw(w, http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"bad form"}}`)
			return
		}
		f.requests = append(f.requests, fakeRequest{
			Method: r.Method, Path: r.URL.Path, Form: r.Form, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		})
		if failure, ok := f.failures[r.Method+" "+r.URL.Path]; ok {
			f.writeRaw(w, failure.status, failure.body)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// failOn makes every request to method+path answer status with body.
func (f *fakeStripe) failOn(method, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method+" "+path] = fakeFailure{status: status, body: body}
}

// requestsTo returns every request to method+path, in arrival order.
func (f *fakeStripe) requestsTo(method, path string) []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRequest
	for _, req := range f.requests {
		if req.Method == method && req.Path == path {
			out = append(out, req)
		}
	}
	return out
}

// writes returns every non-GET request, in arrival order.
func (f *fakeStripe) writes() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeRequest, 0, len(f.requests))
	for _, req := range f.requests {
		if req.Method != http.MethodGet {
			out = append(out, req)
		}
	}
	return out
}

func (f *fakeStripe) nextID(prefix string) string {
	f.seq[prefix]++
	return fmt.Sprintf("%s_%d", prefix, f.seq[prefix])
}

func (f *fakeStripe) seedProduct(id, plan string) *fakeProduct {
	md := map[string]string{}
	if plan != "" {
		md[ProductMetadataKey] = plan
	}
	p := &fakeProduct{ID: id, Object: "product", Active: true, Name: id, Metadata: md}
	f.products = append(f.products, p)
	return p
}

func (f *fakeStripe) seedPrice(id, lookupKey, plan, productID string, amount int64, interval string) *fakePrice {
	p := &fakePrice{
		ID: id, Object: "price", Active: true, Currency: "usd", LookupKey: lookupKey,
		Metadata: map[string]string{PlanMetadataKey: plan}, Product: productID,
		Recurring: fakeRecurring{Interval: interval, IntervalCount: 1, UsageType: "licensed"}, Type: "recurring", UnitAmount: amount,
	}
	f.prices = append(f.prices, p)
	return p
}

func (f *fakeStripe) seedCoupon() *fakeCoupon {
	c := &fakeCoupon{ID: CouponFoundersID, Object: "coupon", Duration: "forever", PercentOff: 50, Valid: true}
	f.coupons[c.ID] = c
	return c
}

func (f *fakeStripe) seedPortal(id string, metadata map[string]string) *fakePortal {
	pc := &fakePortal{ID: id, Object: "billing_portal.configuration", Active: true, Metadata: metadata}
	f.portals = append(f.portals, pc)
	return pc
}

// seedCustomer registers a customer id so DELETE /v1/customers/{id} finds it.
func (f *fakeStripe) seedCustomer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers[id] = true
}

// seedSubscription stores an active, quantity-1 subscription whose single item
// carries a price with concord_plan=plan and the given interval and period end
// (unix seconds). The returned pointer is the stored object: a test mutates it
// before the code under test runs (Status, Quantity, CancelAt, Items, Metadata).
// coupons are the coupon ids applied as discounts.
func (f *fakeStripe) seedSubscription(id, customerID, plan, interval string, periodEnd int64, coupons ...string) *fakeSubscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	amount := int64(899)
	if offer, ok := findOffer(plan, interval); ok {
		amount = offer.unitAmount
	}
	price := &fakePrice{
		ID: f.nextID("price"), Object: "price", Active: true, Currency: "usd",
		Metadata:  map[string]string{PlanMetadataKey: plan},
		Recurring: fakeRecurring{Interval: interval, IntervalCount: 1, UsageType: "licensed"},
		Type:      "recurring", UnitAmount: amount,
	}
	sub := &fakeSubscription{
		ID: id, Object: "subscription", Status: "active", Customer: customerID,
		Metadata: map[string]string{},
		Items: fakeSubItems{Object: "list", Data: []fakeSubItem{{
			ID: f.nextID("si"), Object: "subscription_item", CurrentPeriodEnd: periodEnd, Quantity: 1, Price: price,
		}}},
		couponIDs: coupons,
	}
	f.subscriptions = append(f.subscriptions, sub)
	f.customers[customerID] = true
	return sub
}

// seedSession stores a Checkout Session for customerID in the given status.
func (f *fakeStripe) seedSession(id, customerID, status string) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := &fakeSession{ID: id, Object: "checkout.session", Customer: customerID, Status: status, URL: "https://checkout.stripe.test/c/" + id}
	f.sessions = append(f.sessions, cs)
	return cs
}

// seedDeletedCustomer records a customer this account held and deleted, so a
// delete answers resource_missing and a retrieve answers deleted: true.
func (f *fakeStripe) seedDeletedCustomer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.customers, id)
	f.deletedCustomers[id] = true
}

// customerExists reports whether the fake still holds the customer.
func (f *fakeStripe) customerExists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.customers[id]
}

// sessionStatus returns the stored status of a Checkout Session, or "" if absent.
func (f *fakeStripe) sessionStatus(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cs := range f.sessions {
		if cs.ID == id {
			return cs.Status
		}
	}
	return ""
}

// seedCatalog provisions the complete, valid catalog with fixed IDs.
func (f *fakeStripe) seedCatalog() seededCatalog {
	s := seededCatalog{
		supersonic: f.seedProduct("prod_S", PlanSupersonic),
		wing:       f.seedProduct("prod_W", PlanWing),
		prices: map[string]*fakePrice{
			LookupKeySupersonicMonthly: f.seedPrice("price_SM", LookupKeySupersonicMonthly, PlanSupersonic, "prod_S", 899, "month"),
			LookupKeySupersonicYearly:  f.seedPrice("price_SY", LookupKeySupersonicYearly, PlanSupersonic, "prod_S", 9889, "year"),
			LookupKeyWingMonthly:       f.seedPrice("price_WM", LookupKeyWingMonthly, PlanWing, "prod_W", 1499, "month"),
			LookupKeyWingYearly:        f.seedPrice("price_WY", LookupKeyWingYearly, PlanWing, "prod_W", 16489, "year"),
		},
		coupon: f.seedCoupon(),
		portal: f.seedPortal("bpc_concord", map[string]string{PortalMetadataKey: PortalMetadataVersion}),
	}
	return s
}

func (f *fakeStripe) listProducts(w http.ResponseWriter, r *http.Request) {
	f.writeJSON(w, fakeList{Object: "list", URL: r.URL.Path, Data: f.products})
}

func (f *fakeStripe) createProduct(w http.ResponseWriter, r *http.Request) {
	p := &fakeProduct{
		ID: f.nextID("prod"), Object: "product", Active: true,
		Name: r.PostForm.Get("name"), Metadata: formMetadata(r.PostForm),
	}
	if !f.replayed[r.URL.Path] {
		f.products = append(f.products, p)
	}
	f.writeJSON(w, p)
}

func (f *fakeStripe) retrieveProduct(w http.ResponseWriter, r *http.Request) {
	for _, p := range f.products {
		if p.ID == r.PathValue("id") {
			f.writeJSON(w, p)
			return
		}
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

// listPrices mirrors Stripe's price list: lookup_keys filters, and active
// defaults to true, so an archived price is listed only when the request says
// active=false. A bootstrap that never asks cannot see an archived price.
func (f *fakeStripe) listPrices(w http.ResponseWriter, r *http.Request) {
	keys := map[string]bool{}
	for k, v := range r.Form {
		if strings.HasPrefix(k, "lookup_keys[") && len(v) > 0 {
			keys[v[0]] = true
		}
	}
	wantActive := r.Form.Get("active") != "false"
	out := make([]*fakePrice, 0, len(f.prices))
	for _, p := range f.prices {
		if keys[p.LookupKey] && p.Active == wantActive {
			out = append(out, p)
		}
	}
	f.writeJSON(w, fakeList{Object: "list", URL: r.URL.Path, Data: out})
}

// createPrice refuses a lookup_key that any stored price holds, archived or not,
// unless the request sets transfer_lookup_key=true, which moves the key off its
// holder atomically. The bootstrap never sends it, so an archived price it
// failed to see surfaces as this refusal instead of a quiet second price.
func (f *fakeStripe) createPrice(w http.ResponseWriter, r *http.Request) {
	amount, err := strconv.ParseInt(r.PostForm.Get("unit_amount"), 10, 64)
	if err != nil {
		f.writeRaw(w, http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"bad unit_amount"}}`)
		return
	}
	lookupKey := r.PostForm.Get("lookup_key")
	for _, held := range f.prices {
		if lookupKey == "" || held.LookupKey != lookupKey {
			continue
		}
		if r.PostForm.Get("transfer_lookup_key") != "true" {
			f.writeRaw(w, http.StatusBadRequest, lookupKeyTakenBody)
			return
		}
		held.LookupKey = ""
	}
	p := &fakePrice{
		ID: f.nextID("price"), Object: "price", Active: true,
		Currency: r.PostForm.Get("currency"), LookupKey: r.PostForm.Get("lookup_key"),
		Metadata: formMetadata(r.PostForm), Product: r.PostForm.Get("product"),
		Recurring: fakeRecurring{Interval: r.PostForm.Get("recurring[interval]"), IntervalCount: 1, UsageType: "licensed"},
		Type:      "recurring", UnitAmount: amount,
	}
	f.prices = append(f.prices, p)
	f.writeJSON(w, p)
}

// retrieveCoupon mirrors Stripe: applies_to is expandable and not included by
// default, so it is returned only when the request carries expand for it.
func (f *fakeStripe) retrieveCoupon(w http.ResponseWriter, r *http.Request) {
	c, ok := f.coupons[r.PathValue("id")]
	if !ok {
		f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
		return
	}
	shown := *c
	if !requestsExpand(r.Form, "applies_to") {
		shown.AppliesTo = nil
	}
	f.writeJSON(w, &shown)
}

// requestsExpand reports whether form asks to expand field, in whichever
// indexed or bare form the SDK encodes an expand list.
func requestsExpand(form url.Values, field string) bool {
	for k, values := range form {
		if strings.HasPrefix(k, "expand[") && slices.Contains(values, field) {
			return true
		}
	}
	return false
}

func (f *fakeStripe) createCoupon(w http.ResponseWriter, r *http.Request) {
	pct, err := strconv.ParseFloat(r.PostForm.Get("percent_off"), 64)
	if err != nil {
		f.writeRaw(w, http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"bad percent_off"}}`)
		return
	}
	c := &fakeCoupon{
		ID: r.PostForm.Get("id"), Object: "coupon", Duration: r.PostForm.Get("duration"), PercentOff: pct, Valid: true,
	}
	if !f.replayed[r.URL.Path] {
		f.coupons[c.ID] = c
	}
	f.writeJSON(w, c)
}

func (f *fakeStripe) listPortals(w http.ResponseWriter, r *http.Request) {
	f.writeJSON(w, fakeList{Object: "list", URL: r.URL.Path, Data: f.portals})
}

func (f *fakeStripe) createPortal(w http.ResponseWriter, r *http.Request) {
	pc := &fakePortal{
		ID: f.nextID("bpc"), Object: "billing_portal.configuration", Active: true, Metadata: formMetadata(r.PostForm),
	}
	f.portals = append(f.portals, pc)
	f.writeJSON(w, pc)
}

func (f *fakeStripe) updatePortal(w http.ResponseWriter, r *http.Request) {
	for _, pc := range f.portals {
		if pc.ID == r.PathValue("id") {
			f.writeJSON(w, pc)
			return
		}
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

// retrieveSubscription mirrors Stripe: customer is an id (the code under test
// must never expand it), items.data[].price is a full object, and discounts are
// bare ids unless the request carries expand[]=discounts.
func (f *fakeStripe) retrieveSubscription(w http.ResponseWriter, r *http.Request) {
	for _, sub := range f.subscriptions {
		if sub.ID == r.PathValue("id") {
			shown := f.renderSubscription(sub, requestsExpand(r.Form, "discounts"))
			if sub.LatestInvoice != nil && requestsExpand(r.Form, "latest_invoice") {
				shown.LatestInvoice = map[string]any{"id": "in_" + sub.ID, "object": "invoice"}
			}
			f.writeJSON(w, shown)
			return
		}
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

func (f *fakeStripe) updateSubscription(w http.ResponseWriter, r *http.Request) {
	for _, sub := range f.subscriptions {
		if sub.ID != r.PathValue("id") {
			continue
		}
		if sub.Schedule != "" {
			f.writeRaw(w, http.StatusBadRequest, invalidRequestBody)
			return
		}
		if r.Form.Get("cancel_at_period_end") == "true" {
			sub.CancelAtPeriodEnd = true
			f.writeJSON(w, f.renderSubscription(sub, requestsExpand(r.Form, "discounts")))
			return
		}
		priceID := r.Form.Get("items[0][price]")
		if priceID == "" {
			f.applySubscriptionMetadata(sub, r.Form)
			f.writeJSON(w, f.renderSubscription(sub, requestsExpand(r.Form, "discounts")))
			return
		}
		for _, price := range f.prices {
			if price.ID != priceID {
				continue
			}
			if r.Form.Get("payment_behavior") == "pending_if_incomplete" && sub.pendingPaymentStatus != "" {
				sub.PendingUpdate = map[string]any{"expires_at": int64(1900000000)}
				sub.LatestInvoice = "in_" + sub.ID
				sub.pendingPriceID = priceID
				sub.pendingMetadata = map[string]string{}
				for key, values := range r.Form {
					if strings.HasPrefix(key, "metadata[") && strings.HasSuffix(key, "]") && len(values) > 0 {
						sub.pendingMetadata[strings.TrimSuffix(strings.TrimPrefix(key, "metadata["), "]")] = values[0]
					}
				}
				f.writeJSON(w, f.renderSubscription(sub, requestsExpand(r.Form, "discounts")))
				return
			}
			sub.Items.Data[0].Price = price
			f.applySubscriptionMetadata(sub, r.Form)
			f.writeJSON(w, f.renderSubscription(sub, requestsExpand(r.Form, "discounts")))
			return
		}
		f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
		return
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

func (f *fakeStripe) applySubscriptionMetadata(sub *fakeSubscription, form url.Values) {
	for key, values := range form {
		if strings.HasPrefix(key, "metadata[") && strings.HasSuffix(key, "]") && len(values) > 0 {
			name := strings.TrimSuffix(strings.TrimPrefix(key, "metadata["), "]")
			if values[0] == "" {
				delete(sub.Metadata, name)
			} else {
				sub.Metadata[name] = values[0]
			}
		}
	}
}

func (f *fakeStripe) createInvoicePreview(w http.ResponseWriter, r *http.Request) {
	if r.Form.Get("subscription") == "" || r.Form.Get("subscription_details[items][0][price]") == "" {
		f.writeRaw(w, http.StatusBadRequest, invalidRequestBody)
		return
	}
	f.writeJSON(w, map[string]any{"object": "invoice", "currency": "usd", "amount_due": int64(500)})
}

func (f *fakeStripe) retrieveInvoice(w http.ResponseWriter, r *http.Request) {
	for _, sub := range f.subscriptions {
		if "in_"+sub.ID != r.PathValue("id") || sub.PendingUpdate == nil {
			continue
		}
		invoiceURL := sub.pendingInvoiceURL
		if invoiceURL == "" {
			invoiceURL = "https://invoice.stripe.com/i/test_" + sub.ID
		}
		payments := []any{}
		if sub.pendingPriorPaymentStatus != "" {
			payments = append(payments, map[string]any{
				"object": "invoice_payment", "payment": map[string]any{"type": "payment_intent", "payment_intent": map[string]any{
					"id": "pi_prior_" + sub.ID, "object": "payment_intent", "status": sub.pendingPriorPaymentStatus,
				}},
			})
		}
		payments = append(payments, map[string]any{
			"object": "invoice_payment", "payment": map[string]any{"type": "payment_intent", "payment_intent": map[string]any{
				"id": "pi_" + sub.ID, "object": "payment_intent", "status": sub.pendingPaymentStatus,
			}},
		})
		f.writeJSON(w, map[string]any{
			"id": "in_" + sub.ID, "object": "invoice", "hosted_invoice_url": invoiceURL,
			"payments": map[string]any{"object": "list", "data": payments},
		})
		return
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

func (f *fakeStripe) createSchedule(w http.ResponseWriter, r *http.Request) {
	for _, sub := range f.subscriptions {
		if sub.ID != r.Form.Get("from_subscription") {
			continue
		}
		if sub.Schedule != "" {
			f.writeRaw(w, http.StatusBadRequest, invalidRequestBody)
			return
		}
		schedule := &fakeSchedule{ID: f.nextID("sub_sched"), Object: "subscription_schedule", Status: "active", Metadata: map[string]string{}}
		schedule.CurrentPhase.StartDate = sub.Items.Data[0].CurrentPeriodEnd - 30*24*3600
		schedule.CurrentPhase.EndDate = sub.Items.Data[0].CurrentPeriodEnd
		f.schedules[schedule.ID] = schedule
		sub.Schedule = schedule.ID
		f.writeJSON(w, schedule)
		return
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

func (f *fakeStripe) retrieveSchedule(w http.ResponseWriter, r *http.Request) {
	if schedule := f.schedules[r.PathValue("id")]; schedule != nil {
		f.writeJSON(w, schedule)
		return
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

func (f *fakeStripe) updateSchedule(w http.ResponseWriter, r *http.Request) {
	schedule := f.schedules[r.PathValue("id")]
	if schedule == nil || schedule.Status != "active" {
		f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
		return
	}
	for k, v := range formMetadata(r.Form) {
		schedule.Metadata[k] = v
	}
	f.writeJSON(w, schedule)
}

func (f *fakeStripe) releaseSchedule(w http.ResponseWriter, r *http.Request) {
	schedule := f.schedules[r.PathValue("id")]
	if schedule == nil || schedule.Status != "active" {
		f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
		return
	}
	schedule.Status = "released"
	for _, sub := range f.subscriptions {
		if sub.Schedule == schedule.ID {
			sub.Schedule = ""
		}
	}
	f.writeJSON(w, schedule)
}

func (f *fakeStripe) renderSubscription(sub *fakeSubscription, expandDiscounts bool) fakeSubscription {
	shown := *sub
	shown.Discounts = make([]any, 0, len(sub.couponIDs))
	for i, couponID := range sub.couponIDs {
		id := fmt.Sprintf("di_%s_%d", sub.ID, i)
		if !expandDiscounts {
			shown.Discounts = append(shown.Discounts, id)
			continue
		}
		shown.Discounts = append(shown.Discounts, fakeDiscount{
			ID: id, Object: "discount",
			Source: fakeDiscountSource{Type: "coupon", Coupon: fakeCoupon{ID: couponID, Object: "coupon", Valid: true}},
		})
	}
	return shown
}

// listSubscriptions filters by customer. Like Stripe it omits canceled
// subscriptions unless status=all (or status=canceled) is asked for.
func (f *fakeStripe) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	customer := r.Form.Get("customer")
	status := r.Form.Get("status")
	out := make([]fakeSubscription, 0, len(f.subscriptions))
	for _, sub := range f.subscriptions {
		if customer != "" && sub.Customer != customer {
			continue
		}
		if status == "" && sub.Status == "canceled" {
			continue
		}
		if status != "" && status != "all" && sub.Status != status {
			continue
		}
		out = append(out, f.renderSubscription(sub, requestsExpand(r.Form, "data.discounts")))
	}
	f.writeJSON(w, fakeList{Object: "list", URL: r.URL.Path, Data: out})
}

// deleteCustomer removes a seeded customer; a second delete is resource_missing.
func (f *fakeStripe) deleteCustomer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !f.customers[id] {
		f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
		return
	}
	delete(f.customers, id)
	f.deletedCustomers[id] = true
	f.writeJSON(w, fakeDeleted{ID: id, Object: "customer", Deleted: true})
}

// retrieveCustomer mirrors Stripe: a live customer is returned without a
// deleted field, a deleted one as {"deleted": true}, and an id this account
// never held (or one of another account) is resource_missing.
func (f *fakeStripe) retrieveCustomer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch {
	case f.customers[id]:
		f.writeJSON(w, map[string]any{"id": id, "object": "customer"})
	case f.deletedCustomers[id]:
		f.writeJSON(w, fakeDeleted{ID: id, Object: "customer", Deleted: true})
	default:
		f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
	}
}

// createBalanceTransaction answers a customer balance credit (code credit, D1).
func (f *fakeStripe) createBalanceTransaction(w http.ResponseWriter, r *http.Request) {
	amount, _ := strconv.ParseInt(r.Form.Get("amount"), 10, 64)
	f.writeJSON(w, f.addBalanceTxn(r.PathValue("id"), amount, r.Form.Get("currency"),
		map[string]string{"concord_redemption_id": r.Form.Get("metadata[concord_redemption_id]")}))
}

// addBalanceTxn stores a balance transaction; the caller holds f.mu.
func (f *fakeStripe) addBalanceTxn(customer string, amount int64, currency string, metadata map[string]string) map[string]any {
	txn := map[string]any{
		"id": f.nextID("cbtxn"), "object": "customer_balance_transaction",
		"customer": customer, "amount": amount, "currency": currency, "metadata": metadata,
	}
	f.balanceTxns = append(f.balanceTxns, txn)
	return txn
}

// seedBalanceTxn stores a credit the account already holds for redemptionID.
func (f *fakeStripe) seedBalanceTxn(customer, redemptionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addBalanceTxn(customer, -2697, "usd", map[string]string{"concord_redemption_id": redemptionID})
}

func (f *fakeStripe) listBalanceTransactions(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, txn := range f.balanceTxns {
		if txn["customer"] == r.PathValue("id") {
			out = append(out, txn)
		}
	}
	f.writeJSON(w, map[string]any{"object": "list", "url": r.URL.Path, "data": out})
}

func (f *fakeStripe) listSessions(w http.ResponseWriter, r *http.Request) {
	customer, status := r.Form.Get("customer"), r.Form.Get("status")
	out := make([]*fakeSession, 0, len(f.sessions))
	for _, cs := range f.sessions {
		if (customer == "" || cs.Customer == customer) && (status == "" || cs.Status == status) {
			out = append(out, cs)
		}
	}
	f.writeJSON(w, fakeList{Object: "list", URL: r.URL.Path, Data: out})
}

// expireSession expires an open session. Stripe refuses a session that is no
// longer open (for example one the customer completed first) with
// invalid_request_error and no code, which the erasure outbox must tolerate.
func (f *fakeStripe) expireSession(w http.ResponseWriter, r *http.Request) {
	for _, cs := range f.sessions {
		if cs.ID != r.PathValue("id") {
			continue
		}
		if cs.Status != "open" {
			f.writeRaw(w, http.StatusBadRequest, invalidRequestBody)
			return
		}
		cs.Status = "expired"
		f.writeJSON(w, cs)
		return
	}
	f.writeRaw(w, http.StatusNotFound, resourceMissingBody)
}

// createPortalSession echoes the configuration, customer, return_url and
// flow_data[type] it was given, so a test asserts what the endpoint sent.
func (f *fakeStripe) createPortalSession(w http.ResponseWriter, r *http.Request) {
	ps := &fakePortalSession{
		ID: f.nextID("bps"), Object: "billing_portal.session",
		Configuration: r.PostForm.Get("configuration"), Customer: r.PostForm.Get("customer"),
		ReturnURL: r.PostForm.Get("return_url"),
	}
	ps.URL = "https://billing.stripe.test/p/session/" + ps.ID
	if flowType := r.PostForm.Get("flow_data[type]"); flowType != "" {
		ps.Flow = map[string]any{"type": flowType}
	}
	f.writeJSON(w, ps)
}

func (f *fakeStripe) writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("fake stripe: marshal response: %v", err)
		f.writeRaw(w, http.StatusInternalServerError, serverErrorBody)
		return
	}
	f.writeRaw(w, http.StatusOK, string(body))
}

// writeRaw always sends Stripe-Should-Retry: false, so the SDK's retry loop
// never sleeps in these tests.
func (f *fakeStripe) writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Stripe-Should-Retry", "false")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		f.t.Errorf("fake stripe: write response: %v", err)
	}
}

func formMetadata(form url.Values) map[string]string {
	md := map[string]string{}
	for k, v := range form {
		if name, ok := strings.CutPrefix(k, "metadata["); ok && len(v) > 0 {
			md[strings.TrimSuffix(name, "]")] = v[0]
		}
	}
	return md
}
