package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// injectedAudienceLatency is the per-call stall the fake audience computer
// simulates. The real ComputePresenceAudience issues FOUR sequential round trips
// (friendsOf, the friends-of-friends flag read, friendsOfFriendsOf,
// serverPeersOf), so this stands in for their sum, not for one of them.
const injectedAudienceLatency = 2 * time.Millisecond

// benchPresenceTransitions is the churn depth per iteration — the number of
// distinct users transitioning, which is also the coalescer's goroutine bound.
const benchPresenceTransitions = 200

// BenchmarkPresenceChurnRunLoopOccupancy measures RUN-LOOP OCCUPANCY, not
// end-to-end delivery latency. #1654 slightly INCREASES end-to-end latency (one
// channel hop plus one Run turn) while collapsing loop occupancy, and a
// flattering write-up would quietly report the wrong one. Read the number as
// "how long the Run goroutine is unavailable for unrelated channel and DM
// delivery."
//
// Both arms drive REAL production functions — broadcastPresenceToAllSync is the
// pre-#1654 path, retained for shutdown, and broadcastPresenceToAll is the
// dispatcher — so this is a before/after of the same code rather than a
// comparison against a reconstruction of it.
//
// The audience computer is a latency-injected fake rather than a real Postgres
// on purpose: the claim under test is loop occupancy, and a real-DB benchmark
// measures Postgres. Any artifact quoting these numbers must state the injected
// latency and the transition count, both of which are named constants above.
//
// CI never passes -bench (there is no -bench in .github/workflows/), so this
// file is compiled by the normal test run but the benchmark itself is
// developer-run. Do not expect a CI gate on the numbers.
func BenchmarkPresenceChurnRunLoopOccupancy(b *testing.B) {
	slowComputer := func(ctx context.Context, _ uuid.UUID) (map[uuid.UUID]bool, error) {
		select {
		case <-time.After(injectedAudienceLatency):
			return map[uuid.UUID]bool{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	newBenchHub := func() (*Hub, []uuid.UUID) {
		hub := NewHub(nil, nil)
		// Non-nil so the dispatcher reaches its dispatch path; never queried,
		// because presenceAudienceComputer intercepts first.
		hub.db = &sql.DB{}
		hub.presenceAudienceComputer = slowComputer
		users := make([]uuid.UUID, benchPresenceTransitions)
		for i := range users {
			users[i] = uuid.New()
		}
		return hub, users
	}

	// Pre-#1654: the query runs inline, so Run is occupied for its full duration
	// on every transition. Expect roughly transitions x injectedAudienceLatency.
	// Setup is excluded from the timed region here for the same reason the async
	// arm excludes its drain: the two arms are reported as a RATIO, so anything
	// timed in one and not the other silently biases it.
	b.Run("sync", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			hub, users := newBenchHub()
			b.StartTimer()

			for _, userID := range users {
				hub.broadcastPresenceToAllSync(userID, statusOnline, 1)
			}
		}
	})

	// Post-#1654: Run bumps a generation, schedules the debounce and spawns a
	// worker. Expect orders of magnitude less.
	//
	// The drain is NOT optional and must stay inside StopTimer/StartTimer. Each
	// iteration dispatches benchPresenceTransitions workers; without draining
	// them, workers from earlier iterations keep sleeping and scheduling while
	// later iterations are being timed, so the reported figure measures
	// accumulated background load rather than one iteration's dispatch cost —
	// which is precisely the "benchmark measures the fake, not the fix" failure
	// this artifact exists to avoid.
	// Post-#1654: Run bumps a generation, schedules the debounce and spawns a
	// worker — and later takes a SECOND turn per transition to apply the result.
	//
	// BOTH turns are timed, and the wait between them is not. An earlier version
	// timed only the dispatch and drained results outside the timer, discarding
	// them rather than applying them, so the async arm never paid for the fence
	// check, the fan-out or the redial while the sync arm paid its full fan-out.
	// §6 defines this artifact as the time for Run to DRAIN the queued work, so
	// that shape inflated the published ratio. Found by Codex on PR #2975 — the
	// second contamination of this benchmark, after the missing cross-iteration
	// drain. Do not move the apply loop back outside the timed region.
	b.Run("async", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			hub, users := newBenchHub()
			b.StartTimer()

			// Run turn 1: dispatch.
			for _, userID := range users {
				hub.broadcastPresenceToAll(userID, statusOnline, 1)
			}

			// Waiting for the workers is OFF Run — it is exactly the latency this
			// change moves off the loop — so it must not be charged to occupancy.
			b.StopTimer()
			results := make([]presenceAudienceResult, 0, len(users))
			for range users {
				results = append(results, <-hub.presenceAudienceResults)
			}
			b.StartTimer()

			// Run turns 2..N+1: apply. Fence check, fan-out, redial.
			for _, result := range results {
				hub.applyPresenceAudience(result)
			}
		}
	})
}

