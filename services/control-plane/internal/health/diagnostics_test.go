package health

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeThatBlocksUntilCancelled returns a probe honouring its context, plus a
// channel closed when it is entered.
func probeThatBlocksUntilCancelled() (func(context.Context) error, chan struct{}) {
	entered := make(chan struct{})
	var once sync.Once
	return func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}, entered
}

// TestEvaluateBoundsEveryProbeUnderOneSharedDeadline is the falsifying test for
// ProbeTimeout, which had none: deleting the context.WithTimeout from Evaluate
// outright left the whole suite green.
//
// Without the bound a ctx-respecting probe never returns, Evaluate never
// returns, the prober's loop body blocks, single-flight drops every subsequent
// tick, and /readyz goes probe_stale fleet-wide. This test hangs in that case
// rather than passing.
//
// Three probes rather than one pin CONCURRENCY: total elapsed stays near one
// ProbeTimeout rather than three.
//
// It does NOT pin "one shared deadline rather than one each", and no test can:
// Evaluate starts every probe at the same instant, so N per-probe timeouts of
// ProbeTimeout each and one shared ProbeTimeout expire together and are
// externally indistinguishable. The single context is a structural property of
// the code, not an observable one -- do not add an assertion claiming to
// witness it, and do not read this test as having done so.
func TestEvaluateBoundsEveryProbeUnderOneSharedDeadline(t *testing.T) {
	checks := make([]Check, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		probe, _ := probeThatBlocksUntilCancelled()
		checks = append(checks, Check{Name: name, Gating: true, Probe: probe})
	}

	start := time.Now()
	rep := Evaluate(context.Background(), NewReadiness(), checks)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, ProbeTimeout,
		"returned BEFORE the deadline: the probes cannot have been awaited")
	require.Less(t, elapsed, 2*ProbeTimeout,
		"three probes took more than one budget, so they ran SEQUENTIALLY: a "+
			"slow dependency is spending the budget the next one needs, and "+
			"the checks behind it get misreported as timed out")

	require.False(t, rep.Ready, "a gating check that timed out must not be ready")
	for _, r := range rep.Results {
		assert.False(t, r.Up, "%s should be down", r.Name)
		assert.Equal(t, ClassTimeout, r.Class, "%s should classify as a timeout", r.Name)
	}
}

// TestEvaluateEmptyCheckSetIsNotReady pins the one input that used to fail
// OPEN. Everything else unknown in this package fails closed -- nil probe, nil
// prober, unpublished verdict, stale verdict -- but an empty check set
// reported ready, and it is the default for any future caller of this leaf.
func TestEvaluateEmptyCheckSetIsNotReady(t *testing.T) {
	rep := Evaluate(context.Background(), NewReadiness(), nil)
	require.False(t, rep.Ready, "no checks means 'I have been told nothing', not 'all fine'")
	require.Empty(t, rep.Results)
}

// TestEvaluateRecoversAPanickingProbe: without the recover this test does not
// fail, it CRASHES the test binary -- which is exactly the production
// behaviour it pins. A panic in a non-gating probe (pool, nats) would take the
// whole control-plane down with every live voice session, letting a check that
// is contractually forbidden from causing a 503 cause a process exit instead.
func TestEvaluateRecoversAPanickingProbe(t *testing.T) {
	rep := Evaluate(context.Background(), NewReadiness(), []Check{
		{Name: "boom", Gating: false, Probe: func(context.Context) error {
			panic("driver exploded at 10.0.0.5:5432 canary9f3a")
		}},
		{Name: "fine", Gating: true, Probe: func(context.Context) error { return nil }},
	})

	require.Len(t, rep.Results, 2)
	assert.False(t, rep.Results[0].Up)
	assert.Equal(t, ClassPanic, rep.Results[0].Class)
	assert.True(t, rep.Results[1].Up, "a sibling probe must still be evaluated")
	assert.True(t, rep.Ready, "a NON-gating panic must not move readiness")

	// The panic value is arbitrary and may wrap driver state, so it must not be
	// retained anywhere a sink could reach.
	require.NotNil(t, rep.Results[0].Err)
	assert.NotContains(t, rep.Results[0].Err.Error(), "canary9f3a")
	assert.NotContains(t, rep.Results[0].Err.Error(), "10.0.0.5")
}

