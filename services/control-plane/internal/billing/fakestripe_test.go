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
	requests []fakeRequest
	seq      map[string]int
	failures map[string]fakeFailure // keyed by "METHOD /path"
	// replayed names create routes that answer as Stripe does when it replays
	// an Idempotency-Key whose object was since deleted: a "created" object
	// that nothing stores, so a later retrieve finds no such object.
	replayed map[string]bool
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
		t:        t,
		products: []*fakeProduct{},
		prices:   []*fakePrice{},
		coupons:  map[string]*fakeCoupon{},
		portals:  []*fakePortal{},
		seq:      map[string]int{},
		failures: map[string]fakeFailure{},
		replayed: map[string]bool{},
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
