package health

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Verdict is a probe result plus the instant it was observed. ObservedAt is
// load-bearing, not decoration: it is what lets a reader tell a fresh verdict
// from a stuck prober's last green one.
type Verdict struct {
	Report
	ObservedAt time.Time
}

// Prober evaluates checks on a ticker and publishes the latest Verdict.
//
// It exists so the HTTP handler never performs I/O. That is the whole design:
// database/sql charges connection ACQUISITION to the caller's context, so a
// handler that pings cannot distinguish a saturated pool from a dead database
// and blocks a user request either way. With the probe on a ticker, nothing
// user-facing waits, acquisition delay becomes a MEASUREMENT rather than a
// false negative, and the probe can therefore use the SERVING pool and share
// its fate -- instead of a second pool that reports green while the pool the
// application actually uses is wedged.
//
// Single-flight: a tick that arrives while a probe is in flight is SKIPPED.
// Queueing them is what turns one slow probe into an unbounded backlog. This
// falls out of Start's loop structure rather than needing its own guard: the
// loop body is the only caller of probe, and it is synchronous, so an
// overrunning probe simply causes the ticker to drop the next tick rather
// than starting a second one.
type Prober struct {
	readiness  *Readiness
	checks     []Check
	interval   time.Duration
	staleAfter time.Duration
	now        func() time.Time
	log        func(msg string, kv ...any)
	current    atomic.Pointer[Verdict]

	// stop is the prober's OWN lifetime handle rather than a
	// context.CancelFunc held by whoever constructed it. A long-lived worker
	// that outlives its constructor's stack frame cannot express its lifetime
	// as a context cancelled by a deferred call, and pretending otherwise
	// produces exactly the shape a linter flags: a CancelFunc created in one
	// scope and invoked from another.
	stopOnce sync.Once
	stopped  chan struct{}
}

// NewProber constructs a Prober. now defaults to time.Now when nil, which
// lets a caller inject a controllable clock in tests without every
// production call site having to pass one explicitly.
func NewProber(r *Readiness, checks []Check, interval, staleAfter time.Duration, now func() time.Time) *Prober {
	if now == nil {
		now = time.Now
	}
	if staleAfter <= 0 {
		// time.NewTicker panics on interval <= 0, so that self-reports. This
		// does not: NewTimer(0) fires instantly, so every tick would emit a
		// probe_wedged line -- routed to ERROR -- while Current() reports stale
		// forever. Same argument the empty-check-set guard makes for itself:
		// it is the default for any future caller of this leaf.
		staleAfter = interval + ProbeTimeout
	}
	return &Prober{
		readiness:  r,
		checks:     checks,
		interval:   interval,
		staleAfter: staleAfter,
		now:        now,
		stopped:    make(chan struct{}),
	}
}

// SetLogger installs the edge-transition logger (readiness changes are logged
// on the EDGE, in the prober -- never as level-suppression in the request
// middleware, which would key the silence on a shape that anything failing
// through it silently inherits).
//
// internal/health cannot import pkg/logger (the leaf-package constraint), so
// the caller adapts its own logger into this shape -- e.g. a *logger.Logger's
// Info method already has this signature. Nil-safe both on the receiver and
// on the argument: an unset logger simply makes the transition log a no-op,
// never a panic. Evaluating readiness must never depend on a logger existing.
func (p *Prober) SetLogger(log func(msg string, kv ...any)) {
	if p == nil {
		return
	}
	p.log = log
}

// Stop ends the prober. Idempotent, and safe to call from any goroutine or
// more than once — which is what lets it be folded into an existing shutdown
// closer without that closer having to know whether it already ran.
func (p *Prober) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stopped) })
}

// Start blocks until ctx is done or Stop is called. Run it in its own
// goroutine. The ctx arm exists for tests and for callers that already have a
// lifetime context; production uses Stop.
func (p *Prober) Start(ctx context.Context) {
	if p == nil {
		return
	}
	p.probeWatched(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopped:
			return
		case <-t.C:
			p.probeWatched(ctx)
		}
	}
}

// ProbeNow runs one evaluation synchronously and publishes it, without
// starting the ticker loop. It exists for a caller (chiefly tests) that wants
// a deterministic first verdict without racing Start's background goroutine.
// It is NOT single-flight-safe against a concurrently running Start loop --
// callers that need that guarantee use Start alone.
func (p *Prober) ProbeNow(ctx context.Context) {
	if p == nil {
		return
	}
	p.probe(ctx)
}

