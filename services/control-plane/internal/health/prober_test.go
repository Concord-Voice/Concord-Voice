package health

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestProberPublishesAndIsSingleFlight(t *testing.T) {
	var concurrent, maxConcurrent int32
	probe := func(context.Context) error {
		n := atomic.AddInt32(&concurrent, 1)
		for {
			m := atomic.LoadInt32(&maxConcurrent)
			if n <= m || atomic.CompareAndSwapInt32(&maxConcurrent, m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return nil
	}
	p := NewProber(NewReadiness(), []Check{{Name: "pg", Gating: true, Probe: probe}},
		5*time.Millisecond, time.Minute, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	go p.Start(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()
	if got := atomic.LoadInt32(&maxConcurrent); got != 1 {
		t.Fatalf("prober ran %d probes concurrently; a tick firing while one is "+
			"in flight must be SKIPPED, not queued", got)
	}
	if _, ok := p.Current(); !ok {
		t.Fatal("prober must publish a fresh verdict")
	}
}

// The fence. Falsified by construction: without it a stuck prober reports the
// last green verdict forever, which is the exact "green while wedged" defect
// the dedicated-handle design was rejected for.
//
// Synchronizes on the prober goroutine's actual exit via a "stopped" channel
// rather than sleeping after cancel(): the injected clock is an unsynchronized
// closure variable, so mutating it while Start's goroutine might still be
// mid-probe (reading it via p.now()) is a genuine data race, not a timing
// nicety -- confirmed by `go test -race` against a sleep-based version of this
// test. The channel close/receive establishes the happens-before edge the
// clock mutation needs.
func TestCurrentReportsStaleWhenTheProberStops(t *testing.T) {
	clock := time.Now()
	p := NewProber(NewReadiness(), []Check{{Name: "pg", Gating: true,
		Probe: func(context.Context) error { return nil }}},
		5*time.Millisecond, 50*time.Millisecond, func() time.Time { return clock })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		p.Start(ctx)
		close(stopped)
	}()
	time.Sleep(50 * time.Millisecond)
	if _, ok := p.Current(); !ok {
		t.Fatal("precondition: a running prober must publish fresh")
	}
	cancel()
	<-stopped                    // the prober has now actually stopped -- "stuck" from the reader's view
	clock = clock.Add(time.Hour) // advance the injected clock past staleAfter
	if _, ok := p.Current(); ok {
		t.Fatal("a verdict older than staleAfter must report NOT fresh, never last-known-good")
	}
}

func TestCurrentIsFalseBeforeTheFirstProbe(t *testing.T) {
	p := NewProber(NewReadiness(), nil, time.Second, time.Minute, time.Now)
	if _, ok := p.Current(); ok {
		t.Fatal("no verdict has been published yet; must fail closed, not report ready")
	}
}

func TestProberIsNilSafe(t *testing.T) {
	var p *Prober
	p.SetLogger(func(string, ...any) {})
	p.Start(context.Background())
	p.ProbeNow(context.Background())
	if _, ok := p.Current(); ok {
		t.Fatal("nil *Prober must report not-fresh, never a usable verdict")
	}
}

func TestProbeNowPublishesWithoutStartingTheTicker(t *testing.T) {
	p := NewProber(NewReadiness(), []Check{{Name: "pg", Gating: true,
		Probe: func(context.Context) error { return nil }}},
		time.Hour, time.Hour, time.Now)
	p.ProbeNow(context.Background())
	v, ok := p.Current()
	if !ok {
		t.Fatal("ProbeNow must publish a fresh verdict synchronously")
	}
	if !v.Ready {
		t.Fatal("the single check succeeded, want Ready")
	}
}

func TestProberLogsOnlyOnReadyEdgeTransitions(t *testing.T) {
	up := true
	probe := func(context.Context) error {
		if up {
			return nil
		}
		return context.DeadlineExceeded
	}
	p := NewProber(NewReadiness(), []Check{{Name: "pg", Gating: true, Probe: probe}},
		time.Hour, time.Hour, time.Now)
	var calls int32
	p.SetLogger(func(string, ...any) { atomic.AddInt32(&calls, 1) })

	p.ProbeNow(context.Background()) // unknown -> ready: logs
	p.ProbeNow(context.Background()) // ready -> ready: silent
	up = false
	p.ProbeNow(context.Background()) // ready -> not_ready: logs
	up = true
	p.ProbeNow(context.Background()) // not_ready -> ready: logs

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("edge-transition log calls = %d, want 3 (steady-state ticks must stay silent)", got)
	}
}

// TestStopHaltsTheProberAndIsIdempotent pins the lifetime handle that replaced
// the context.CancelFunc. Without this the swap would be cosmetic: a Stop that
// does not stop leaks exactly the goroutine the CancelFunc was there to avoid.
func TestStopHaltsTheProberAndIsIdempotent(t *testing.T) {
	var probes int64
	p := NewProber(NewReadiness(), []Check{{Name: "x", Gating: true,
		Probe: func(context.Context) error { atomic.AddInt64(&probes, 1); return nil }}},
		2*time.Millisecond, time.Minute, time.Now)

	done := make(chan struct{})
	go func() { p.Start(context.Background()); close(done) }()

	time.Sleep(30 * time.Millisecond)
	p.Stop()
	p.Stop() // idempotent: a second close would panic
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Stop — the prober goroutine leaks")
	}

	settled := atomic.LoadInt64(&probes)
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt64(&probes); got != settled {
		t.Fatalf("prober kept probing after Stop: %d -> %d", settled, got)
	}
}

// TestNilProberIsSafeOnEveryMethod pins the whole nil contract, not just Stop.
// The handler is reachable with a nil *Prober (an embedder that never wired
// one), and it is an UNAUTHENTICATED endpoint — a nil-deref there is a crash
// anyone on the container network can trigger.
func TestNilProberIsSafeOnEveryMethod(t *testing.T) {
	var p *Prober
	p.Stop() // must not panic, and must not close a nil channel

	if p.Draining() {
		t.Fatal("a nil *Prober must report not-draining, not panic")
	}
	if _, fresh := p.Current(); fresh {
		t.Fatal("a nil *Prober must report its verdict as NOT fresh, so callers fail closed")
	}
}
