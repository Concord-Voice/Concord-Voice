package voice

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
)

const (
	voiceLifecycleDispatchWorkerCount = 16
	voiceLifecycleDispatchRoomLimit   = 64
	voiceLifecycleDispatchRoomCount   = 1024
	voiceLifecycleDispatchTotalLimit  = 4096
)

type voiceLifecycleDispatchEvent struct {
	subject string
	data    []byte
	roomKey string
}

type voiceLifecycleDispatchRoom struct {
	queue  []voiceLifecycleDispatchEvent
	active bool
	ready  bool
}

type voiceLifecycleDispatchOverflow struct {
	subject string
	roomKey string
}

// voiceLifecycleDispatcher is a bounded fair scheduler. The NATS callback only
// copies and enqueues; a saturated room is recovered conservatively without
// parking ingress for unrelated rooms. At most one worker owns a room, which
// preserves its cross-subject FIFO while the fixed pool runs rooms concurrently.
type voiceLifecycleDispatcher struct {
	mu              sync.Mutex
	ready           *sync.Cond
	overflowReady   *sync.Cond
	rooms           map[string]*voiceLifecycleDispatchRoom
	readyIDs        []string
	pending         int
	closed          bool
	handler         func(string, []byte)
	overflow        func(string, string)
	overflowEvent   voiceLifecycleDispatchOverflow
	overflowPending bool
	wg              sync.WaitGroup
	overflowWG      sync.WaitGroup
}

func newVoiceLifecycleDispatcher(
	handler func(string, []byte),
	overflow ...func(string, string),
) *voiceLifecycleDispatcher {
	dispatcher := &voiceLifecycleDispatcher{
		rooms:   make(map[string]*voiceLifecycleDispatchRoom),
		handler: handler,
	}
	if len(overflow) > 0 {
		dispatcher.overflow = overflow[0]
	}
	dispatcher.ready = sync.NewCond(&dispatcher.mu)
	dispatcher.overflowReady = sync.NewCond(&dispatcher.mu)
	for range voiceLifecycleDispatchWorkerCount {
		dispatcher.wg.Add(1)
		go dispatcher.runWorker()
	}
	if dispatcher.overflow != nil {
		dispatcher.overflowWG.Add(1)
		go dispatcher.runOverflowWorker()
	}
	return dispatcher
}

