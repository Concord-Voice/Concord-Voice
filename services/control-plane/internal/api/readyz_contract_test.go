package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/health"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A connector whose connections succeed instantly and do nothing. It exists to
// drive database/sql's POOL ACCOUNTING deterministically -- InUse and
// OpenConnections cannot be separated without holding real connections, and
// that distinction is the whole subject of the pool check.
type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{}, nil }
func (fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (*fakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*fakeConn) Close() error                        { return nil }
func (*fakeConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func poolProbe(t *testing.T, db *sql.DB) func(context.Context) error {
	t.Helper()
	for _, c := range newReadinessChecks(db, nil, nil) {
		if c.Name == "pool" {
			return c.Probe
		}
	}
	t.Fatal("no pool check")
	return nil
}

// TestNewReadinessChecksShape pins the production check set itself, which had
// NO coverage at all: every /readyz test builds synthetic health.Check literals
// and then asserts the gating bit it just set, which is tautological with
// respect to production. Flipping nats to gating passed the entire suite.
func TestNewReadinessChecksShape(t *testing.T) {
	checks := newReadinessChecks(nil, nil, nil)

	got := map[string]bool{}
	for _, c := range checks {
		require.NotNil(t, c.Probe, "%s has a nil probe, which fails closed forever", c.Name)
		got[c.Name] = c.Gating
	}
	require.Equal(t, map[string]bool{
		// Postgres and Redis gate: without them the service cannot serve.
		"postgres": true,
		"redis":    true,
		// NATS must NOT gate. RetryOnFailedConnect + MaxReconnects(-1) mean an
		// outage self-heals, so gating would 503 the only control-plane node on
		// every cold start where NATS lags Postgres.
		"nats": false,
		// Pool pressure must NOT gate: hub.go documents 29-against-25 as
		// DESIGNED over-subscription during deploy-time mass disconnect, so a
		// saturation gate would fire fleet-wide exactly while the fleet rolls.
		"pool": false,
	}, got)
}

// TestNewReadinessChecksNilHandlesFailClosed: a nil handle is a wiring bug.
// Before this it produced a nil dereference inside a background goroutine
// 0-5s later rather than a failed check.
func TestNewReadinessChecksNilHandlesFailClosed(t *testing.T) {
	rep := health.Evaluate(context.Background(), health.NewReadiness(), newReadinessChecks(nil, nil, nil))
	require.False(t, rep.Ready)
	// pool is included deliberately. Scoping this to postgres/redis is what let
	// a missing nil guard through: db.Stats() panicked before the probe could
	// return, Evaluate's recover caught it, and the check reported probe_panic
	// -- the wrong CAUSE for a wiring bug, on a check whose whole job is naming
	// causes.
	for _, r := range rep.Results {
		switch r.Name {
		case "postgres", "redis", "pool":
			assert.False(t, r.Up, "%s must fail closed on a nil handle", r.Name)
			assert.Equal(t, "handle_unwired", r.Class,
				"%s must report the wiring bug, not a panic", r.Name)
		}
	}
}

// TestPoolCheckMeasuresContentionNotWarmth is the regression lock for the two
// pool-predicate bugs, NEITHER of which had a test.
func TestPoolCheckMeasuresContentionNotWarmth(t *testing.T) {
	t.Run("unbounded pool is never saturated", func(t *testing.T) {
		// database/sql reports MaxOpenConnections 0 when SetMaxOpenConns was
		// never called, and 0 means UNBOUNDED. Without the `> 0` guard the
		// bare `InUse >= Max` is `0 >= 0` on an idle pool -- saturated forever,
		// from the first tick, on a pool with no limit at all.
		db := sql.OpenDB(fakeConnector{})
		defer func() { _ = db.Close() }()
		require.Zero(t, db.Stats().MaxOpenConnections)
		require.NoError(t, poolProbe(t, db)(context.Background()))
	})

	t.Run("warm but idle pool is not saturated", func(t *testing.T) {
		// THE InUse-vs-OpenConnections case. Open every slot, then return them
		// all: OpenConnections stays at the ceiling while InUse drops to zero.
		// Measuring OpenConnections reports a permanently saturated pool on any
		// pool that has simply been busy once -- which is every warm pool.
		db := sql.OpenDB(fakeConnector{})
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(2)
		db.SetMaxIdleConns(2)

		c1, err := db.Conn(context.Background())
		require.NoError(t, err)
		c2, err := db.Conn(context.Background())
		require.NoError(t, err)
		require.NoError(t, c1.Close())
		require.NoError(t, c2.Close())

		st := db.Stats()
		require.Equal(t, 2, st.OpenConnections, "precondition: every slot is open")
		require.Zero(t, st.InUse, "precondition: none is checked out")
		require.NoError(t, poolProbe(t, db)(context.Background()),
			"a warm, idle pool is not contended; only InUse says otherwise")
	})

	t.Run("fully checked-out pool is saturated", func(t *testing.T) {
		db := sql.OpenDB(fakeConnector{})
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(2)

		c1, err := db.Conn(context.Background())
		require.NoError(t, err)
		defer func() { _ = c1.Close() }()
		c2, err := db.Conn(context.Background())
		require.NoError(t, err)
		defer func() { _ = c2.Close() }()

		require.Equal(t, 2, db.Stats().InUse)
		require.ErrorIs(t, poolProbe(t, db)(context.Background()), errPoolSaturated)
	})
}

// TestReadyzProductionConstantsAreTheirValues pins the VALUES, not merely the
// coupling.
//
// The drain regression test references readyzProbeInterval symbolically, so
// lowering it to a millisecond left the whole suite green -- silently
// converting the VULN-001 lock back into the vacuous ancestor whose 1ms
// interval hid the bug in the first place. And readyzStaleAfter carries a
// documented MUST with no guard: below readyzProbeInterval + ProbeTimeout a
// healthy prober trips its own fence on ordinary jitter and 503s the fleet.
func TestReadyzProductionConstantsAreTheirValues(t *testing.T) {
	require.Equal(t, 5*time.Second, readyzProbeInterval)
	require.Equal(t, 20*time.Second, readyzStaleAfter)
	require.Greater(t, readyzStaleAfter, readyzProbeInterval+health.ProbeTimeout,
		"the staleness fence must exceed one full probe cycle or a healthy "+
			"prober trips it on scheduling jitter alone")
}

// TestReadyzStaleBodyKeepsTheEvidenceAndLabelsIt covers the AGED verdict -- the
// path no test reached, because the existing stale case uses a prober that
// never published and so returns at Current's FIRST early return.
//
// The handler used to build the checks slice and then discard it on this
// branch, dropping the last-known dependency state in exactly the case an
// operator needs it: concord-ctl.sh prints this body on a failed deploy.
func TestReadyzStaleBodyKeepsTheEvidenceAndLabelsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	clock := time.Now()
	p := health.NewProber(health.NewReadiness(), []health.Check{
		{Name: "postgres", Gating: true, Probe: func(context.Context) error { return nil }},
	}, time.Hour, readyzStaleAfter, func() time.Time { return clock })
	p.ProbeNow(context.Background())

	clock = clock.Add(readyzStaleAfter + time.Minute) // age it past the fence

	r := gin.New()
	r.GET("/readyz", ReadyzHandler(p))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "a stale verdict fails closed")
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "probe_stale", body["status"])
	require.Equal(t, true, body["checks_stale"], "stale evidence must be LABELLED, never passed off as current")
	require.NotEmpty(t, body["checks"], "the last-known state is the only diagnostic on this path")
	require.Positive(t, body["checks_age_seconds"], "an operator needs to know HOW stale")
}

