package voice

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lifecycleDispatchPayload(roomID string, sequence int) []byte {
	payload, _ := json.Marshal(map[string]interface{}{
		"channelId": roomID,
		"sequence":  sequence,
	})
	return payload
}

func lifecycleDispatchSequence(t *testing.T, data []byte) int {
	t.Helper()
	var payload struct {
		Sequence int `json:"sequence"`
	}
	require.NoError(t, json.Unmarshal(data, &payload))
	return payload.Sequence
}

func TestVoiceLifecycleDispatcherPreservesPerRoomOrder(t *testing.T) {
	var mu sync.Mutex
	got := make([]int, 0, 3)
	done := make(chan struct{})
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte, _ time.Time) {
		mu.Lock()
		got = append(got, lifecycleDispatchSequence(t, data))
		if len(got) == 3 {
			close(done)
		}
		mu.Unlock()
	}, func(voiceLifecycleDropCounts) {})
	t.Cleanup(dispatcher.close)

	roomID := "same-room"
	dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload(roomID, 1))
	dispatcher.enqueue(natsSubjectVoiceLeft, lifecycleDispatchPayload(roomID, 2))
	dispatcher.enqueue(natsSubjectVoiceRoomEmpty, lifecycleDispatchPayload(roomID, 3))

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ordered lifecycle dispatch")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{1, 2, 3}, got)
}

func TestVoiceLifecycleDispatcherRunsIndependentRoomsConcurrently(t *testing.T) {
	blocked := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked) }) }
	started := make(chan struct{})
	independentDone := make(chan struct{})
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte, _ time.Time) {
		var payload struct {
			ChannelID string `json:"channelId"`
		}
		_ = json.Unmarshal(data, &payload)
		if payload.ChannelID == "blocked-room" {
			close(started)
			<-blocked
			return
		}
		close(independentDone)
	}, func(voiceLifecycleDropCounts) {})
	t.Cleanup(dispatcher.close)
	t.Cleanup(release)

	dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload("blocked-room", 1))
	<-started
	dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload("independent-room", 1))
	select {
	case <-independentDone:
	case <-time.After(time.Second):
		t.Fatal("independent room was blocked behind unrelated lifecycle work")
	}
	release()
}

func blockAllVoiceLifecycleWorkers(
	t *testing.T,
	dispatcher *voiceLifecycleDispatcher,
	started <-chan struct{},
) {
	t.Helper()
	for index := range voiceLifecycleDispatchWorkerCount {
		dispatcher.enqueue(
			natsSubjectVoiceJoined,
			lifecycleDispatchPayload(fmt.Sprintf("blocker-%d", index), 0),
		)
	}
	for range voiceLifecycleDispatchWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out blocking lifecycle dispatcher workers")
		}
	}
}

func TestVoiceLifecycleDispatcherCoalescesOnlyAdjacentPendingHeartbeats(t *testing.T) {
	blocked := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked) }) }
	started := make(chan struct{}, voiceLifecycleDispatchWorkerCount)
	sequences := make(chan int, 4)
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte, _ time.Time) {
		var payload struct {
			ChannelID string `json:"channelId"`
		}
		_ = json.Unmarshal(data, &payload)
		if len(payload.ChannelID) >= len("blocker-") && payload.ChannelID[:len("blocker-")] == "blocker-" {
			started <- struct{}{}
			<-blocked
			return
		}
		sequences <- lifecycleDispatchSequence(t, data)
	}, func(voiceLifecycleDropCounts) {})
	t.Cleanup(dispatcher.close)
	t.Cleanup(release)
	blockAllVoiceLifecycleWorkers(t, dispatcher, started)

	roomID := "heartbeat-room"
	dispatcher.enqueue(natsSubjectVoiceHeartbeat, lifecycleDispatchPayload(roomID, 1))
	dispatcher.enqueue(natsSubjectVoiceHeartbeat, lifecycleDispatchPayload(roomID, 2))
	dispatcher.enqueue(natsSubjectVoiceHeartbeat, lifecycleDispatchPayload(roomID, 3))
	release()

	select {
	case sequence := <-sequences:
		assert.Equal(t, 3, sequence)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced heartbeat")
	}
	select {
	case extra := <-sequences:
		t.Fatalf("unexpected superseded heartbeat %d", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestVoiceLifecycleDispatcherDoesNotCoalesceAcrossRoomBoundary(t *testing.T) {
	blocked := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked) }) }
	started := make(chan struct{}, voiceLifecycleDispatchWorkerCount)
	sequences := make(chan int, 4)
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte, _ time.Time) {
		var payload struct {
			ChannelID string `json:"channelId"`
		}
		_ = json.Unmarshal(data, &payload)
		if len(payload.ChannelID) >= len("blocker-") && payload.ChannelID[:len("blocker-")] == "blocker-" {
			started <- struct{}{}
			<-blocked
			return
		}
		sequences <- lifecycleDispatchSequence(t, data)
	}, func(voiceLifecycleDropCounts) {})
	t.Cleanup(dispatcher.close)
	t.Cleanup(release)
	blockAllVoiceLifecycleWorkers(t, dispatcher, started)

	roomID := "boundary-room"
	dispatcher.enqueue(natsSubjectVoiceHeartbeat, lifecycleDispatchPayload(roomID, 1))
	dispatcher.enqueue(natsSubjectVoiceLeft, lifecycleDispatchPayload(roomID, 2))
	dispatcher.enqueue(natsSubjectVoiceHeartbeat, lifecycleDispatchPayload(roomID, 3))
	release()

	got := make([]int, 0, 3)
	for len(got) < 3 {
		select {
		case sequence := <-sequences:
			got = append(got, sequence)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for boundary-preserving dispatch")
		}
	}
	assert.Equal(t, []int{1, 2, 3}, got)
}