func (d *voiceLifecycleDispatcher) enqueue(subject string, data []byte) {
	if d == nil || d.handler == nil {
		return
	}
	event := voiceLifecycleDispatchEvent{
		subject: subject,
		data:    append([]byte(nil), data...),
		roomKey: voiceLifecycleDispatchRoomKey(subject, data),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	room, overflowed := d.roomForEnqueueLocked(event.roomKey)
	if !overflowed && event.subject == natsSubjectVoiceHeartbeat &&
		coalescePendingVoiceHeartbeat(room, event) {
		return
	}
	if !overflowed && (len(room.queue) >= voiceLifecycleDispatchRoomLimit ||
		d.pending >= voiceLifecycleDispatchTotalLimit) {
		overflowed = true
	}
	if overflowed {
		d.scheduleOverflowLocked(event)
		return
	}
	room.queue = append(room.queue, event)
	d.pending++
	if !room.active && !room.ready {
		room.ready = true
		d.readyIDs = append(d.readyIDs, event.roomKey)
		d.ready.Signal()
	}
}

// roomForEnqueueLocked returns an existing/new room or reports that bounded
// global capacity has been exhausted. The caller must hold d.mu.
func (d *voiceLifecycleDispatcher) roomForEnqueueLocked(
	roomKey string,
) (*voiceLifecycleDispatchRoom, bool) {
	if room := d.rooms[roomKey]; room != nil {
		return room, false
	}
	if len(d.rooms) >= voiceLifecycleDispatchRoomCount ||
		d.pending >= voiceLifecycleDispatchTotalLimit {
		return nil, true
	}
	room := &voiceLifecycleDispatchRoom{
		queue: make([]voiceLifecycleDispatchEvent, 0, voiceLifecycleDispatchRoomLimit),
	}
	d.rooms[roomKey] = room
	return room, false
}

// scheduleOverflowLocked coalesces overflow recovery onto the dedicated
// worker. The caller must hold d.mu.
func (d *voiceLifecycleDispatcher) scheduleOverflowLocked(
	event voiceLifecycleDispatchEvent,
) {
	if d.overflow == nil || d.overflowPending {
		return
	}
	d.overflowEvent = voiceLifecycleDispatchOverflow{
		subject: event.subject,
		roomKey: event.roomKey,
	}
	d.overflowPending = true
	d.overflowReady.Signal()
}

// coalescePendingVoiceHeartbeat replaces only an adjacent pending heartbeat.
// Since each room owns one FIFO, a joined/left/room-empty tail is an explicit
// lifecycle boundary that the incoming heartbeat cannot cross.
func coalescePendingVoiceHeartbeat(
	room *voiceLifecycleDispatchRoom,
	incoming voiceLifecycleDispatchEvent,
) bool {
	if room == nil || len(room.queue) == 0 {
		return false
	}
	last := len(room.queue) - 1
	if room.queue[last].subject != natsSubjectVoiceHeartbeat {
		return false
	}
	room.queue[last] = incoming
	return true
}

func (d *voiceLifecycleDispatcher) runWorker() {
	defer d.wg.Done()
	for {
		event, ok := d.nextEvent()
		if !ok {
			return
		}
		d.handler(event.subject, event.data)
		d.finishEvent(event.roomKey)
	}
}

func (d *voiceLifecycleDispatcher) runOverflowWorker() {
	defer d.overflowWG.Done()
	for {
		event, ok := d.nextOverflow()
		if !ok {
			return
		}
		d.overflow(event.subject, event.roomKey)
	}
}

func (d *voiceLifecycleDispatcher) nextOverflow() (
	voiceLifecycleDispatchOverflow,
	bool,
) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for !d.overflowPending && !d.closed {
		d.overflowReady.Wait()
	}
	if d.closed {
		return voiceLifecycleDispatchOverflow{}, false
	}
	event := d.overflowEvent
	d.overflowEvent = voiceLifecycleDispatchOverflow{}
	d.overflowPending = false
	return event, true
}

func (d *voiceLifecycleDispatcher) nextEvent() (voiceLifecycleDispatchEvent, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.readyIDs) == 0 && !d.closed {
		d.ready.Wait()
	}
	if len(d.readyIDs) == 0 {
		return voiceLifecycleDispatchEvent{}, false
	}
	roomID := d.readyIDs[0]
	copy(d.readyIDs, d.readyIDs[1:])
	d.readyIDs = d.readyIDs[:len(d.readyIDs)-1]
	room := d.rooms[roomID]
	room.ready = false
	room.active = true
	event := room.queue[0]
	copy(room.queue, room.queue[1:])
	room.queue[len(room.queue)-1] = voiceLifecycleDispatchEvent{}
	room.queue = room.queue[:len(room.queue)-1]
	d.pending--
	return event, true
}

func (d *voiceLifecycleDispatcher) finishEvent(roomID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	room := d.rooms[roomID]
	if room == nil {
		return
	}
	room.active = false
	if len(room.queue) == 0 {
		delete(d.rooms, roomID)
		return
	}
	room.ready = true
	d.readyIDs = append(d.readyIDs, roomID)
	d.ready.Signal()
}

func (d *voiceLifecycleDispatcher) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.readyIDs = nil
	d.pending = 0
	d.overflowEvent = voiceLifecycleDispatchOverflow{}
	d.overflowPending = false
	for roomID, room := range d.rooms {
		room.queue = nil
		room.ready = false
		if !room.active {
			delete(d.rooms, roomID)
		}
	}
	d.ready.Broadcast()
	d.overflowReady.Broadcast()
	d.mu.Unlock()
	d.wg.Wait()
	d.overflowWG.Wait()
}

func voiceLifecycleDispatchRoomKey(subject string, data []byte) string {
	var envelope struct {
		ChannelID string `json:"channelId"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.ChannelID != "" {
		return envelope.ChannelID
	}
	digest := sha256.Sum256(append(append([]byte(nil), subject...), data...))
	return string(digest[:])
}