// TestEvaluateClassifiesEveryFailureCause is the regression test for the defect
// where every cause was computed and discarded: an operator saw "postgres:
// down" and could not tell connection-refused from a probe timeout from a nil
// probe -- a WIRING bug reported as a dead database.
func TestEvaluateClassifiesEveryFailureCause(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name  string
		check Check
		ctx   context.Context
		want  string
	}{
		{"unwired", Check{Name: "unwired", Probe: nil}, context.Background(), ClassUnwired},
		{"generic", Check{Name: "generic", Probe: func(context.Context) error {
			return errors.New("connection refused")
		}}, context.Background(), ClassError},
		{"caller-supplied", Check{Name: "caller", Probe: func(context.Context) error {
			return &ClassifiedError{Class: "nats_unconfigured", Err: errors.New("nope")}
		}}, context.Background(), "nats_unconfigured"},
		{"cancelled", Check{Name: "cancelled", Probe: func(ctx context.Context) error {
			return ctx.Err()
		}}, cancelled, ClassCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Evaluate(tc.ctx, NewReadiness(), []Check{tc.check})
			require.Len(t, rep.Results, 1)
			assert.False(t, rep.Results[0].Up)
			assert.Equal(t, tc.want, rep.Results[0].Class)
		})
	}
}

// TestFailingChecksNeverCarryErrorText is the log-sink twin of
// TestReadyzNeverEchoesProbeError. The HTTP body had a redaction test; the log
// sink had none, so appending r.Err.Error() to the failing list passed the
// whole suite. lib/pq formats a dial failure with host and port; pgx adds user
// and database (CWE-532).
func TestFailingChecksNeverCarryErrorText(t *testing.T) {
	const dsn = "dial tcp 172.19.0.3:5432: connect: connection refused"
	rep := Evaluate(context.Background(), NewReadiness(), []Check{
		{Name: "postgres", Gating: true, Probe: func(context.Context) error {
			return errors.New(dsn)
		}},
	})

	got := failingChecks(rep)
	require.Equal(t, []string{"postgres=" + ClassError}, got,
		"the log list carries name=class only")
	for _, s := range got {
		assert.NotContains(t, s, "172.19.0.3")
		assert.NotContains(t, s, "5432")
		assert.False(t, strings.Contains(s, "connection refused"))
	}
}

// recordingLogger collects log lines for assertion.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
	kvs   [][]any
}

func (l *recordingLogger) log(msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
	l.kvs = append(l.kvs, kv)
}

func (l *recordingLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

func (l *recordingLogger) sawKV(key string, want any) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, kv := range l.kvs {
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i] == key && kv[i+1] == want {
				return true
			}
		}
	}
	return false
}

// TestProberLogsWhenOnlyANonGatingCheckFlips is the regression test for a
// defect that made both non-gating checks unobservable EVERYWHERE at once.
//
// The log edge was `prev.Ready != rep.Ready`, and Ready is moved only by a
// GATING check. So pool and nats -- declared non-gating precisely so they
// could be "alerting-only signal" -- could go down on tick 2 and stay down for
// the whole process lifetime while emitting nothing. Nothing polls the
// /readyz body in steady state either, so there was no sink and no consumer:
// the signal they exist to provide reached nobody.
func TestProberLogsWhenOnlyANonGatingCheckFlips(t *testing.T) {
	var poolUp atomic.Bool
	poolUp.Store(true)
	rec := &recordingLogger{}

	p := NewProber(NewReadiness(), []Check{
		{Name: "postgres", Gating: true, Probe: func(context.Context) error { return nil }},
		{Name: "pool", Gating: false, Probe: func(context.Context) error {
			if poolUp.Load() {
				return nil
			}
			return &ClassifiedError{Class: "pool_saturated", Err: errors.New("saturated")}
		}},
	}, time.Hour, time.Hour, time.Now)
	p.SetLogger(rec.log)

	p.ProbeNow(context.Background())
	require.Equal(t, 1, rec.count(), "the first verdict always logs")

	// Steady state must stay silent -- the edge-trigger is the point.
	p.ProbeNow(context.Background())
	require.Equal(t, 1, rec.count(), "an unchanged verdict must not log")

	poolUp.Store(false)
	p.ProbeNow(context.Background())
	require.Equal(t, 2, rec.count(),
		"a NON-gating check going down must log: readiness is unchanged, so a "+
			"Ready-only edge cannot see it, and nothing else observes this check")
	require.True(t, rec.sawKV("to", "ready"),
		"readiness itself is unchanged, so the line still reports ready")
}

