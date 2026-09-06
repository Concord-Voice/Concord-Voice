// Package health holds the control-plane's readiness state and probe
// evaluator (#3106).
//
// It is deliberately a STDLIB-ONLY LEAF: imports context, errors, sync,
// sync/atomic, and time ONLY. internal/api already imports internal/websocket,
// so readiness state placed in internal/api would be unreachable from the
// websocket package -- which is where a future drain frame (#2753) would need
// to read it. Keeping this package importable by api, websocket and
// cmd/server without a cycle is the whole reason it exists, and adding any
// in-repo import here forfeits that. Checks arrive as caller-supplied
// closures precisely so this package never learns what a database is.
package health

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// ProbeTimeout bounds ALL checks together, not each one. Three independent
// budgets can exceed the compose healthcheck's `timeout:` and flap the
// container; one shared parent deadline cannot.
const ProbeTimeout = 2 * time.Second

// WordReady / WordNotReady are the verdict strings the transition log carries.
// They are EXPORTED because internal/api's severity adapter matches on them:
// while they were an unexported implementation detail, the adapter held a bare
// "not_ready" literal, and renaming this word would have silently dropped every
// readiness-LOSS line from Error to Info with nothing failing to compile.
const (
	WordReady    = "ready"
	WordNotReady = "not_ready"
)

var (
	errNilProbe      = errors.New("check has no probe")
	errProbePanicked = errors.New("probe panicked")
)

// Failure classes: a CLOSED set of server-constructed labels. Every one is a
// compile-time constant, so none can carry wire data, a driver string, a
// hostname or a credential -- which is what makes them the only part of a
// failure that may reach a log sink (observability.md 1/2/5, the same posture
// as router.go's failure_class=nats_config).
//
// Result.Err holds the real cause and is neither logged nor serialized. Before
// #3130 the cause was computed and then discarded entirely: an operator saw
// "postgres: down" and could not distinguish connection-refused from a probe
// timeout from errNilProbe -- a WIRING BUG reported as a dead database.
const (
	ClassUnwired   = "probe_unwired"
	ClassTimeout   = "probe_timeout"
	ClassCancelled = "probe_cancelled"
	ClassPanic     = "probe_panic"
	ClassError     = "probe_error"
	ClassNoChecks  = "no_checks_configured"
	ClassWedged    = "probe_wedged"
)

// ClassifiedError lets a caller attach its OWN bounded class to a probe failure
// without this package learning what a database or a message bus is. Only the
// class travels; the wrapped error never leaves the process.
type ClassifiedError struct {
	Class string
	Err   error
}

func (e *ClassifiedError) Error() string { return e.Err.Error() }
func (e *ClassifiedError) Unwrap() error { return e.Err }

// classify maps a probe error to a bounded label. A caller-supplied class wins,
// so a check can separate causes this package cannot see -- a permanently
// unconfigured NATS client from a transient reconnect window, say, which are
// the same bool to IsConnected() but opposite operator actions.
func classify(err error) string {
	var ce *ClassifiedError
	if errors.As(err, &ce) && ce.Class != "" {
		return ce.Class
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, context.Canceled):
		return ClassCancelled
	default:
		return ClassError
	}
}

// Readiness is the process-wide drain flag.
//
// atomic.Bool rather than a mutex: one writer (the shutdown path, once) and N
// readers (every in-flight prober tick and /readyz request). A plain bool is
// a race the memory model permits never to become visible to a reader. An
// RWMutex would also be correct but costs a lock per read for a single-word
// value that must be consistent with nothing else -- there is no second field
// to keep in step, so the atomic is the COMPLETE synchronisation requirement,
// not merely the cheap one.
//
// The flag is write-once-monotonic: there is no undrain. A read that races the
// flip and observes false is drained normally by the HTTP server's own
// shutdown, which is the wanted behaviour rather than a TOCTOU bug.
type Readiness struct{ draining atomic.Bool }

// NewReadiness constructs a not-draining Readiness flag.
func NewReadiness() *Readiness { return &Readiness{} }

// Draining is nil-safe so an embedder or test that leaves the field unset
// gets a working not-draining default rather than a panic.
func (r *Readiness) Draining() bool { return r != nil && r.draining.Load() }

// MarkDraining flips the flag. Nil-safe and idempotent.
func (r *Readiness) MarkDraining() {
	if r != nil {
		r.draining.Store(true)
	}
}

// Check is one dependency probe. Gating decides whether a failure changes the
// HTTP status; a non-gating failure is reported without affecting readiness.
type Check struct {
	Name   string
	Gating bool
	Probe  func(context.Context) error
}

