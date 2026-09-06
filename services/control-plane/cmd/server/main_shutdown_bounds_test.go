package main

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

// stagesFor drives shutdownControlPlane with a recorder, blocking whichever
// stage is named in block until release is closed.
//
// The results are UNNAMED deliberately. With named results, `return
// append(...), append(...)` assigns the copies to the result variables and the
// return machinery reads them back AFTER the deferred mu.Unlock() has run --
// while the abandoned stage's goroutine is still appending under the lock. One
// side locked, one side not, which is a real race the detector catches. It is
// reachable here only because this test does what production does: leave a
// goroutine running past the wait.
func stagesFor(t *testing.T, budget time.Duration, block string) ([]string, []string) {
	t.Helper()
	drainCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var mu sync.Mutex
	var order, abandoned []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	stage := func(name string) func() {
		return func() {
			if name == block {
				<-release // ignores the deadline, as a wedged drain does
			}
			note(name)
		}
	}
	err := shutdownControlPlane(
		drainCtx,
		func(s string, starved bool) {
			mu.Lock()
			if starved {
				s += "(starved)"
			}
			abandoned = append(abandoned, s)
			mu.Unlock()
		},
		func() { note("cancel") },
		func() error { note("http"); return nil },
		stage("activity"),
		stage("presence"),
		stage("hub"),
		func() error { note("metrics"); return nil },
		func() error { note("reader"); return nil },
		func() { note("nats") },
	)
	if err != nil {
		t.Fatalf("shutdownControlPlane returned %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), order...), append([]string(nil), abandoned...)
}

// TestAWedgedStageNoLongerSkipsEveryStageAfterIt is the whole point of the
// bound, and the property the unbounded version could not have.
//
// waitBackgroundWorkers, closePresenceWorkers and hub.Shutdown took no context.
// A stage that blocked ran until Docker SIGKILLed the process at
// stop_grace_period -- and SIGKILL does not merely truncate that stage, it
// skips every stage after it. The NATS drain is the one that matters: it is
// last, and skipping it drops buffered publishes.
//
// The bound cannot cancel the wedged stage (Go has no such primitive), so its
// remaining work is lost either way. What changes is that the loss stops there.
func TestAWedgedStageNoLongerSkipsEveryStageAfterIt(t *testing.T) {
	// The left side is this test's label for the stage; the right side is the
	// name production reports in the overrun log. They differ, and asserting on
	// the production name is the point -- an operator greps for that string.
	for _, tc := range []struct{ label, reported string }{
		{"activity", "background_workers"},
		{"presence", "presence_workers"},
		{"hub", "hub"},
	} {
		wedged, reported := tc.label, tc.reported
		t.Run(wedged, func(t *testing.T) {
			order, abandoned := stagesFor(t, 20*time.Millisecond, wedged)

			// The tail MUST still run. Asserting "nats ran" rather than "the
			// function returned" is deliberate: the unbounded version also
			// returned eventually, just far too late to matter.
			for _, want := range []string{"metrics", "reader", "nats"} {
				if !contains(order, want) {
					t.Fatalf("stage %q wedged and %q never ran; order=%v. Every stage after a "+
						"wedged one is exactly what SIGKILL used to discard.", wedged, want, order)
				}
			}
			if !contains(abandoned, reported) {
				t.Fatalf("stage %q wedged but was not reported as abandoned (got %v). "+
					"An abandoned stage that logs nothing is indistinguishable from one "+
					"that completed, and AC-10 has nothing to observe.", reported, abandoned)
			}
			// The wedged stage itself must NOT appear: it never finished.
			if contains(order, wedged) {
				t.Fatalf("stage %q reported completion despite being wedged; order=%v", wedged, order)
			}
		})
	}
}

// TestEveryStageStillRunsInOrderWhenNothingOverruns is the positive control.
// Without it, a bound that abandoned every stage immediately would satisfy the
// test above -- the tail would run, and nothing would notice the drain never
// did.
func TestEveryStageStillRunsInOrderWhenNothingOverruns(t *testing.T) {
	order, abandoned := stagesFor(t, time.Minute, "")

	want := []string{"cancel", "http", "activity", "presence", "hub", "metrics", "reader", "nats"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (differs at %d)", order, want, i)
		}
	}
	if len(abandoned) != 0 {
		t.Fatalf("stages completed but %v were reported as abandoned", abandoned)
	}
}

// TestPresenceStillDrainsBeforeTheHubCloses pins the ordering constraint the
// bound must not disturb: the presence queues' fail-closed abandons disconnect
// THROUGH the hub, so closing the hub first discards the work the drain exists
// to perform (#2738).
func TestPresenceStillDrainsBeforeTheHubCloses(t *testing.T) {
	order, _ := stagesFor(t, time.Minute, "")
	if indexOf(order, "presence") > indexOf(order, "hub") {
		t.Fatalf("hub closed before the presence drain; order=%v", order)
	}
}

// TestShutdownBudgetsFitTheComposeStopGracePeriod reads the grace period from
// the compose file rather than restating it, so raising a budget without
// raising the grace fails here instead of in production as a 137.
//
// LIMIT, stated rather than implied: the NATS drain's own 2s budget lives in
// pkg/nats as an unexported constant and cannot be read from here. This asserts
// that at least that much headroom REMAINS; it cannot notice if pkg/nats
// raises its own bound.
func TestShutdownBudgetsFitTheComposeStopGracePeriod(t *testing.T) {
	const natsHeadroom = 2 * time.Second   // owned by pkg/nats.drainWaitBudget
	const teardownMargin = 1 * time.Second // process exit after the last stage

	for _, f := range []string{"../../../../docker-compose.yml", "../../../../docker-compose.production.yml"} {
		grace := controlPlaneStopGrace(t, f)
		spent := drainSettle + shutdownDrainBudget + shutdownMetricsBudget + shutdownAdminReaderBudget
		if spent+natsHeadroom+teardownMargin > grace {
			t.Fatalf("%s: stop_grace_period is %s but the shutdown path claims %s "+
				"(settle %s + drain %s + metrics %s + reader %s), leaving less than the "+
				"%s the NATS drain needs plus %s of teardown margin. Docker SIGKILLs the "+
				"difference, and a SIGKILL skips the NATS drain entirely.",
				f, grace, spent, drainSettle, shutdownDrainBudget,
				shutdownMetricsBudget, shutdownAdminReaderBudget, natsHeadroom, teardownMargin)
		}
	}
	if shutdownHTTPBudget >= shutdownDrainBudget {
		t.Fatalf("shutdownHTTPBudget (%s) does not sub-cap shutdownDrainBudget (%s), so a slow "+
			"HTTP drain can consume the whole phase and starve the presence and "+
			"activity-history flushes that follow it", shutdownHTTPBudget, shutdownDrainBudget)
	}
}

var stopGraceRe = regexp.MustCompile(`(?m)^\s+stop_grace_period:\s*(\d+)s\s*$`)

// controlPlaneStopGrace reads the control-plane service's stop_grace_period,
// scanning only that service's block so a sibling service's value cannot
// satisfy the assertion.
func controlPlaneStopGrace(t *testing.T, path string) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed compose paths, literals owned by this test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?ms)^  control-plane:\n(.*?)(?:^  [a-z]|\z)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("%s: no control-plane service block", path)
	}
	m := stopGraceRe.FindSubmatch(block[1])
	if m == nil {
		t.Fatalf("%s: control-plane declares no stop_grace_period", path)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("%s: unparseable stop_grace_period %q: %v", path, m[1], err)
	}
	return time.Duration(n) * time.Second
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

