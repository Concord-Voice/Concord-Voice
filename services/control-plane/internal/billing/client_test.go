package billing

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

func TestBackendConfig_EmptyBaseURLKeepsStripeDefault(t *testing.T) {
	t.Parallel()
	cfg := backendConfig("")
	assert.Nil(t, cfg.URL, "an empty base URL must leave the SDK default, https://api.stripe.com")
	require.NotNil(t, cfg.EnableTelemetry)
	assert.False(t, *cfg.EnableTelemetry)
	// LeveledLogger is an interface{} field, so a non-nil check alone would
	// accept a debug-level logger. Pin the type and the level.
	logger, ok := cfg.LeveledLogger.(*stripe.LeveledLogger)
	require.True(t, ok, "LeveledLogger is %T, want *stripe.LeveledLogger", cfg.LeveledLogger)
	assert.Equal(t, stripe.LevelNull, logger.Level, "the SDK logger must be silenced")
}

func TestBackendConfig_BaseURLOverride(t *testing.T) {
	t.Parallel()
	cfg := backendConfig("http://127.0.0.1:1")
	require.NotNil(t, cfg.URL)
	assert.Equal(t, "http://127.0.0.1:1", *cfg.URL)
	require.NotNil(t, cfg.EnableTelemetry)
	assert.False(t, *cfg.EnableTelemetry)
}

// requireBackendURL fails the test unless b is the SDK's own backend aimed at
// want. It reads the BUILT client, not backendConfig, so it proves what a
// request would actually reach. Call it before sending anything: if a backend
// still pointed at the SDK default, the next request would leave the machine
// for api.stripe.com.
func requireBackendURL(t *testing.T, name string, b stripe.Backend, want string) {
	t.Helper()
	be, ok := b.(*stripe.BackendImplementation)
	require.True(t, ok, "%s backend is %T, want *stripe.BackendImplementation", name, b)
	require.Equal(t, want, be.URL, "%s backend must point at the stub, not the Stripe default", name)
}

// TestNewClient_UsesBaseURLWithoutTelemetry sends two real requests, to a stub.
// With telemetry on, stripe-go v87 would write $XDG_CONFIG_HOME/stripe/telemetry_id
// on the first request and put telemetry_id in X-Stripe-Client-User-Agent. It
// queues the first request's metrics only when the response carries a
// Request-Id header (maybeEnqueueTelemetryMetrics returns early on an empty
// id), and sends them as X-Stripe-Client-Telemetry on the second request. The
// stub therefore sets Request-Id; without it the telemetry-header assertion
// below could never fail. The SDK reads XDG_CONFIG_HOME itself on every
// non-Windows OS, darwin included (telemetry_id.go getConfigDir; it never calls
// os.UserConfigDir), so the Setenv below redirects the write on the developer's
// Mac and in CI alike. TestMain also redirects it for the whole package, because
// the SDK writes the id and caches the user-agent only once per process.
func TestNewClient_UsesBaseURLWithoutTelemetry(t *testing.T) {
	require.NotEmpty(t, packageConfigHome, "TestMain must have redirected XDG_CONFIG_HOME")
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var mu sync.Mutex
	var paths []string
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_test")
		if _, err := w.Write([]byte(`{"id":"founders-50","object":"coupon","duration":"forever","percent_off":50,"valid":true}`)); err != nil {
			t.Errorf("write stub response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient("sk_test_fake", srv.URL)

	// Network guard, before any request: every backend the client can reach
	// must point at the stub. One service per backend. The coupon call below
	// uses only the API backend, so the other three prove no other route leads
	// to the Stripe default.
	requireBackendURL(t, "API", c.V1Coupons.B, srv.URL)
	requireBackendURL(t, "Connect", c.OAuth.B, srv.URL)
	requireBackendURL(t, "Uploads", c.V1Files.BUploads, srv.URL)
	requireBackendURL(t, "MeterEvents", c.V2BillingMeterEventStreams.BMeterEvents, srv.URL)

	for range 2 {
		cp, err := c.V1Coupons.Retrieve(t.Context(), CouponFoundersID, nil)
		require.NoError(t, err)
		assert.Equal(t, CouponFoundersID, cp.ID)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, headers, 2)
	assert.Equal(t, []string{"/v1/coupons/founders-50", "/v1/coupons/founders-50"}, paths)
	assert.Equal(t, "Bearer sk_test_fake", headers[0].Get("Authorization"))
	for i, h := range headers {
		assert.NotContains(t, h.Get("X-Stripe-Client-User-Agent"), "telemetry_id", "request %d", i)
		assert.Empty(t, h.Get("X-Stripe-Client-Telemetry"), "request %d", i)
	}
	for _, home := range []string{configHome, packageConfigHome} {
		_, statErr := os.Stat(filepath.Join(home, "stripe", "telemetry_id"))
		assert.ErrorIs(t, statErr, os.ErrNotExist, "telemetry must never write an id file under %s", home)
	}
}