func TestVoiceLifecycleDispatcherSaturatedRoomDoesNotBlockIngress(t *testing.T) {
	blocked := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked) }) }
	overflowBlocked := make(chan struct{})
	var overflowReleaseOnce sync.Once
	releaseOverflow := func() { overflowReleaseOnce.Do(func() { close(overflowBlocked) }) }
	started := make(chan struct{})
	overflowStarted := make(chan struct{}, 2)
	independentDone := make(chan struct{})
	var overflowCount atomic.Int32
	dispatcher := newVoiceLifecycleDispatcher(
		func(_ string, data []byte, _ time.Time) {
			var payload struct {
				ChannelID string `json:"channelId"`
			}
			_ = json.Unmarshal(data, &payload)
			switch payload.ChannelID {
			case "saturated-room":
				select {
				case started <- struct{}{}:
				default:
				}
				<-blocked
			case "independent-after-saturation":
				close(independentDone)
			}
		},
		func(_ voiceLifecycleDropCounts) {
			overflowCount.Add(1)
			overflowStarted <- struct{}{}
			<-overflowBlocked
		},
	)
	t.Cleanup(dispatcher.close)
	t.Cleanup(release)
	t.Cleanup(releaseOverflow)

	dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload("saturated-room", 0))
	<-started
	for index := range voiceLifecycleDispatchRoomLimit + 1 {
		dispatcher.enqueue(
			natsSubjectVoiceJoined,
			lifecycleDispatchPayload("saturated-room", index+1),
		)
	}
	select {
	case <-overflowStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for overflow recovery")
	}
	for index := range 10 {
		dispatcher.enqueue(
			natsSubjectVoiceJoined,
			lifecycleDispatchPayload("saturated-room", index+100),
		)
	}
	dispatcher.enqueue(
		natsSubjectVoiceJoined,
		lifecycleDispatchPayload("independent-after-saturation", 1),
	)
	select {
	case <-independentDone:
	case <-time.After(time.Second):
		t.Fatal("overflow recovery parked ingress for an independent room")
	}
	releaseOverflow()
	select {
	case <-overflowStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced overflow recovery")
	}
	assert.Equal(t, int32(2), overflowCount.Load(),
		"repeated overflow while recovery runs must coalesce to one pending pass")
	release()
}

func TestVoiceLifecycleDispatcherCloseDropsQueuedWork(t *testing.T) {
	blocked := make(chan struct{})
	started := make(chan struct{}, voiceLifecycleDispatchWorkerCount)
	var queuedProcessed atomic.Int32
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte, _ time.Time) {
		var payload struct {
			ChannelID string `json:"channelId"`
		}
		_ = json.Unmarshal(data, &payload)
		if payload.ChannelID == "queued-during-close" {
			queuedProcessed.Add(1)
			return
		}
		started <- struct{}{}
		<-blocked
	}, func(voiceLifecycleDropCounts) {})

	for index := range voiceLifecycleDispatchWorkerCount {
		dispatcher.enqueue(
			natsSubjectVoiceJoined,
			lifecycleDispatchPayload(fmt.Sprintf("active-%d", index), 0),
		)
	}
	for range voiceLifecycleDispatchWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out starting dispatcher workers")
		}
	}
	for index := range voiceLifecycleDispatchRoomLimit {
		dispatcher.enqueue(
			natsSubjectVoiceJoined,
			lifecycleDispatchPayload("queued-during-close", index),
		)
	}

	closed := make(chan struct{})
	go func() {
		dispatcher.close()
		close(closed)
	}()
	require.Eventually(t, func() bool {
		dispatcher.mu.Lock()
		defer dispatcher.mu.Unlock()
		return dispatcher.closed
	}, time.Second, time.Millisecond)
	close(blocked)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("dispatcher close drained queued lifecycle work")
	}
	assert.Zero(t, queuedProcessed.Load())
}