// TestReadinessLogLevelSeparatesLossFromNonGatingFailure.
//
// A two-way loss/no-loss split under-reported the exact case the widened log
// edge exists to surface: a non-gating check going down leaves Ready unmoved,
// so `to` is still "ready" and the line went to Info -- level-indistinguishable
// from the per-request line emitted for every HTTP call. A permanently
// unconfigured NATS client is not routine traffic.
func TestReadinessLogLevelSeparatesLossFromNonGatingFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		kv   []any
		want string
	}{
		{"all up", []any{"from", "ready", "to", "ready", "failing", []string{}}, readinessLevelInfo},
		{"recovery edge", []any{"from", "not_ready", "to", "ready", "failing", []string{}}, readinessLevelInfo},
		{"readiness lost pages", []any{"from", "ready", "to", "not_ready", "failing", []string{"postgres=probe_timeout"}}, readinessLevelError},
		{"wedged probe pages", []any{"failure_class", health.ClassWedged, "budget_ms", int64(20000)}, readinessLevelError},
		// The SECOND value that reaches the same flag. The field was called
		// `wedged`, which read as though only the watchdog set it; onProbePanic
		// sets it too, and a reader trusting the name could route a future
		// class wrongly. Both are pinned so the name and the behaviour agree.
		{"panicking probe pages", []any{"failure_class", health.ClassPanic, "check", "postgres"}, readinessLevelError},
		// An unrecognised class must stay loud rather than fall through to Info.
		{"unknown failure class still pages", []any{"failure_class", "some_future_class", "to", "ready"}, readinessLevelError},
		// The finding. Ready is unchanged, so only the failing list distinguishes
		// this from steady state.
		{"non-gating down warns", []any{"from", "ready", "to", "ready", "failing", []string{"nats=nats_unconfigured"}}, readinessLevelWarn},
		{"pool saturated warns", []any{"from", "ready", "to", "ready", "failing", []string{"pool=pool_saturated"}}, readinessLevelWarn},
		// FAIL-CLOSED cases. Both are cross-package shape drift that used to
		// land on Info -- the one outcome that must never be reached by
		// accident, because nothing then alerts and nothing errors.
		{"unknown verdict word does not become Info", []any{"to", "quiescing", "failing", []string{}}, readinessLevelWarn},
		{"drifted failing type does not become Info", []any{"to", "ready", "failing", "postgres=probe_timeout"}, readinessLevelWarn},
		{"no verdict at all does not become Info", []any{"from", "ready"}, readinessLevelWarn},
		// A PLANNED drain is not a page. drainSettle keeps the prober ticking
		// through the drain, so a tick lands inside it on most shutdowns.
		{"planned drain does not page", []any{"to", "not_ready", "draining", true, "failing", []string{}}, readinessLevelInfo},
		// Negative controls for that exemption.
		{"loss with NO drain marker still pages", []any{"to", "not_ready", "failing", []string{}}, readinessLevelError},
		{"drain WITH a failing check warns", []any{"to", "not_ready", "draining", true, "failing", []string{"redis=probe_timeout"}}, readinessLevelWarn},
		{"wedge during a drain still pages", []any{"failure_class", health.ClassWedged, "to", "not_ready", "draining", true}, readinessLevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, readinessLogLevel(tc.kv))
		})
	}
}