// BenchmarkPresenceChurnCoalescedBurst measures the reconnect-storm shape the
// maintainer comment on #1654 asks about: many transitions concentrated on FEW
// users, which is what a flapping connection produces. Per-user in-flight
// coalescing should make Run occupancy independent of the burst depth.
func BenchmarkPresenceChurnCoalescedBurst(b *testing.B) {
	const senders = 8
	const burstPerSender = 25

	computer := func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{}, nil
	}

	for b.Loop() {
		b.StopTimer()
		hub := NewHub(nil, nil)
		hub.db = &sql.DB{}
		hub.presenceAudienceComputer = computer
		users := make([]uuid.UUID, senders)
		for i := range users {
			users[i] = uuid.New()
		}
		b.StartTimer()

		for range burstPerSender {
			for _, userID := range users {
				hub.broadcastPresenceToAll(userID, statusOnline, 1)
			}
		}

		// Drain to quiescence, applying each result so the coalescer's re-dials also
		// complete. Two separate reasons the shape is what it is:
		//
		// The drain must happen AT ALL, or workers from earlier iterations run during
		// later timed ones and the figure reflects accumulated load rather than one
		// burst's cost.
		//
		// And applyPresenceAudience must be INSIDE the timed region, because it is Run
		// work — fence check, fan-out, redial — and this benchmark claims to measure
		// Run occupancy. Excluding it was the same defect Codex found in the
		// occupancy benchmark, repeated one artifact later.
		//
		// Unlike that benchmark, the RECEIVE is timed here too, and deliberately:
		// this fixture's computer injects NO latency (unlike injectedAudienceLatency
		// in the occupancy benchmark), so the receive is a bare channel handoff worth
		// nanoseconds. Toggling the timer per iteration to exclude it was measured
		// and made things WORSE — the toggle overhead dominated and the figure spread
		// 4x across repeats (87us / 155us / 374us) where it is otherwise stable.
		for len(hub.presenceInFlight) > 0 {
			hub.applyPresenceAudience(<-hub.presenceAudienceResults)
		}
	}
}

// benchReconnectCustomStatusSenders is M in "reconnect with M active Custom
// Status senders" (spec §6 / A9). Kept well under clientBootstrapFrameLimit
// (256): each sender contributes one replay frame and one live presence frame,
// so M=50 fills 100 of the 256 slots and the bootstrap buffer never overflows
// into disconnectPrivacyCriticalClient — which would measure the overflow path
// instead of the reconnect path.
const benchReconnectCustomStatusSenders = 50