func TestClassifyVoiceLifecycleDrop(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		subject  string
		resolved bool
		want     voiceLifecycleDropClass
	}{
		{"heartbeat is convergent", natsSubjectVoiceHeartbeat, true, voiceLifecycleDropConvergent},
		{"joined is convergent", natsSubjectVoiceJoined, true, voiceLifecycleDropConvergent},
		{"left is convergent", natsSubjectVoiceLeft, true, voiceLifecycleDropConvergent},
		{"room_empty is terminal", natsSubjectVoiceRoomEmpty, true, voiceLifecycleDropTerminal},
		{"unresolved key wins over subject", natsSubjectVoiceHeartbeat, false, voiceLifecycleDropUnresolvable},
		{"unknown subject is terminal", "voice.unknown", true, voiceLifecycleDropTerminal},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := classifyVoiceLifecycleDrop(testCase.subject, testCase.resolved); got != testCase.want {
				t.Fatalf("classifyVoiceLifecycleDrop(%q, %v) = %d, want %d",
					testCase.subject, testCase.resolved, got, testCase.want)
			}
		})
	}
}

func TestVoiceLifecycleDispatchRoomKeyReportsResolution(t *testing.T) {
	key, resolved := voiceLifecycleDispatchRoomKey(
		natsSubjectVoiceJoined, []byte(`{"channelId":"c0ffee00-0000-4000-8000-000000000001"}`))
	if !resolved || key != "c0ffee00-0000-4000-8000-000000000001" {
		t.Fatalf("channelId arm: key=%q resolved=%v", key, resolved)
	}

	// No channelId: the SHA-256 fallback arm. Production-unreachable post-#2871
	// G1, which rejects any event whose channelId is not a canonical UUID. This
	// test is the reason the fallback cannot be deleted (#2757 owns that).
	fallbackKey, fallbackResolved := voiceLifecycleDispatchRoomKey(
		natsSubjectVoiceJoined, []byte(`{}`))
	if fallbackResolved {
		t.Fatal("fallback arm must report resolved=false")
	}
	if fallbackKey == "" {
		t.Fatal("fallback arm must still produce a key")
	}
}

// TestVoiceLifecycleDispatcherCoalescingDoesNotMaskADropClass pins spec R5: while
// an overflow is pending, scheduleOverflowLocked returns early. If counting were
// done at the callback, a terminal drop arriving behind a convergent one would be
// invisible. Counting happens BEFORE the pending check, so both survive.
func TestVoiceLifecycleDispatcherCoalescingDoesNotMaskADropClass(t *testing.T) {
	blocked := make(chan struct{})
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	reported := make(chan voiceLifecycleDropCounts, 4)
	dispatcher := newVoiceLifecycleDispatcher(
		func(_ string, _ []byte, _ time.Time) { <-blocked },
		func(counts voiceLifecycleDropCounts) {
			select {
			case parked <- struct{}{}:
				// First entry only: hold the overflow worker here. nextOverflow
				// has already cleared overflowPending for the primer, and the
				// worker cannot clear it again while it is parked — so the two
				// graded drops below provably arrive under a HELD overflowPending.
				<-release
			default:
			}
			reported <- counts
		},
	)
	t.Cleanup(func() { close(blocked); dispatcher.close() })

	// Fill every room slot so the next distinct key overflows.
	for index := range voiceLifecycleDispatchRoomCount {
		dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload(
			fmt.Sprintf("00000000-0000-4000-8000-%012d", index), 0))
	}

	// Primer overflow, solely to park the overflow worker inside the callback.
	// Without it this test has a vacuity axis: if the worker drains between the
	// two graded drops below, overflowPending is cleared and the second drop is
	// counted even when the increment sits AFTER the pending check — so the test
	// would pass against the very defect it exists to catch. Parking removes the
	// race instead of relying on it being narrow.
	dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload("primer", 0))
	<-parked

	// Two overflows of DIFFERENT classes while overflowPending is held.
	dispatcher.enqueue(natsSubjectVoiceHeartbeat, lifecycleDispatchPayload("overflow-a", 0))
	dispatcher.enqueue(natsSubjectVoiceRoomEmpty, lifecycleDispatchPayload("overflow-b", 0))
	close(release)

	// Expected: primer + overflow-a are convergent (2), overflow-b is terminal (1).
	// With the increment after the pending check, overflow-b is masked entirely
	// and terminal stays 0.
	var total voiceLifecycleDropCounts
	deadline := time.After(5 * time.Second)
	for total[voiceLifecycleDropConvergent] < 2 || total[voiceLifecycleDropTerminal] < 1 {
		select {
		case counts := <-reported:
			for class := range counts {
				total[class] += counts[class]
			}
		case <-deadline:
			t.Fatalf("classes masked by coalescing: %v", total)
		}
	}
	if total[voiceLifecycleDropConvergent] != 2 || total[voiceLifecycleDropTerminal] != 1 {
		t.Fatalf("counts not exact: %v", total)
	}
}

