package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/health"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// jsonField/jsonElem are local stand-ins for testhelpers.JSONField/JSONElem
// (comma-ok, never a single-return type assertion -- #2811). internal/api
// cannot import internal/testhelpers here: testhelpers/testserver.go imports
// internal/api, and this file is an in-package (`package api`) test, so that
// import would be a cycle.
func jsonField[T any](t *testing.T, m map[string]interface{}, key string) T {
	t.Helper()
	raw, present := m[key]
	require.True(t, present, "field %q absent from object %v", key, m)
	tv, ok := raw.(T)
	require.True(t, ok, "field %q: got %T (%v), want %T", key, raw, raw, tv)
	return tv
}

func jsonElem[T any](t *testing.T, s []interface{}, i int) T {
	t.Helper()
	require.True(t, i >= 0 && i < len(s), "index %d out of range (len %d)", i, len(s))
	tv, ok := s[i].(T)
	require.True(t, ok, "index %d: got %T (%v), want %T", i, s[i], s[i], tv)
	return tv
}

func init() {
	gin.SetMode(gin.TestMode)
}

// doReadyz drives ReadyzHandler through a minimal router, exactly as
// TestHealthEndpoint drives healthHandler -- registration is exercised the
// same way production routing does, not by calling the handler func directly.
func doReadyz(t *testing.T, p *health.Prober) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	handler := ReadyzHandler(p)
	router.GET("/readyz", handler)
	router.HEAD("/readyz", handler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	router.ServeHTTP(w, req)
	return w
}

// proberWith builds a Prober over the given checks and publishes exactly one
// verdict synchronously via ProbeNow -- no goroutine, no ticker, no timing
// dependency. staleAfter is generous (one hour) so the published verdict
// stays fresh for the lifetime of the test.
func proberWith(t *testing.T, draining bool, checks ...health.Check) *health.Prober {
	t.Helper()
	r := health.NewReadiness()
	if draining {
		r.MarkDraining()
	}
	p := health.NewProber(r, checks, time.Hour, time.Hour, time.Now)
	p.ProbeNow(context.Background())
	return p
}

// staleProber returns a Prober that has never published a verdict, so
// Current() reports not-fresh -- the same condition a genuinely stuck
// goroutine would leave behind.
func staleProber() *health.Prober {
	return health.NewProber(health.NewReadiness(), nil, time.Hour, time.Hour, time.Now)
}

func decodeReadyzBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

func requireCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	require.Equal(t, want, w.Code)
}

func requireStatus(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	body := decodeReadyzBody(t, w)
	require.Equal(t, want, jsonField[string](t, body, "status"))
}

// requireCheck asserts one named check's reported status and gating bit,
// proving the check ACTUALLY RAN (rather than silently vanishing from the
// body) alongside whatever redaction assertion the caller also makes.
func requireCheck(t *testing.T, w *httptest.ResponseRecorder, name string, wantUp, wantGating bool) {
	t.Helper()
	body := decodeReadyzBody(t, w)
	checks := jsonField[[]interface{}](t, body, "checks")
	for i := range checks {
		c := jsonElem[map[string]interface{}](t, checks, i)
		if jsonField[string](t, c, "name") != name {
			continue
		}
		wantStatus := "down"
		if wantUp {
			wantStatus = "up"
		}
		require.Equal(t, wantStatus, jsonField[string](t, c, "status"), "check %s status", name)
		require.Equal(t, wantGating, jsonField[bool](t, c, "gating"), "check %s gating", name)
		return
	}
	t.Fatalf("check %q not present in /readyz body: %v", name, body)
}

var (
	ok           = func(context.Context) error { return nil }
	errSaturated = errors.New("saturated")
)

func TestReadyzStatusMatrix(t *testing.T) {
	// (draining x pg x redis x nats) -> exactly one documented status code.
	// The combinatorial pin makes NATS's non-gating a deliberate, visible
	// decision rather than an accident of handler ordering.
	for _, tc := range []struct {
		name                          string
		draining, pg, redisUp, natsUp bool
		code                          int
		status                        string
	}{
		{"all up", false, true, true, true, http.StatusOK, "ready"},
		{"nats down does not gate", false, true, true, false, http.StatusOK, "ready"},
		{"postgres down gates", false, false, true, true, http.StatusServiceUnavailable, "not_ready"},
		{"redis down gates", false, true, false, true, http.StatusServiceUnavailable, "not_ready"},
		{"draining", true, true, true, true, http.StatusServiceUnavailable, "draining"},
		{"draining and pg down", true, false, true, true, http.StatusServiceUnavailable, "draining"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boolCheck := func(name string, up bool) health.Check {
				probe := ok
				if !up {
					probe = func(context.Context) error { return errSaturated }
				}
				return health.Check{Name: name, Gating: true, Probe: probe}
			}
			natsCheck := health.Check{Name: "nats", Gating: false, Probe: func(context.Context) error {
				if tc.natsUp {
					return nil
				}
				return errSaturated
			}}
			p := proberWith(t, tc.draining, boolCheck("postgres", tc.pg), boolCheck("redis", tc.redisUp), natsCheck)
			w := doReadyz(t, p)
			requireCode(t, w, tc.code)
			requireStatus(t, w, tc.status)
			requireCheck(t, w, "nats", tc.natsUp, false)
		})
	}
}