// TestProberLogLinesRouteToTheIntendedLevel drives the REAL path: a live
// health.Prober emits, the adapter classifies.
//
// TestReadinessLogLevelSeparatesLossFromNonGatingFailure hand-builds its kv
// slices, so it pins the function against itself and is structurally blind to
// the two cross-package contracts this function actually depends on -- the
// verdict words produced by health.readyWord, and the []string returned by
// failingChecks. Either drifting used to downgrade severity to Info in silence,
// which is the one outcome that must never be reached by accident.
func TestProberLogLinesRouteToTheIntendedLevel(t *testing.T) {
	var pgUp atomic.Bool
	pgUp.Store(true)

	var mu sync.Mutex
	var levels []string
	capture := func(_ string, kv ...any) {
		mu.Lock()
		defer mu.Unlock()
		levels = append(levels, readinessLogLevel(kv))
	}

	p := health.NewProber(health.NewReadiness(), []health.Check{
		{Name: "postgres", Gating: true, Probe: func(context.Context) error {
			if pgUp.Load() {
				return nil
			}
			return errors.New("down")
		}},
		{Name: "nats", Gating: false, Probe: func(context.Context) error {
			return errNATSUnconfigured
		}},
	}, time.Hour, time.Hour, time.Now)
	p.SetLogger(capture)

	// First verdict: postgres up, nats (non-gating) down.
	p.ProbeNow(context.Background())
	// Readiness lost.
	pgUp.Store(false)
	p.ProbeNow(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, levels, 2)
	require.Equal(t, readinessLevelWarn, levels[0],
		"a non-gating check down while Ready holds must WARN, not blend into Info")
	require.Equal(t, readinessLevelError, levels[1],
		"a readiness LOSS must reach Error; if health's verdict word ever drifts "+
			"from the constant this adapter matches, that page silently disappears")
}

