package voice

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Issue #2868 -- a dispatcher overflow drops the event and reports it; it never
// disconnects every Rich Presence client on the replica.
//
// Every fill below drives ONE of the three overflow triggers to its boundary
// WITHOUT crossing it, so the only drops a subtest sees are the deliberate ones.
// That is what makes the exact-delta assertion in clause (b) meaningful: a
// ">= 1 drop" assertion would be satisfied by a fill that overflowed by accident.

// dispatchOverflowSaturatedRoom is the room fillOneRoomQueue saturates. The
// per-room arm only fires for the room that is actually full, so that trigger's
// deliberate overflows must target this key rather than a fresh one.
const dispatchOverflowSaturatedRoom = "aaaaaaaa-0000-4000-8000-000000000001"

// parkedDispatcher returns a dispatcher whose every worker is blocked inside the
// handler.
//
// Parking is what makes the fills deterministic. With a free worker a room is
// drained while it is being filled, so the queue length at the boundary depends
// on scheduling and the fill either under- or over-shoots its trigger. Each room
// admits at most one active worker, so one event on each of
// voiceLifecycleDispatchWorkerCount distinct keys parks the whole pool.
func parkedDispatcher(
	t *testing.T,
	overflow func(voiceLifecycleDropCounts),
) *voiceLifecycleDispatcher {
	t.Helper()
	blocked := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked) }) }
	started := make(chan struct{}, voiceLifecycleDispatchWorkerCount)
	dispatcher := newVoiceLifecycleDispatcher(
		func(_ string, _ []byte) {
			// Non-blocking: after release the parked workers drain the fill
			// through this same handler, and a blocking send on a full channel
			// nobody is reading would deadlock close().
			select {
			case started <- struct{}{}:
			default:
			}
			<-blocked
		},
		overflow,
	)
	// close() joins the workers, so they must be released first. Cleanups run
	// last-registered-first, hence close is registered before release.
	t.Cleanup(dispatcher.close)
	t.Cleanup(release)
	blockAllVoiceLifecycleWorkers(t, dispatcher, started)
	return dispatcher
}

// dispatchOverflowResidentRooms is the room-map residency the parked workers
// themselves hold: a room stays resident while a worker is active on it, so
// every fill subtracts it to land exactly on the boundary.
const dispatchOverflowResidentRooms = voiceLifecycleDispatchWorkerCount

// fillDistinctRooms occupies every remaining room slot. roomForEnqueueLocked
// overflows a NEW key once len(d.rooms) >= voiceLifecycleDispatchRoomCount.
func fillDistinctRooms(dispatcher *voiceLifecycleDispatcher) {
	for index := range voiceLifecycleDispatchRoomCount - dispatchOverflowResidentRooms {
		dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload(
			fmt.Sprintf("00000000-0000-4000-8000-%012d", index), 0))
	}
}

// fillOneRoomQueue saturates ONE room to its per-room cap. voice.joined, so the
// queue tail is never a heartbeat and coalescePendingVoiceHeartbeat cannot
// absorb a deliberate heartbeat overflow instead of dropping it.
func fillOneRoomQueue(dispatcher *voiceLifecycleDispatcher) {
	for index := range voiceLifecycleDispatchRoomLimit {
		dispatcher.enqueue(natsSubjectVoiceJoined,
			lifecycleDispatchPayload(dispatchOverflowSaturatedRoom, index))
	}
}

// fillTotalPending drives d.pending to its global ceiling across many rooms that
// each stay well under the per-room cap, so the total-pending arm is the one at
// its boundary rather than the per-room arm.
func fillTotalPending(dispatcher *voiceLifecycleDispatcher) {
	const perRoom = 32                                  // safely under voiceLifecycleDispatchRoomLimit (64)
	rooms := voiceLifecycleDispatchTotalLimit / perRoom // 128, safely under 1024
	for room := range rooms {
		for index := range perRoom {
			dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload(
				fmt.Sprintf("10000000-0000-4000-8000-%012d", room), index))
		}
	}
}

// overflowNEvents enqueues n events past the boundary and returns n.
func overflowNEvents(
	dispatcher *voiceLifecycleDispatcher,
	subject string,
	roomKey func(index int) string,
	count uint64,
) uint64 {
	for index := range count {
		dispatcher.enqueue(subject, lifecycleDispatchPayload(roomKey(int(index)), 0))
	}
	return count
}

func freshOverflowRoom(index int) string {
	return fmt.Sprintf("ffffffff-0000-4000-8000-%012d", index)
}

func saturatedOverflowRoom(int) string { return dispatchOverflowSaturatedRoom }