// BenchmarkPresenceReconnectWithCustomStatusSenders is the second DoD artifact
// required by A9: reconnect cost with M active Custom Status senders, on the
// #1234 path. It exists because #1654 is the change most able to regress it —
// base presence now arrives at a reconnecting client ASYNCHRONOUSLY, so frames
// that used to be delivered inline can now land mid-bootstrap and must be
// buffered and replayed in order (§4.6 / T7f).
//
// What is measured is END-TO-END RECONNECT LATENCY, which is deliberately NOT
// the terms of BenchmarkPresenceChurnRunLoopOccupancy. That artifact excludes the
// off-Run wait because Run occupancy is its whole question; this one asks §6/A9's
// question instead — does a reconnect carrying M active Custom Status senders get
// SLOWER — and the viewer's bootstrap cannot complete until those audience results
// land, so the wait is part of the answer. Excluding it here made the arms
// asymmetric (the sync baseline pays every injected query inline) and, worse,
// structurally unable to show the regression A9 exists to detect: the async path
// adds a channel hop and a Run turn, and a benchmark that never times the wait
// reports that as a pure win. Codex caught it on PR #2975.
//
// The honest comparison is therefore serial-vs-concurrent query cost: the sync arm
// pays M injected latencies in sequence, the async arm pays about M divided by
// presenceAudienceConcurrency. A ratio near that bound is the expected result; a
// ratio in the hundreds means the wait escaped the timer again.
//
// What is NOT measured, stated plainly because a benchmark that overstates its
// own scope is worse than none: the real sendCustomTextSnapshot performs a
// per-sender database read, and per §6 the instrument here must be a
// latency-injected fake rather than Postgres. appendBootstrapReplay therefore
// stands in for that function's OUTPUT — one Custom Status replay frame per
// sender — not for its query cost. Every other call is the production path.
func BenchmarkPresenceReconnectWithCustomStatusSenders(b *testing.B) {
	customFrame := []byte(`{"type":"custom_text","data":{"text":"benchmark"}}`)
	snapshot := []byte(`{"type":"presence_snapshot"}`)

	// Same latency-injected fake as the occupancy benchmark, so the two arms here
	// are comparable with each other AND with that artifact.
	type reconnectFixture struct {
		hub     *Hub
		viewer  *Client
		senders []uuid.UUID
	}
	setup := func(b *testing.B) reconnectFixture {
		b.Helper()
		hub := NewHub(nil, nil)
		hub.db = &sql.DB{}

		senders := make([]uuid.UUID, benchReconnectCustomStatusSenders)
		for i := range senders {
			senders[i] = uuid.New()
		}
		viewerID := uuid.New()
		viewer := &Client{ID: uuid.New(), UserID: viewerID, Send: make(chan []byte, 512), Hub: hub}
		viewer.beginBootstrap()
		hub.clients[viewer.ID] = viewer
		hub.userClients[viewerID] = map[uuid.UUID]bool{viewer.ID: true}

		// The viewer MUST be in every sender's audience. An empty audience is not a
		// smaller version of this benchmark — it is a different one:
		// deliverPresenceAudienceResult adds only the sender itself, so the viewer
		// would receive no live presence frame, bufferBootstrapLive would never run,
		// and the measured flush would contain the Custom Status replay and nothing
		// else. TestReconnectBenchmarkExercisesTheLivePresencePath locks this.
		hub.presenceAudienceComputer = func(ctx context.Context, _ uuid.UUID) (map[uuid.UUID]bool, error) {
			select {
			case <-time.After(injectedAudienceLatency):
				return map[uuid.UUID]bool{viewerID: true}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return reconnectFixture{hub: hub, viewer: viewer, senders: senders}
	}
	drain := func(viewer *Client) {
		for len(viewer.Send) > 0 {
			<-viewer.Send
		}
	}

	// BASELINE — the pre-#1654 reconnect. broadcastPresenceToAllSync is the
	// retained synchronous path, so Run pays each audience query inline while the
	// viewer is mid-bootstrap. Without this arm the async number below is a lone
	// figure that cannot establish whether reconnect improved or regressed, which
	// is what A9 actually asks.
	b.Run("sync", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			f := setup(b)
			b.StartTimer()

			for _, senderID := range f.senders {
				if err := f.viewer.appendBootstrapReplay(customFrame); err != nil {
					b.Fatalf("bootstrap replay rejected: %v", err)
				}
				f.hub.broadcastPresenceToAllSync(senderID, statusOnline, 1)
			}
			if !f.hub.completeClientBootstrap(f.viewer, snapshot) {
				b.Fatal("bootstrap completion failed; the benchmark measured the wrong path")
			}

			b.StopTimer()
			drain(f.viewer)
			b.StartTimer()
		}
	})

	// POST-CHANGE — dispatch turn, the wait for the workers, apply turn, and the
	// same flush. The wait is TIMED here, unlike the occupancy benchmark: see the
	// header. Keep it inside the timer.
	b.Run("async", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			f := setup(b)
			b.StartTimer()

			for _, senderID := range f.senders {
				if err := f.viewer.appendBootstrapReplay(customFrame); err != nil {
					b.Fatalf("bootstrap replay rejected: %v", err)
				}
				f.hub.broadcastPresenceToAll(senderID, statusOnline, 1)
			}

			results := make([]presenceAudienceResult, 0, len(f.senders))
			for range f.senders {
				results = append(results, <-f.hub.presenceAudienceResults)
			}

			for _, result := range results {
				f.hub.applyPresenceAudience(result)
			}
			if !f.hub.completeClientBootstrap(f.viewer, snapshot) {
				b.Fatal("bootstrap completion failed; the benchmark measured the wrong path")
			}

			b.StopTimer()
			drain(f.viewer)
			b.StartTimer()
		}
	})
}