// TestRouteReadinessLineReachesTheRightSink closes the DOWNSTREAM half of the
// severity contract. readinessLogLevel's table proves classification; nothing
// proved that a level actually reaches log.Error rather than log.Info, so
// replacing the whole adapter with a bare log.Info reverted the production
// effect of an entire commit while the suite stayed green.
func TestRouteReadinessLineReachesTheRightSink(t *testing.T) {
	for _, tc := range []struct {
		name string
		kv   []any
		want string
	}{
		{"loss pages", []any{"to", health.WordNotReady, "failing", []string{"postgres=probe_timeout"}}, "error"},
		{"wedge pages", []any{"failure_class", health.ClassWedged}, "error"},
		{"non-gating down warns", []any{"to", health.WordReady, "failing", []string{"nats=nats_unconfigured"}}, "warn"},
		{"planned drain informs", []any{"to", health.WordNotReady, "draining", true, "failing", []string{}}, "info"},
		{"all up informs", []any{"to", health.WordReady, "failing", []string{}}, "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			sink := func(name string) func(string, ...any) {
				return func(string, ...any) { got = append(got, name) }
			}
			routeReadinessLine("readiness transition", tc.kv,
				sink("info"), sink("warn"), sink("error"))
			require.Equal(t, []string{tc.want}, got,
				"a %s-level line must reach exactly the %s sink", tc.want, tc.want)
		})
	}
}

// TestReadyzHeadCarriesTheVerdictInTheStatusCodeOnly covers the HEAD
// short-circuit, which had no test at all.
//
// HEAD is the shape a cheap external prober uses, and it discards the body by
// definition -- so the status code is not a summary of the verdict here, it is
// the ENTIRE verdict. A short-circuit that returned a constant 200 (or fell
// through to the ready branch) would be invisible to every GET-based test in
// this file while telling every HEAD prober the service is up during an
// outage. Both polarities are asserted for exactly that reason: a test that
// only checks the healthy case passes for a handler hardwired to 200.
func TestReadyzHeadCarriesTheVerdictInTheStatusCodeOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name  string
		probe func(context.Context) error
		want  int
	}{
		{"gating check up", func(context.Context) error { return nil }, http.StatusOK},
		{"gating check down", func(context.Context) error { return errors.New("down") }, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := health.NewProber(health.NewReadiness(), []health.Check{
				{Name: "postgres", Gating: true, Probe: tc.probe},
			}, time.Hour, readyzStaleAfter, time.Now)
			p.ProbeNow(context.Background())

			r := gin.New()
			r.GET("/readyz", ReadyzHandler(p))
			r.HEAD("/readyz", ReadyzHandler(p))

			get := httptest.NewRecorder()
			r.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			head := httptest.NewRecorder()
			r.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/readyz", nil))

			require.Equal(t, tc.want, get.Code, "GET verdict")
			require.Equal(t, get.Code, head.Code,
				"HEAD disagrees with GET: a prober using HEAD reads a different "+
					"readiness verdict than one using GET")
			require.Empty(t, head.Body.Bytes(),
				"HEAD must carry no body (RFC 7231); the short-circuit exists to "+
					"skip the marshal, not to have it discarded downstream")
		})
	}
}