// TestProberReportsAWedgedProbe pins the only signal that can ever explain a
// probe_stale.
//
// ProbeTimeout bounds the probes' CONTEXT, not Evaluate's return: a probe
// ignoring cancellation blocks Evaluate, blocks the loop body, and single-flight
// then drops every tick. The reader side fences that, but before this the
// writer side never noticed it had stopped probing -- so /readyz went stale
// with no log line naming which check hung, including on the deploy-failure
// path where concord-ctl.sh prints the body.
func TestProberReportsAWedgedProbe(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	rec := &recordingLogger{}

	p := NewProber(NewReadiness(), []Check{
		{Name: "postgres", Gating: true, Probe: func(context.Context) error {
			once.Do(func() { close(entered) })
			<-release // deliberately ignores ctx, like a driver that does
			return nil
		}},
	}, time.Hour, 40*time.Millisecond, time.Now)
	p.SetLogger(rec.log)

	go p.Start(context.Background())
	<-entered

	require.Eventually(t, func() bool { return rec.sawKV("failure_class", ClassWedged) },
		2*time.Second, 10*time.Millisecond,
		"a probe that outlives the staleness budget must be reported; otherwise "+
			"probe_stale has no explanation in any sink")

	close(release)
	p.Stop()
}

// TestWedgedProbeYieldsOneLineThenSilence pins what probeWatched ACTUALLY
// guarantees, which is not what its first comment implied.
//
// The join is deliberate: returning without it would spawn a fresh Evaluate
// every interval against the same wedged dependency, each blocking on the same
// wg.Wait -- unbounded goroutine growth, one per tick.
//
// What it costs is one leaked goroutine and a Start that never advances. Note
// that precisely: Start calls probeWatched BEFORE time.NewTicker, so a first
// probe that wedges means no ticker is ever constructed. There is no dead
// ticker here -- there is no ticker. That is why the silence below is a real
// property and not an artefact of a stopped one.
//
// What must stay true is that the operator gets the one explanatory line AND
// that the reader side then fails closed rather than serving the last green
// verdict forever.
func TestWedgedProbeYieldsOneLineThenSilence(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	rec := &recordingLogger{}
	clock := time.Now()

	p := NewProber(NewReadiness(), []Check{
		{Name: "postgres", Gating: true, Probe: func(context.Context) error {
			once.Do(func() { close(entered) })
			<-release // ignores ctx, as a wedged driver does
			return nil
		}},
	}, 10*time.Millisecond, 40*time.Millisecond, func() time.Time { return clock })
	p.SetLogger(rec.log)

	go p.Start(context.Background())
	<-entered
	require.Eventually(t, func() bool { return rec.sawKV("failure_class", ClassWedged) },
		2*time.Second, 5*time.Millisecond, "the wedge must be reported once")

	// Silence, not a flood: Start is still inside its pre-ticker probeWatched
	// call, so no further lines can accrue however long we wait.
	after := rec.count()
	time.Sleep(120 * time.Millisecond) // longer than 12 intervals, had one existed
	require.Equal(t, after, rec.count(),
		"the wedged probeWatched must not return; if it does, Start reaches "+
			"time.NewTicker and every tick logs another wedge line")

	// And the reader fails CLOSED rather than serving the never-published or
	// last-good verdict.
	_, fresh := p.Current()
	require.False(t, fresh, "/readyz must answer probe_stale while the prober is wedged")

	close(release)
	p.Stop()
}

// kvValue returns the first value logged under key, across all lines.
func (l *recordingLogger) kvValue(key string) (any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, kv := range l.kvs {
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i] == key {
				return kv[i+1], true
			}
		}
	}
	return nil, false
}

// TestWedgeLogNamesTheOutstandingCheck. The wedge line is the entire
// justification for probeWatched, and it used to carry only failure_class and
// budget_ms -- a CONSTANT, not a measurement -- so it named no check at all.
// That matters because /readyz is simultaneously frozen on the last PRE-wedge
// verdict, which is typically all-green, so the body names nothing either: an
// operator on the deploy-failure path got "something overran" and four
// candidates. Postgres wedged and NATS wedged are different pages.
func TestWedgeLogNamesTheOutstandingCheck(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	rec := &recordingLogger{}

	p := NewProber(NewReadiness(), []Check{
		{Name: "fast", Gating: true, Probe: func(context.Context) error { return nil }},
		{Name: "wedged", Gating: true, Probe: func(context.Context) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}},
	}, 10*time.Millisecond, 40*time.Millisecond, time.Now)
	p.SetLogger(rec.log)

	go p.Start(context.Background())
	<-entered
	require.Eventually(t, func() bool { _, ok := rec.kvValue("outstanding"); return ok },
		2*time.Second, 5*time.Millisecond, "the wedge line must carry an outstanding list")

	v, _ := rec.kvValue("outstanding")
	require.Equal(t, []string{"wedged"}, v,
		"only the probe that has not returned may be named; naming the whole check "+
			"set would be no better than naming none")

	close(release)
	p.Stop()
}

