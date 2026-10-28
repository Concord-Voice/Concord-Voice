package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// None of these tests may call t.Parallel: they set env vars and package
// variables.

const (
	fakeTestKey = "sk_test_fake" // pragma: allowlist secret
	fakeLiveKey = "rk_live_fake" // pragma: allowlist secret
	devCustomer = `{"id":"cus_A","object":"customer","metadata":{"concord_user_id":"u1","concord_env":"development"}}`
	fixedNow    = 1_800_000_000
)

// stub answers GET /v1/customers/cus_A with customerBody and POST
// /v1/test_helpers/test_clocks with a clock, recording the clock's form.
type stub struct {
	mu           sync.Mutex
	customerBody string
	status       int // non-zero: every request answers this with an error body
	clockForm    url.Values
	requests     int
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Stripe-Should-Retry", "false")
	switch {
	case s.status != 0:
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided: sk_test_****fake"}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/customers/cus_A":
		_, _ = io.WriteString(w, s.customerBody)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/test_helpers/test_clocks":
		_ = r.ParseForm()
		s.clockForm = r.PostForm
		frozen, _ := strconv.ParseInt(r.PostForm.Get("frozen_time"), 10, 64)
		_, _ = fmt.Fprintf(w, `{"id":"clock_1","object":"test_helpers.test_clock","frozen_time":%d}`, frozen)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"unexpected"}}`)
	}
}

func setup(t *testing.T, key string, s *stub) {
	t.Helper()
	t.Setenv(keyEnv, key)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	stripeAPIBaseURL = srv.URL
	now = func() time.Time { return time.Unix(fixedNow, 0) }
	t.Cleanup(func() { stripeAPIBaseURL = ""; now = time.Now })
}

func run(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := runMain(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestAttachClockHappyPath(t *testing.T) {
	s := &stub{customerBody: devCustomer}
	setup(t, fakeTestKey, s)
	code, out, errOut := run("attach-clock", "--customer", "cus_A")
	require.Equal(t, 0, code, errOut)
	assert.Contains(t, out, "attached cus_A to test clock clock_1")
	assert.Equal(t, "cus_A", s.clockForm.Get("customer"))
	assert.Equal(t, "1800000060", s.clockForm.Get("frozen_time"), "default is now + clockLead")
}

func TestAttachClockFrozenTimeFlag(t *testing.T) {
	s := &stub{customerBody: devCustomer}
	setup(t, fakeTestKey, s)
	at := time.Unix(fixedNow, 0).Add(48 * time.Hour).UTC().Format(time.RFC3339)
	code, _, errOut := run("attach-clock", "--customer", "cus_A", "--frozen-time", at)
	require.Equal(t, 0, code, errOut)
	assert.Equal(t, "1800172800", s.clockForm.Get("frozen_time"))
}

func TestAttachClockUsageErrors(t *testing.T) {
	past := time.Unix(fixedNow, 0).Add(-time.Hour).UTC().Format(time.RFC3339)
	for name, args := range map[string][]string{
		"no subcommand":      nil,
		"unknown subcommand": {"advance"},
		"no customer":        {"attach-clock"},
		"not a customer id":  {"attach-clock", "--customer", "sub_1"},
		"extra argument":     {"attach-clock", "--customer", "cus_A", "x"},
		"bad frozen time":    {"attach-clock", "--customer", "cus_A", "--frozen-time", "tomorrow"},
		"past frozen time":   {"attach-clock", "--customer", "cus_A", "--frozen-time", past},
		"unknown flag":       {"attach-clock", "--bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &stub{customerBody: devCustomer}
			setup(t, fakeTestKey, s)
			code, _, _ := run(args...)
			assert.Equal(t, 2, code)
			assert.Zero(t, s.requests)
		})
	}
}

func TestAttachClockRefusesNonTestKey(t *testing.T) {
	for name, key := range map[string]string{"live": fakeLiveKey, "missing": "", "garbage": "nope"} {
		t.Run(name, func(t *testing.T) {
			s := &stub{customerBody: devCustomer}
			setup(t, key, s)
			code, _, errOut := run("attach-clock", "--customer", "cus_A")
			assert.Equal(t, 2, code)
			assert.Contains(t, errOut, "test-mode key")
			assert.Zero(t, s.requests)
		})
	}
}

func TestAttachClockRefusesCustomer(t *testing.T) {
	for name, body := range map[string]string{
		"deleted":         `{"id":"cus_A","object":"customer","deleted":true}`,
		"already clocked": `{"id":"cus_A","object":"customer","test_clock":"clock_0","metadata":{"concord_user_id":"u1","concord_env":"development"}}`,
		"not concord":     `{"id":"cus_A","object":"customer","metadata":{}}`,
		"other env":       `{"id":"cus_A","object":"customer","metadata":{"concord_user_id":"u1","concord_env":"staging"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := &stub{customerBody: body}
			setup(t, fakeTestKey, s)
			code, _, errOut := run("attach-clock", "--customer", "cus_A")
			assert.Equal(t, 1, code)
			assert.Contains(t, errOut, "refused")
			assert.Nil(t, s.clockForm, "no clock is created")
		})
	}
}

func TestAttachClockKeyRejectedHidesStripeText(t *testing.T) {
	s := &stub{status: http.StatusUnauthorized}
	setup(t, fakeTestKey, s)
	code, _, errOut := run("attach-clock", "--customer", "cus_A")
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "Stripe rejected STRIPE_SECRET_KEY (HTTP 401)")
	assert.False(t, strings.Contains(errOut, "fake"), "Stripe's text quotes the key")
}

func TestAttachClockStripeError(t *testing.T) {
	s := &stub{status: http.StatusBadRequest}
	setup(t, fakeTestKey, s)
	code, _, errOut := run("attach-clock", "--customer", "cus_A")
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "attach-clock: retrieve customer:")
}