// #3205. receivedAt must be the NATS RECEIPT stamp, taken on the callback
// goroutine, and never a stamp taken when a worker finally drains the event.
//
// The discrimination is the blocked worker: event B is enqueued while the room's
// only worker is parked inside event A's handler, so B's drain provably happens
// after enqueuedB was read. A drain-time stamp is therefore strictly greater
// than enqueuedB, and an enqueue-time stamp is never greater than it. Nothing
// here depends on how LONG the worker is parked -- only on the ordering, which
// the channel handshake makes total.
func TestVoiceLifecycleDispatcherStampsReceiptTimeAtEnqueue(t *testing.T) {
	var mu sync.Mutex
	stamps := make([]time.Time, 0, 2)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	dispatcher := newVoiceLifecycleDispatcher(
		func(_ string, _ []byte, receivedAt time.Time) {
			mu.Lock()
			stamps = append(stamps, receivedAt)
			first := len(stamps) == 1
			mu.Unlock()
			entered <- struct{}{}
			if first {
				<-release
			}
		},
		func(voiceLifecycleDropCounts) {})
	t.Cleanup(dispatcher.close)

	const roomID = "receipt-stamp-room"
	before := time.Now()
	dispatcher.enqueue(natsSubjectVoiceJoined, lifecycleDispatchPayload(roomID, 1))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first handler to park")
	}

	// Same room, so the parked worker owns it and B cannot be drained until the
	// release below.
	dispatcher.enqueue(natsSubjectVoiceLeft, lifecycleDispatchPayload(roomID, 2))
	enqueuedB := time.Now()
	close(release)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the second handler")
	}
	after := time.Now()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, stamps, 2)
	require.False(t, stamps[0].Before(before), "receipt time predates the enqueue call")
	require.False(t, stamps[1].After(after), "receipt time postdates the drain")
	require.False(t, stamps[1].After(enqueuedB),
		"the second event was stamped when it DRAINED, not when it was received: "+
			"dispatcher lateness would be carried into the clamp as a stamp advance")
}

// coalescePendingVoiceHeartbeat replaces the whole event struct, so the newer
// receipt time rides along for free today. Pinned so a refactor that copies
// fields individually cannot silently strand the older stamp on a live room --
// an older receipt clamps HARDER, which is the direction that loses a
// comparison the event should have won (#3205).
func TestCoalescedHeartbeatCarriesTheNewerReceiptTime(t *testing.T) {
	older := time.Now().Add(-time.Minute)
	room := &voiceLifecycleDispatchRoom{
		queue: []voiceLifecycleDispatchEvent{{
			subject:    natsSubjectVoiceHeartbeat,
			data:       lifecycleDispatchPayload("coalesce-room", 1),
			receivedAt: older,
		}},
	}
	incoming := voiceLifecycleDispatchEvent{
		subject:    natsSubjectVoiceHeartbeat,
		data:       lifecycleDispatchPayload("coalesce-room", 2),
		receivedAt: time.Now(),
	}

	require.True(t, coalescePendingVoiceHeartbeat(room, incoming))
	require.Equal(t, incoming, room.queue[0])
	require.True(t, room.queue[0].receivedAt.After(older),
		"a coalesced heartbeat must carry the NEWER receipt time")
}