// Saturation must be reported and must NOT flip the verdict. hub.go:64-85
// documents 29-against-25 as DESIGNED over-subscription during deploy-time
// mass disconnect, so a saturation gate fires fleet-wide while the fleet rolls.
func TestReadyzPoolSaturationReportsButDoesNotGate(t *testing.T) {
	p := proberWith(t, false,
		health.Check{Name: "postgres", Gating: true, Probe: ok},
		health.Check{Name: "redis", Gating: true, Probe: ok},
		health.Check{Name: "pool", Gating: false, Probe: func(context.Context) error { return errSaturated }},
	)
	w := doReadyz(t, p)
	requireCode(t, w, http.StatusOK)
	requireCheck(t, w, "pool", false, false) // down, non-gating
}

func TestReadyzFailsClosedOnStaleVerdict(t *testing.T) {
	w := doReadyz(t, staleProber()) // Current() returns ok == false
	requireCode(t, w, http.StatusServiceUnavailable)
	requireStatus(t, w, "probe_stale")
}

func TestReadyzNeverEchoesProbeError(t *testing.T) {
	leak := errors.New("dial tcp 172.19.0.3:5432: connect: connection refused")
	p := proberWith(t, false, health.Check{Name: "postgres", Gating: true,
		Probe: func(context.Context) error { return leak }})
	w := doReadyz(t, p)
	requireCode(t, w, http.StatusServiceUnavailable)
	requireCheck(t, w, "postgres", false, true) // asserts the check ACTUALLY ran
	for _, f := range []string{"172.19.0.3", "5432", "dial", "refused", "connect"} {
		require.NotContains(t, w.Body.String(), f)
	}
}

func TestReadyzDrainingStillReportsChecks(t *testing.T) {
	p := proberWith(t, true, health.Check{Name: "postgres", Gating: true, Probe: ok})
	w := doReadyz(t, p)
	requireCode(t, w, http.StatusServiceUnavailable)
	requireStatus(t, w, "draining")
	requireCheck(t, w, "postgres", true, true)
}

func TestReadyzNilProberFailsClosed(t *testing.T) {
	w := doReadyz(t, nil)
	requireCode(t, w, http.StatusServiceUnavailable)
	requireStatus(t, w, "probe_stale")
}

// TestReadyzReportsDrainImmediatelyAtProductionInterval is the regression lock
// on VULN-001.
//
// The handler used to read Verdict.Draining — the drain bit CACHED by the last
// probe tick. With the production 5s interval, and srv.Shutdown closing the
// listener within microseconds of the latch, that tick never lands, so /readyz
// never once reported draining and the shutdown window the whole issue exists
// to create was not delivered.
//
// Two properties are pinned deliberately:
//  1. readyzProbeInterval / readyzStaleAfter — the PRODUCTION constants, not
//     a fast test value. An earlier test used a 1ms interval, which is 5000x
//     faster than production and made the bug invisible.
//  2. NO polling and NO sleep. The assertion runs on the very next request
//     after MarkDraining, which is the actual guarantee: the drain is visible
//     immediately, not eventually.
func TestReadyzReportsDrainImmediatelyAtProductionInterval(t *testing.T) {
	readiness := health.NewReadiness()
	prober := health.NewProber(readiness, []health.Check{
		{Name: "postgres", Gating: true, Probe: func(context.Context) error { return nil }},
		{Name: "redis", Gating: true, Probe: func(context.Context) error { return nil }},
	}, readyzProbeInterval, readyzStaleAfter, time.Now)

	// One probe, then NEVER again for readyzProbeInterval (5s). Everything
	// below happens inside that window, exactly as a real SIGTERM does.
	ctx, cancel := context.WithCancel(context.Background())
	go prober.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, fresh := prober.Current(); fresh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("precondition: the prober must publish one verdict")
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // no further ticks — the cached verdict is now frozen as "not draining"

	if code := doReadyzCode(t, prober); code != http.StatusOK {
		t.Fatalf("precondition: healthy prober must be 200, got %d", code)
	}

	readiness.MarkDraining()

	// The VERY NEXT request. No sleep, no retry, no waiting for a tick.
	w := doReadyz(t, prober)
	requireCode(t, w, http.StatusServiceUnavailable)
	requireStatus(t, w, "draining")
}

func doReadyzCode(t *testing.T, p *health.Prober) int {
	t.Helper()
	return doReadyz(t, p).Code
}