// probeWatched runs one probe and reports it if it OVERRUNS, which is the only
// way a wedged prober is ever noticed.
//
// ProbeTimeout bounds the probes' CONTEXT, not Evaluate's return: a probe that
// ignores cancellation blocks wg.Wait, which blocks Evaluate, which blocks this
// loop body, and single-flight then means the ticker silently drops every
// subsequent tick. The reader side fences that (Current goes stale and /readyz
// 503s `probe_stale`), but before #3130 nothing on the WRITER side noticed it
// had stopped probing -- so the endpoint went stale with no log line anywhere
// saying which check hung, including on the deploy-failure path where
// concord-ctl.sh prints the body.
//
// THE GUARANTEE IS "one line, then silence" -- not "one line, then continued
// bounded reporting", which an earlier version of this comment implied.
//
// The join below is what makes it so: on a probe that never returns, this
// function never returns either, Start's loop never reaches its next select,
// and the ticker is dead for the rest of the process. That is deliberate, and
// the alternative is worse. Returning without joining would spawn a fresh
// Evaluate every interval against the same wedged dependency, each one
// blocking on the same wg.Wait -- unbounded goroutine growth, one per tick,
// forever. Joining costs exactly one leaked goroutine.
//
// The reader side is what stays correct in that state: Current() goes stale, so
// /readyz answers 503 probe_stale rather than serving the last green verdict,
// and this single log line is the only place the CAUSE is named. Recovery is
// #3107's job -- a wedged driver cannot be un-wedged from inside the process,
// since Go cannot kill a goroutine.
func (p *Prober) probeWatched(ctx context.Context) {
	finished := make([]atomic.Bool, len(p.checks))
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.probeTracked(ctx, finished)
	}()

	// Budget from when the PUBLISHED verdict goes stale, not from how long this
	// probe has been running. Those are two different clocks: the timer arms at
	// probe START, while Current()'s freshness measures from the last
	// ObservedAt, which probe() stamps at probe COMPLETION. The gap between
	// published verdicts is `probe duration + interval`, and arming on the
	// first term alone leaves every duration in
	// (staleAfter - interval, staleAfter) reporting probe_stale with NO log
	// line -- with production constants, any hang between 15s and 20s, which is
	// squarely the driver-ignoring-cancellation case this watchdog exists for.
	budget := p.staleAfter
	if prev := p.current.Load(); prev != nil && !prev.ObservedAt.IsZero() {
		if remaining := p.staleAfter - p.now().Sub(prev.ObservedAt); remaining < budget {
			budget = remaining
		}
	}
	if budget < 0 {
		budget = 0
	}
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	if p.log != nil {
		// Name the checks still outstanding. budget_ms alone is a constant and
		// says nothing about WHICH probe hung -- and the frozen /readyz body
		// shows the last PRE-wedge verdict, which is typically all-green, so
		// without this an operator on the deploy-failure path has four
		// candidates and no way to discriminate.
		outstanding := make([]string, 0, len(p.checks))
		for i := range p.checks {
			if !finished[i].Load() {
				outstanding = append(outstanding, p.checks[i].Name)
			}
		}
		p.log("readiness probe overran its staleness budget; /readyz will report probe_stale",
			"failure_class", ClassWedged, "budget_ms", budget.Milliseconds(),
			"outstanding", outstanding)
	}
	<-done
}

func (p *Prober) probe(ctx context.Context) { p.probeTracked(ctx, nil) }

func (p *Prober) probeTracked(ctx context.Context, finished []atomic.Bool) {
	rep := evaluate(ctx, p.readiness, p.checks, finished, p.onProbePanic)
	prev := p.current.Load()
	if p.log != nil && (prev == nil || observablyChanged(prev.Report, rep)) {
		from := "unknown"
		if prev != nil {
			from = readyWord(prev.Ready)
		}
		// `draining` is carried so the severity adapter can tell a PLANNED
		// shutdown from a dependency loss. Evaluate sets Ready = !Draining, so
		// the first tick after the drain latches reads `to: not_ready` with an
		// EMPTY failing list -- byte-identical, at the adapter's inputs, to
		// Postgres dying. It is a bounded server-constructed bool and can hold
		// no wire data.
		p.log("readiness transition", "from", from, "to", readyWord(rep.Ready),
			"draining", rep.Draining, "failing", failingChecks(rep))
	}
	p.current.Store(&Verdict{Report: rep, ObservedAt: p.now()})
}