// Result is one check's outcome.
type Result struct {
	Name   string
	Gating bool
	Up     bool
	// Err is the real cause. It MUST NOT be serialized and MUST NOT be logged
	// -- driver errors carry hosts, ports and, under pgx, user and database
	// names (CWE-532).
	Err error
	// Class is the bounded label for Err and the ONLY part of a failure that
	// may reach a log sink. Empty when Up.
	Class string
}

// Report is one evaluation's full verdict.
type Report struct {
	Draining bool
	Ready    bool
	Results  []Result
}

// Evaluate runs every check concurrently under ONE deadline derived from ctx
// and capped at ProbeTimeout.
//
// It is called by the background Prober on a ticker, NEVER from an HTTP
// handler. That is what lets the probe use the SERVING pool: the objection to
// shared-pool probing was only ever an objection to charging connection
// ACQUISITION to a request's context.
//
// Concurrent rather than sequential is correctness, not speed: sharing one
// budget serially lets a slow Postgres consume it and report Redis down while
// Redis is fine -- a misattribution that lands precisely during an incident,
// when this endpoint is the only thing anyone is reading.
//
// Results are written into pre-indexed slots of a pre-sized slice, so they
// are in registration order regardless of completion order, with no mutex and
// no append race.
func Evaluate(ctx context.Context, r *Readiness, checks []Check) Report {
	return evaluate(ctx, r, checks, nil, nil)
}

// evaluate is Evaluate plus two optional observers, both nil for external
// callers. They exist because the Prober needs facts the Report cannot carry:
//
//   - finished[i] is set when probe i returns, so a watchdog firing while
//     Evaluate is still blocked can name WHICH checks are outstanding. Without
//     it the wedge log named none of them, and the frozen /readyz body shows
//     the pre-wedge verdict, so an operator had four candidates and no way to
//     discriminate -- Postgres wedged and NATS wedged are different pages.
//   - onPanic receives the check name and the STACK. The panic value stays
//     dropped (it is arbitrary and may wrap driver state); a stack is program
//     text -- function names, files, lines -- and is the only thing that
//     locates the defect.
func evaluate(
	ctx context.Context,
	r *Readiness,
	checks []Check,
	finished []atomic.Bool,
	onPanic func(name string, stack []byte),
) Report {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	results := make([]Result, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		results[i] = Result{Name: c.Name, Gating: c.Gating}
		if c.Probe == nil {
			// Fail closed: an unwired probe is a wiring bug, not "no check
			// configured".
			results[i].Err = errNilProbe
			results[i].Class = ClassUnwired
			continue
		}
		wg.Add(1)
		go func(i int, name string, probe func(context.Context) error) {
			defer wg.Done()
			// Registered BEFORE runProbe's recover, so it runs AFTER it: a
			// panicking probe still counts as finished and is not reported as
			// outstanding by the watchdog.
			if finished != nil {
				defer finished[i].Store(true)
			}
			runProbe(ctx, &results[i], name, probe, onPanic)
		}(i, c.Name, c.Probe)
	}
	wg.Wait()
	return summarise(r, checks, results)
}

// runProbe executes one probe into its own pre-indexed slot.
//
// A panicking probe must not kill the process. Two of the four production
// checks are declared NON-GATING precisely so a failure there can never move
// readiness; without the recover, a panic inside one of them escalates to
// process death, taking every live voice session with it -- a check
// contractually forbidden from causing a 503 causing a process exit instead.
//
// The panic VALUE is deliberately dropped: it is arbitrary and may wrap driver
// state, so it is exactly what must not reach a sink. The STACK is program text
// and is the only thing that locates the defect. `recover() != nil` rather than
// binding it, since the value is never read; recover is still called DIRECTLY
// by the deferred function, which is what the spec requires.
func runProbe(
	ctx context.Context,
	res *Result,
	name string,
	probe func(context.Context) error,
	onPanic func(name string, stack []byte),
) {
	defer func() {
		if recover() != nil {
			res.Err = errProbePanicked
			res.Class = ClassPanic
			if onPanic != nil {
				onPanic(name, debug.Stack())
			}
		}
	}()
	if err := probe(ctx); err != nil {
		res.Err = err
		res.Class = classify(err)
		return
	}
	res.Up = true
}

// summarise folds the per-check results into the verdict.
//
// An empty check set means "I have been told nothing", not "everything is
// fine". Every other unknown in this package fails closed -- nil probe, nil
// prober, unpublished verdict, stale verdict -- and this was the one that did
// not. It is also the default for any future caller of this leaf.
func summarise(r *Readiness, checks []Check, results []Result) Report {
	rep := Report{Draining: r.Draining(), Results: results}
	rep.Ready = !rep.Draining
	if len(checks) == 0 {
		rep.Ready = false
	}
	for _, res := range results {
		if res.Gating && !res.Up {
			rep.Ready = false
		}
	}
	return rep
}
