package billing

import (
	"net/http"
	"strconv"
)

// Checkout routes of the Stripe fake (#2033 PR-3): customer create and
// Checkout Session create. Both run under the fake's lock, like every route.

// customerMissingParamBody is Stripe's answer to a session create naming a
// customer it does not hold: resource_missing with param customer.
const customerMissingParamBody = `{"error":{"type":"invalid_request_error","code":"resource_missing","param":"customer","message":"No such customer: 'cus_old'"}}`

// customerMissingNoParamBody is the same refusal without a param, the shape
// the self-heal must also recognise by its message.
const customerMissingNoParamBody = `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such customer: 'cus_old'"}}`

// createCustomer stores a new customer. A request whose Idempotency-Key was
// already used gets the customer that key created, as Stripe replays it.
func (f *fakeStripe) createCustomer(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if id, ok := f.customerKeys[key]; ok && key != "" {
		f.writeJSON(w, map[string]any{"id": id, "object": "customer"})
		return
	}
	id := f.nextID("cus")
	f.customers[id] = true
	if key != "" {
		f.customerKeys[key] = id
	}
	if f.onCustomerCreate != nil {
		f.onCustomerCreate()
	}
	f.writeJSON(w, map[string]any{"id": id, "object": "customer"})
}

// createSession stores an open Checkout Session for the form's customer and
// echoes expires_at. A customer in missingOnSession is refused once with
// its resource_missing body, then forgotten.
func (f *fakeStripe) createSession(w http.ResponseWriter, r *http.Request) {
	customer := r.PostForm.Get("customer")
	if body, ok := f.missingOnSession[customer]; ok {
		delete(f.missingOnSession, customer)
		f.writeRaw(w, http.StatusNotFound, body)
		return
	}
	expiresAt, _ := strconv.ParseInt(r.PostForm.Get("expires_at"), 10, 64)
	id := f.nextID("cs")
	cs := &fakeSession{
		ID: id, Object: "checkout.session", Customer: customer, Status: "open",
		URL: "https://checkout.stripe.test/c/pay/" + id, ExpiresAt: expiresAt,
	}
	f.sessions = append(f.sessions, cs)
	if f.onSessionCreate != nil {
		f.onSessionCreate()
	}
	f.writeJSON(w, cs)
}

// refuseSessionFor makes the next session create for customer answer
// resource_missing on param customer.
func (f *fakeStripe) refuseSessionFor(customer string) {
	f.refuseSessionWith(customer, customerMissingParamBody)
}

// refuseSessionWith makes the next session create for customer answer 404
// with body.
func (f *fakeStripe) refuseSessionWith(customer, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missingOnSession[customer] = body
}

// setOnCustomerCreate installs a hook run inside the next customer creates.
func (f *fakeStripe) setOnCustomerCreate(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCustomerCreate = fn
}

// setOnSessionCreate installs a hook run inside the next session creates.
func (f *fakeStripe) setOnSessionCreate(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onSessionCreate = fn
}

// archivePrice marks the catalog price with lookupKey inactive.
func (f *fakeStripe) archivePrice(lookupKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prices {
		if p.LookupKey == lookupKey {
			p.Active = false
		}
	}
}