// TestAnEarlyWedgeStarvesTheStagesAfterIt pins a CONSEQUENCE of the single
// shared deadline, so it is a known property rather than a surprise in an
// incident.
//
// One deadline means one pot. A stage that wedges consumes the entire remaining
// budget, so every stage after it is abandoned on arrival -- a wedged
// background-worker wait takes the presence drain down with it, and that drain
// writes Server Voice state nothing else recomputes.
//
// Accepted deliberately rather than solved with per-stage floors: the
// alternative is a knob per stage whose correct value nobody can derive, and
// the comparison that matters is against the previous behaviour, where the same
// wedge ran to stop_grace_period and then SIGKILL discarded the tail as well.
// The tail is what changes hands here -- the NATS drain in particular.
//
// If production ever shows this cascade firing (failure_class
// shutdown_stage_overran, more than one stage per shutdown), per-stage floors
// are the next move, and this test is where to record the decision.
func TestAnEarlyWedgeStarvesTheStagesAfterIt(t *testing.T) {
	order, abandoned := stagesFor(t, 20*time.Millisecond, "activity")

	// The wedged stage HAD the budget and used it; the two after it never ran.
	// Conflating those sends an operator after two innocent stages.
	for _, want := range []string{"background_workers", "presence_workers(starved)", "hub(starved)"} {
		if !contains(abandoned, want) {
			t.Fatalf("a wedge in the first stage should abandon %q; abandoned=%v. "+
				"A starved stage reported as an overrun is a cascade that reads as "+
				"three independent slow stages.", want, abandoned)
		}
	}
	if !contains(order, "nats") {
		t.Fatalf("the NATS drain must still run even when every drain stage is starved; order=%v", order)
	}
}

// TestTheAbandonSignalDoesNotLieAboutStagesThatRan pins the two properties that
// make `shutdown_stage_overran` worth the runbook telling operators to watch it.
//
// awaitStage's select has BOTH cases ready whenever the budget is already gone,
// and Go then picks at random -- so the first version reported a stage as having
// overrun when it had completed. Measured at 20000/20000, because the spawned
// goroutine is usually not scheduled before the select runs, which also means
// preferring `done` in a nested select (the obvious fix) moves the number to
// 19995/20000 and settles nothing on its own.
//
// The resolution is that those stages are not overrunning, they are STARVED:
// an earlier stage drained the shared pot and they never got any budget. That
// is a true statement about them, and a different one from "this stage is slow",
// which is the diagnosis an operator would otherwise reach about two innocent
// stages.
func TestTheAbandonSignalDoesNotLieAboutStagesThatRan(t *testing.T) {
	const n = 2000

	t.Run("a stage with budget that completes is never reported", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		reports := 0
		for i := 0; i < n; i++ {
			awaitStage(ctx, "probe", func() {}, func(string, bool) { reports++ })
		}
		if reports != 0 {
			t.Fatalf("%d/%d completed stages were reported as abandoned despite having "+
				"budget; every one is a false shutdown_stage_overran in production", reports, n)
		}
	})

	t.Run("an exhausted budget reports starved, never overran", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done() // the pot is definitively empty before any stage arrives

		var starved, overran int
		for i := 0; i < n; i++ {
			ran := make(chan struct{})
			awaitStage(ctx, "probe", func() { close(ran) }, func(_ string, s bool) {
				if s {
					starved++
				} else {
					overran++
				}
			})
			<-ran // the stage's work DID finish, just not before we stopped waiting
		}
		if overran != 0 {
			t.Fatalf("%d/%d starved stages were labelled overran. They had zero budget and "+
				"never ran; calling them slow points the operator at the wrong stage.", overran, n)
		}
		if starved == 0 {
			t.Fatal("no stage was reported at all on an exhausted budget, so the cascade " +
				"that TestAnEarlyWedgeStarvesTheStagesAfterIt describes would be invisible")
		}
	})
}