// waitForCountDelta asserts EXACTLY want additional drops were counted since
// baseline. Delta rather than absolute, so a fill that incidentally overflowed
// cannot make the assertion pass for the wrong reason.
func waitForCountDelta(t *testing.T, counted *atomic.Uint64, baseline, want uint64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if counted.Load()-baseline == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("counted %d additional drops, want exactly %d",
				counted.Load()-baseline, want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestDispatcherOverflowDisconnectsNoClients(t *testing.T) {
	for _, trigger := range []struct {
		name    string
		fill    func(dispatcher *voiceLifecycleDispatcher)
		roomKey func(index int) string
	}{
		{"room count 1024", fillDistinctRooms, freshOverflowRoom},
		{"per-room queue 64", fillOneRoomQueue, saturatedOverflowRoom},
		{"total pending 4096", fillTotalPending, freshOverflowRoom},
	} {
		for _, subject := range []string{
			natsSubjectVoiceHeartbeat, natsSubjectVoiceJoined,
			natsSubjectVoiceLeft, natsSubjectVoiceRoomEmpty,
		} {
			t.Run(trigger.name+"/"+subject, func(t *testing.T) {
				var disconnects atomic.Int32
				subscriber := &NATSSubscriber{
					log:                                  logger.New("test"),
					disconnectAllRichPresenceClientsHook: func() { disconnects.Add(1) },
				}
				var counted atomic.Uint64
				dispatcher := parkedDispatcher(t, func(counts voiceLifecycleDropCounts) {
					// The report runs BEFORE the counter is published, so an
					// observed delta proves the callback returned. Counting first
					// would let clause (a) read disconnects while the callback was
					// still on its way to the sink it must never reach.
					subscriber.handleVoiceLifecycleDispatchOverflow(counts)
					for _, dropped := range counts {
						counted.Add(dropped)
					}
				})

				trigger.fill(dispatcher)
				baseline := counted.Load()
				require.Zero(t, baseline,
					"the fill must sit AT its trigger boundary, never past it; a "+
						"fill that overflows by itself makes clause (b) meaningless")
				overflowed := overflowNEvents(dispatcher, subject, trigger.roomKey, 3)

				// (b) MANDATORY: the drops were actually counted. Without this the
				// test passes trivially if nothing happened at all. Delta from the
				// post-fill baseline, so a fill that incidentally overflowed cannot
				// satisfy the assertion on the wrong events.
				waitForCountDelta(t, &counted, baseline, overflowed)
				// (a) the fleet-wide sink is never reached.
				if got := disconnects.Load(); got != 0 {
					t.Fatalf("%d fleet disconnects; want 0", got)
				}
			})
		}
	}
}

// The report is per CLASS and aggregated, never one line per dropped message. A
// per-message report is itself an amplification primitive -- the same reasoning
// that put ingressShedState in front of the gate's own sheds.
func TestVoiceDropShedAggregatesAndNamesClasses(t *testing.T) {
	var observed []string
	logged := 0
	subscriber := &NATSSubscriber{
		log:                     logger.New("test"),
		ingressShedObservedHook: func(class string) { observed = append(observed, class) },
		ingressShedLoggedHook:   func() { logged++ },
	}

	var counts voiceLifecycleDropCounts
	counts[voiceLifecycleDropConvergent] = 5
	counts[voiceLifecycleDropTerminal] = 2
	subscriber.handleVoiceLifecycleDispatchOverflow(counts)

	require.Len(t, observed, 7,
		"one observed drop per counted message, across both classes: %v", observed)
	require.Equal(t, 1, logged,
		"exactly one report: convergent arms silently, the terminal first-report\n\t\tflushes both classes in one due map. LessOrEqual would also pass on ZERO,\n\t\twhich is the opposite defect from the one this test names")
	require.Contains(t, observed, "dispatch_drop_convergent")
	require.Contains(t, observed, "dispatch_drop_terminal")
	require.NotContains(t, observed, "resource_limit",
		"resource_limit is retired and must not be emitted")
}

// The EMISSION arm. Asserts the log output rather than ingressShedLoggedHook,
// because a hook proves the seam ran and never that the lines after it were
// written -- the finding recorded on TestReportShedWritesAnAggregatedLogLine.
func TestVoiceDropReportWritesAnAggregatedLogLine(t *testing.T) {
	var sink bytes.Buffer
	subscriber := &NATSSubscriber{log: logger.NewWithWriter(&sink)}

	var convergent voiceLifecycleDropCounts
	convergent[voiceLifecycleDropConvergent] = 5
	subscriber.handleVoiceLifecycleDispatchOverflow(convergent)
	require.Empty(t, sink.String(), "the arming report does not itself emit")

	subscriber.voiceDropShedState.mu.Lock()
	subscriber.voiceDropShedState.loggedAt = time.Now().Add(-2 * ingressShedLogInterval)
	subscriber.voiceDropShedState.mu.Unlock()

	var terminal voiceLifecycleDropCounts
	terminal[voiceLifecycleDropTerminal] = 2
	subscriber.handleVoiceLifecycleDispatchOverflow(terminal)

	out := sink.String()
	require.Contains(t, out, voiceDropShedMessage)
	require.Contains(t, out, "failure_class=dispatch_drop_convergent")
	require.Contains(t, out, "dropped=5",
		"the flush carries the drops that armed the window, not just the last one")
	require.Contains(t, out, "failure_class=dispatch_drop_terminal")
	require.Contains(t, out, "dropped=2")
	// Severity is per reported class, so one flush carries both levels.
	require.Contains(t, out, "level=WARN")
	require.Contains(t, out, "level=ERROR")
	require.NotContains(t, out, "resource_limit")
	require.NotContains(t, out, "disconnect_all_presence_clients")
}

// resource_limit is RETIRED, not reused: nats.go held its only producer, and
// removing that producer is the whole of #2868. A source pin, because "a value
// is never emitted again" cannot be proven by exercising paths.
func TestRetiredResourceLimitHasNoProducer(t *testing.T) {
	// Walks EVERY non-test file in the package, not just nats.go. The drop classes
	// are emitted from ingress_gate.go, so that is exactly where a future author
	// would reintroduce the retired name -- and a nats.go-only pin would stay green.
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		//nolint:gosec // G304: name is an entry of this package's own directory, filtered to *.go, never user input
		source, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		require.NotContains(t, string(source), `"resource_limit"`,
			"%s: failure_class is a closed vocabulary; resource_limit is retired with "+
				"no producer, and any alerting keyed on it is expected to be silent", name)
		checked++
	}
	require.NotZero(t, checked, "the pin scanned no files -- it cannot fail, so it proves nothing")
}

// A terminal drop has NO successor event, so swallowing its first occurrence is
// not a delay -- it is permanent silence. Reporting is shed-driven rather than
// time-driven, so a bounded burst that ends is never flushed by anything later.
// Before #2868 the overflow arm logged unconditionally on every wakeup; retiring
// the teardown without this would have traded a disproportionate response for a
// silent one (CWE-778). Found by an adversarial pass on the fix itself.
func TestVoiceDropTerminalIsAudibleOnItsFirstOccurrence(t *testing.T) {
	var sink bytes.Buffer
	subscriber := &NATSSubscriber{log: logger.NewWithWriter(&sink)}

	var terminal voiceLifecycleDropCounts
	terminal[voiceLifecycleDropTerminal] = 1
	subscriber.handleVoiceLifecycleDispatchOverflow(terminal)

	out := sink.String()
	require.NotEmpty(t, out,
		"the FIRST terminal drop must emit; the arming window would swallow it forever")
	require.Contains(t, out, "failure_class=dispatch_drop_terminal")
	require.Contains(t, out, "dropped=1")
}

// The counterpart, and the reason the fix is class-SELECTIVE: a convergent drop
// repairs itself on the next heartbeat tick, so its first occurrence may still
// arm the window silently. Emitting for every class would hand the overflow
// worker one log write per wakeup -- the amplification the aggregation exists to
// prevent, recreated in the log.
func TestVoiceDropConvergentStillArmsSilently(t *testing.T) {
	var sink bytes.Buffer
	subscriber := &NATSSubscriber{log: logger.NewWithWriter(&sink)}

	var convergent voiceLifecycleDropCounts
	convergent[voiceLifecycleDropConvergent] = 5
	subscriber.handleVoiceLifecycleDispatchOverflow(convergent)

	require.Empty(t, sink.String(),
		"a convergent first drop arms the interval rather than emitting")
}

// After the first terminal report the class returns to interval gating, so a
// sustained forged burst buys one line, not one line per wakeup. voice.room_empty
// is forgeable on the unauthenticated bus and is NOT metered by G2, so this is
// the property that keeps the fix from being an amplification primitive.
func TestVoiceDropTerminalIsIntervalGatedAfterTheFirst(t *testing.T) {
	var sink bytes.Buffer
	subscriber := &NATSSubscriber{log: logger.NewWithWriter(&sink)}

	var terminal voiceLifecycleDropCounts
	terminal[voiceLifecycleDropTerminal] = 1
	subscriber.handleVoiceLifecycleDispatchOverflow(terminal)
	firstLen := sink.Len()
	require.NotZero(t, firstLen)

	for range 50 {
		subscriber.handleVoiceLifecycleDispatchOverflow(terminal)
	}
	require.Equal(t, firstLen, sink.Len(),
		"50 further terminal drops inside the interval must add no log output")
}