// TestReconnectBenchmarkExercisesTheLivePresencePath is a regression lock ON THE
// BENCHMARK, and it exists because the benchmark shipped measuring the wrong
// thing while still producing a plausible number.
//
// The first version handed the fake computer an EMPTY audience.
// deliverPresenceAudienceResult adds only the sender itself, so the reconnecting
// viewer was in nobody's audience, received no live presence frame,
// bufferBootstrapLive never ran, and the "snapshot -> Custom Status replay ->
// live presence" flush the benchmark documents itself as measuring contained
// only the replay. Nothing failed; the number simply meant something else.
//
// A benchmark cannot assert, and CI never passes -bench, so this test is the
// only thing standing between that mistake and a repeat of it. It drives the
// same construction the benchmark does and counts frames by kind.
func TestReconnectBenchmarkExercisesTheLivePresencePath(t *testing.T) {
	const senders = 5

	hub := NewHub(nil, nil)
	hub.db = &sql.DB{}
	viewerID := uuid.New()
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{viewerID: true}, nil
	}

	viewer := &Client{ID: uuid.New(), UserID: viewerID, Send: make(chan []byte, 64), Hub: hub}
	viewer.beginBootstrap()
	hub.clients[viewer.ID] = viewer
	hub.userClients[viewerID] = map[uuid.UUID]bool{viewer.ID: true}

	for range senders {
		require.NoError(t, viewer.appendBootstrapReplay([]byte(`{"type":"custom_text"}`)))
		hub.broadcastPresenceToAll(uuid.New(), statusOnline, 1)
	}
	for range senders {
		hub.applyPresenceAudience(<-hub.presenceAudienceResults)
	}
	require.True(t, hub.completeClientBootstrap(viewer, []byte(`{"type":"presence_snapshot"}`)))

	byType := map[string]int{}
	for len(viewer.Send) > 0 {
		var frame map[string]interface{}
		require.NoError(t, json.Unmarshal(<-viewer.Send, &frame))
		kind, ok := frame["type"].(string)
		require.True(t, ok, "frame carries no type: %#v", frame)
		byType[kind]++
	}

	assert.Equal(t, 1, byType["presence_snapshot"], "the bootstrap snapshot must be flushed first")
	assert.Equal(t, senders, byType["custom_text"], "one Custom Status replay frame per sender")
	assert.Equal(t, senders, byType["presence"],
		"one LIVE presence frame per sender — zero here means the viewer is not in the "+
			"fake audience and the benchmark is measuring the replay path alone")
}
