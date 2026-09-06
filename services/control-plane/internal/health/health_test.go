package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReadinessNilAndZeroAreNotDraining(t *testing.T) {
	var nilR *Readiness
	if nilR.Draining() {
		t.Fatal("nil *Readiness must report not-draining")
	}
	if new(Readiness).Draining() {
		t.Fatal("zero Readiness must report not-draining")
	}
}

func TestMarkDrainingIsVisibleAndIdempotent(t *testing.T) {
	r := NewReadiness()
	r.MarkDraining()
	r.MarkDraining()
	if !r.Draining() {
		t.Fatal("MarkDraining must be observable")
	}
}

func TestEvaluateGatingVsNonGating(t *testing.T) {
	ok := func(context.Context) error { return nil }
	bad := func(context.Context) error { return errors.New("boom") }
	for _, tc := range []struct {
		name    string
		checks  []Check
		wantRdy bool
	}{
		{"all up", []Check{{Name: "a", Gating: true, Probe: ok}}, true},
		{"gating down", []Check{{Name: "a", Gating: true, Probe: bad}}, false},
		{"non-gating down", []Check{{Name: "a", Gating: true, Probe: ok}, {Name: "b", Probe: bad}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Evaluate(context.Background(), nil, tc.checks).Ready; got != tc.wantRdy {
				t.Fatalf("Ready = %v, want %v", got, tc.wantRdy)
			}
		})
	}
}

func TestEvaluateNilProbeFailsClosed(t *testing.T) {
	if Evaluate(context.Background(), nil, []Check{{Name: "a", Gating: true}}).Ready {
		t.Fatal("a nil Probe on a gating check must fail closed")
	}
}

func TestEvaluateDrainingIsNotReadyButStillReports(t *testing.T) {
	r := NewReadiness()
	r.MarkDraining()
	rep := Evaluate(context.Background(), r, []Check{{Name: "a", Gating: true, Probe: func(context.Context) error { return nil }}})
	if rep.Ready {
		t.Fatal("draining must not be Ready")
	}
	if len(rep.Results) != 1 || !rep.Results[0].Up {
		t.Fatal("draining must still evaluate and report checks")
	}
}

// Structural proof of parallelism: both probes must ENTER before either
// returns. A wall-clock ceiling would flake on loaded CI; this cannot pass
// under a sequential implementation because the first probe would block
// forever waiting for a second that is never started.
func TestEvaluateRunsChecksInParallel(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	probe := func(context.Context) error {
		entered <- struct{}{}
		<-release
		return nil
	}
	done := make(chan Report, 1)
	go func() {
		done <- Evaluate(context.Background(), nil,
			[]Check{{Name: "a", Gating: true, Probe: probe}, {Name: "b", Gating: true, Probe: probe}})
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("both probes must be in flight simultaneously; implementation is sequential")
		}
	}
	close(release)
	if !(<-done).Ready {
		t.Fatal("both probes succeeded, want Ready")
	}
}

func TestEvaluateResultsAreInRegistrationOrder(t *testing.T) {
	slow := func(context.Context) error { time.Sleep(50 * time.Millisecond); return nil }
	fast := func(context.Context) error { return nil }
	rep := Evaluate(context.Background(), nil,
		[]Check{{Name: "slow", Gating: true, Probe: slow}, {Name: "fast", Gating: true, Probe: fast}})
	if rep.Results[0].Name != "slow" || rep.Results[1].Name != "fast" {
		t.Fatalf("results must be in registration order, got %s,%s", rep.Results[0].Name, rep.Results[1].Name)
	}
}
