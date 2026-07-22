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
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte) {
		mu.Lock()
		got = append(got, lifecycleDispatchSequence(t, data))
		if len(got) == 3 {
			close(done)
		}
		mu.Unlock()
	})
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
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte) {
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
	})
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
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte) {
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
	})
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
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte) {
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
	})
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
		func(_ string, data []byte) {
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
		func(_, _ string) {
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
	dispatcher := newVoiceLifecycleDispatcher(func(_ string, data []byte) {
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
	})

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