// TestPanickingProbeIsReportedAtErrorWithAStack.
//
// Two reasons a panic cannot ride the transition line. A panic in a NON-GATING
// check leaves Ready unmoved, so that line reads to:ready and the severity
// adapter sends it to Warn -- a genuine code defect quieter than a NATS blip.
// And observablyChanged is edge-triggered on (Name, Up, Class), so a panic
// recurring every tick for the process lifetime emits exactly ONE line, ever.
func TestPanickingProbeIsReportedAtErrorWithAStack(t *testing.T) {
	rec := &recordingLogger{}
	p := NewProber(NewReadiness(), []Check{
		{Name: "boom", Gating: false, Probe: func(context.Context) error {
			panic("arbitrary value carrying 10.0.0.5 and a canary9f3a")
		}},
	}, time.Hour, time.Hour, time.Now)
	p.SetLogger(rec.log)
	p.ProbeNow(context.Background())

	require.True(t, rec.sawKV("failure_class", ClassPanic),
		"a panic must carry failure_class so the api severity adapter's Error arm fires; "+
			"without it a non-gating panic lands at Warn")
	require.True(t, rec.sawKV("check", "boom"), "the panicking check must be named")

	stack, ok := rec.kvValue("stack")
	require.True(t, ok, "a stack is program text and is the only thing that locates the defect")
	s, _ := stack.(string)
	require.Contains(t, s, "health.", "the stack must actually be a stack")
	// The VALUE stays dropped -- it is arbitrary and may wrap driver state.
	require.NotContains(t, s, "canary9f3a")
	require.NotContains(t, s, "10.0.0.5")
}

// TestNewProberRejectsANonPositiveStaleAfter. time.NewTicker panics on
// interval <= 0 so that self-reports; staleAfter does not. NewTimer(0) fires
// instantly, so every tick would emit a probe_wedged line -- routed to ERROR by
// the api adapter -- while Current() reports stale forever and /readyz 503s for
// the process lifetime. Same argument the empty-check-set guard makes: it is
// the default for any future caller of this leaf.
func TestNewProberRejectsANonPositiveStaleAfter(t *testing.T) {
	for _, bad := range []time.Duration{0, -time.Second} {
		p := NewProber(NewReadiness(), []Check{
			{Name: "x", Gating: true, Probe: func(context.Context) error { return nil }},
		}, 5*time.Second, bad, time.Now)
		require.Greater(t, p.staleAfter, time.Duration(0),
			"a non-positive staleAfter must be replaced, not honoured")
		require.GreaterOrEqual(t, p.staleAfter, 5*time.Second+ProbeTimeout,
			"the replacement must exceed one full probe cycle or it trips its own fence")
	}
}

// TestStaleVerdictIsNeverSilent closes the window where /readyz reported
// probe_stale and the watchdog said nothing.
//
// The two are armed off DIFFERENT clocks. The timer arms at probe START; the
// freshness fence measures from the last PUBLISHED ObservedAt, stamped at probe
// COMPLETION. The gap between publications is `probe duration + interval`, so
// arming on the first term alone leaves every duration in
// (staleAfter - interval, staleAfter) stale-but-silent. With production
// constants that is any hang between 15s and 20s -- squarely the
// driver-ignoring-cancellation case the watchdog exists for.
//
// Constants are scaled because interval and staleAfter are CONSTRUCTOR
// parameters here; the production values are pinned separately by
// TestReadyzProductionConstantsAreTheirValues.
func TestStaleVerdictIsNeverSilent(t *testing.T) {
	const (
		interval   = 50 * time.Millisecond
		staleAfter = 100 * time.Millisecond
		probeTime  = 95 * time.Millisecond // publication gap 145ms > staleAfter
	)
	rec := &recordingLogger{}
	p := NewProber(NewReadiness(), []Check{
		{Name: "slow", Gating: true, Probe: func(context.Context) error {
			time.Sleep(probeTime)
			return nil
		}},
	}, interval, staleAfter, time.Now)
	p.SetLogger(rec.log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Start(ctx)

	stale := 0
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, fresh := p.Current(); !fresh {
			stale++
		}
		time.Sleep(2 * time.Millisecond)
	}
	p.Stop()
	cancel()

	require.Positive(t, stale, "precondition: this cadence must drive Current() stale")
	require.True(t, rec.sawKV("failure_class", ClassWedged),
		"/readyz reported probe_stale on %d samples with NO wedged line: the watchdog "+
			"must be armed against the PUBLISHED verdict's age, not this probe's runtime", stale)
}
