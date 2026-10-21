package billing

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

const telemetryControlChildEnv = "CONCORD_TELEMETRY_CONTROL_CHILD"

// TestSDKDefaultTelemetry_PositiveControl proves the telemetry-off assertions
// in TestNewClient_UsesBaseURLWithoutTelemetry can fail. It builds a client the
// way NewClient does but leaves EnableTelemetry at the SDK default, against the
// same kind of stub, and requires the telemetry header and the user-agent
// telemetry_id to appear. If the SDK stopped sending them under that setup, or
// the stub stopped provoking them, the telemetry-off test would pass for the
// wrong reason, and this control fails instead.
//
// stripe-go computes its user-agent once per process, so a telemetry-on
// request here would poison the telemetry-off test. The control therefore runs
// in a child process of this test binary, with its own XDG_CONFIG_HOME.
func TestSDKDefaultTelemetry_PositiveControl(t *testing.T) {
	if os.Getenv(telemetryControlChildEnv) == "1" {
		runTelemetryControl(t)
		return
	}
	require.True(t, stripe.EnableTelemetry, "premise: the SDK default is telemetry ON, which is why NewClient must override it")

	//nolint:gosec // G204: re-executes this test binary (os.Args[0]) with fixed flags; no external input.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSDKDefaultTelemetry_PositiveControl$", "-test.v")
	cmd.Env = append(os.Environ(), telemetryControlChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the child control failed:\n%s", out)
	assert.Contains(t, string(out), "--- PASS: TestSDKDefaultTelemetry_PositiveControl")
}

func runTelemetryControl(t *testing.T) {
	var mu sync.Mutex
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_test")
		if _, err := w.Write([]byte(`{"id":"founders-50","object":"coupon","duration":"forever","percent_off":50,"valid":true}`)); err != nil {
			t.Errorf("write stub response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	// Same shape as backendConfig, minus the telemetry override.
	cfg := &stripe.BackendConfig{
		URL:           stripe.String(srv.URL),
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	}
	require.Nil(t, cfg.EnableTelemetry)
	c := stripe.NewClient("sk_test_fake", stripe.WithBackends(stripe.NewBackendsWithConfig(cfg)))
	for range 2 {
		_, err := c.V1Coupons.Retrieve(t.Context(), CouponFoundersID, nil)
		require.NoError(t, err)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, headers, 2)
	assert.Contains(t, headers[0].Get("X-Stripe-Client-User-Agent"), "telemetry_id",
		"with the SDK default, the first request carries a telemetry id")
	assert.NotEmpty(t, headers[1].Get("X-Stripe-Client-Telemetry"),
		"with the SDK default, the second request carries the first one's metrics")
}