// observablyChanged reports whether anything an operator would act on moved.
//
// The predicate used to be `prev.Ready != rep.Ready`, and Ready is moved ONLY
// by a gating check (Evaluate). So a NON-GATING check could go down on tick 2
// and stay down for the process's whole life while emitting zero log lines --
// which made both non-gating checks (pool, nats) unobservable everywhere at
// once, since nothing polls the /readyz body in steady state either. The
// "alerting-only signal" they were declared for had nothing to alert on.
func observablyChanged(prev, cur Report) bool {
	if prev.Ready != cur.Ready || len(prev.Results) != len(cur.Results) {
		return true
	}
	for i := range cur.Results {
		if prev.Results[i].Name != cur.Results[i].Name ||
			prev.Results[i].Up != cur.Results[i].Up ||
			prev.Results[i].Class != cur.Results[i].Class {
			return true
		}
	}
	return false
}

// onProbePanic reports a panicking probe at ERROR, separately from the
// transition line.
//
// Two reasons it cannot ride the transition log. A panic in a NON-GATING check
// leaves Ready unmoved, so that line reads `to: ready` and the severity adapter
// sends it to Warn -- a genuine code defect quieter than a NATS reconnect blip.
// And observablyChanged is edge-triggered on (Name, Up, Class), so a panic
// recurring on every tick for the process lifetime would emit exactly ONE line,
// ever. failure_class routes this through the adapter's existing Error arm.
//
// The stack is program text and is safe for the sink; the panic VALUE is not
// and never leaves Evaluate.
func (p *Prober) onProbePanic(name string, stack []byte) {
	if p == nil || p.log == nil {
		return
	}
	p.log("readiness probe PANICKED; a check must never take the process down",
		"failure_class", ClassPanic, "check", name, "stack", string(stack))
}

func readyWord(ready bool) string {
	if ready {
		return WordReady
	}
	return WordNotReady
}

// failingChecks renders each down check as `name=class`. BOTH halves are
// bounded: Name is a caller-supplied constant and Class is from the closed set
// above, so neither can carry wire data.
//
// It must NEVER reach for r.Err. lib/pq formats a dial failure with host and
// port; pgx adds user and database (CWE-532). The HTTP body has a test pinning
// that redaction; this is the same property on the log sink, and it now has
// one too.
//
// Both gating and non-gating checks are named, so a flap caused by a
// non-gating check is diagnosable from the line alone -- which is true only
// because observablyChanged now fires on one.
func failingChecks(rep Report) []string {
	out := make([]string, 0, len(rep.Results))
	for _, r := range rep.Results {
		if !r.Up {
			class := r.Class
			if class == "" {
				class = ClassError
			}
			out = append(out, r.Name+"="+class)
		}
	}
	return out
}

// Age reports how old a verdict is on the PROBER's clock -- the injected one,
// so a test that freezes time gets a deterministic answer instead of whatever
// time.Since says. Zero for a verdict that was never published.
func (p *Prober) Age(v Verdict) time.Duration {
	if p == nil || v.ObservedAt.IsZero() {
		return 0
	}
	return p.now().Sub(v.ObservedAt)
}

// Draining reports the LIVE drain state, not the cached verdict's snapshot of
// it.
//
// This exists because the two are not the same fact. A dependency check is an
// OBSERVATION and may legitimately be up to one tick old; the drain flag is
// PROCESS STATE the process knows instantly, and caching it introduces a lag
// that destroys the only thing it is for. On SIGTERM the flag is latched and
// srv.Shutdown closes the listener within microseconds, while the next probe
// tick is up to readyzProbeInterval (5s) away -- so a handler reading
// Verdict.Draining would never once observe a drain in production.
//
// Exposed from Prober rather than passing a second *Readiness to the handler:
// the Prober already holds the one instance, and a second parameter would
// re-open the two-instance hazard this design went out of its way to close.
func (p *Prober) Draining() bool {
	// Nil-safe at both levels, matching Current(): the handler is reachable
	// with a nil *Prober (an embedder that never wired one), and a nil
	// *Readiness reports not-draining by contract.
	if p == nil {
		return false
	}
	return p.readiness.Draining()
}

// Current returns the latest verdict and whether it is FRESH. A false second
// return means "no usable answer" -- never "not ready" and never
// last-known-good -- and callers must fail closed on it. A stuck prober
// goroutine (deadlock, a driver ignoring cancellation) is an async prober's
// characteristic failure, and without this fence its last green verdict is
// served forever.
//
// PANIC is deliberately NOT in that list, though an earlier version of this
// comment claimed it. An unrecovered panic terminates the process, so there is
// no survivor left to serve a stale verdict and this fence could never observe
// it; Evaluate now recovers per-probe instead, which is the actual guard.
// probeWatched covers the other two by reporting the overrun.
func (p *Prober) Current() (Verdict, bool) {
	if p == nil {
		return Verdict{}, false
	}
	v := p.current.Load()
	if v == nil {
		return Verdict{}, false
	}
	if p.now().Sub(v.ObservedAt) > p.staleAfter {
		return *v, false
	}
	return *v, true
}
