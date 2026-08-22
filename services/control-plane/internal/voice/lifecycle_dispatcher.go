package voice

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
)

// voiceLifecycleDispatchRoomCount bounds CONCURRENTLY BACKLOGGED rooms, not
// concurrently active ones: finishEvent deletes a room as soon as its queue
// drains, so a room appears here only while a handler is still running for it.
// At the ceiling the map costs 1024 rooms * cap(64) * sizeof(event) = 4.0 MiB.
// The event struct is 64 B since #2868 added dropClass (it was 56 B; the byte
// lands in alignment padding but the eager cap-64 allocation multiplies it).
//
// Raising it moves the cliff rather than removing it, and bounding is #2757's
// scope. Since #2868 the cliff is no longer destructive: crossing it drops and
// counts (see scheduleOverflowLocked) instead of disconnecting the replica.
const (
	voiceLifecycleDispatchWorkerCount = 16
	voiceLifecycleDispatchRoomLimit   = 64
	voiceLifecycleDispatchRoomCount   = 1024
	voiceLifecycleDispatchTotalLimit  = 4096
)

type voiceLifecycleDropClass uint8

const (
	voiceLifecycleDropConvergent   voiceLifecycleDropClass = iota // heartbeat, joined, left
	voiceLifecycleDropTerminal                                    // room_empty
	voiceLifecycleDropUnresolvable                                // room key took the SHA-256 arm; see classifyVoiceLifecycleDrop
	voiceLifecycleDropClassCount
)

// voiceLifecycleDropCounts is an ARRAY, not a map or slice: snapshotting it out
// from under d.mu is a value copy with no aliasing, so no defensive clone is
// needed and no reviewer has to check for one.
type voiceLifecycleDropCounts [voiceLifecycleDropClassCount]uint64

// classifyVoiceLifecycleDrop names what a dropped event costs.
//
// A subject outside voiceIngressHandledSubjects cannot reach enqueue (G1 rejects
// it at the subscription -- G0, not G1, which is the channel-ID check), so the
// default arm is Terminal: unknown subject, most
// conservative class. It costs nothing because every class drops.
func classifyVoiceLifecycleDrop(subject string, resolved bool) voiceLifecycleDropClass {
	if !resolved {
		// Kept DISTINCT from Terminal even though the two are handled identically
		// today -- same severity, same first-report policy, same drop.
		//
		// It is a CANARY, not scaffolding. Post-#2871 the G1 gate rejects any
		// event whose channelId is not a canonical UUID, so voiceLifecycleDispatchRoomKey
		// always reports resolved=true and this class cannot fire in production.
		// If dispatch_drop_unresolvable ever DOES appear in a production log, the
		// gate has stopped doing its job -- and folding this into Terminal would
		// make that regression indistinguishable from an ordinary room_empty drop.
		//
		// An earlier comment described this as "test-reachable only", which reads
		// as dead weight and duly attracted a proposal to delete it. The class
		// earns its vocabulary entry by being the only signal that an invariant
		// broke; retire it only if the resolved=false path is removed outright.
		return voiceLifecycleDropUnresolvable
	}
	switch subject {
	case natsSubjectVoiceHeartbeat, natsSubjectVoiceJoined, natsSubjectVoiceLeft:
		// The heartbeat reconciles BIDIRECTIONALLY: reconcileServerHeartbeatParticipants
		// removes rows absent from the media list and refreshServerHeartbeatParticipant
		// adds rows present in it, so a dropped joined or left converges on the
		// room's next 30s tick exactly as a dropped heartbeat does.
		return voiceLifecycleDropConvergent
	default:
		// room_empty and anything unrecognised. After room_empty the media plane
		// drops the room from getActiveRoomIds(), so no successor message exists.
		return voiceLifecycleDropTerminal
	}
}

type voiceLifecycleDispatchEvent struct {
	subject   string
	data      []byte
	roomKey   string
	dropClass voiceLifecycleDropClass
}

type voiceLifecycleDispatchRoom struct {
	queue  []voiceLifecycleDispatchEvent
	active bool
	ready  bool
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
	overflow        func(voiceLifecycleDropCounts)
	overflowCounts  voiceLifecycleDropCounts
	overflowPending bool
	wg              sync.WaitGroup
	overflowWG      sync.WaitGroup
}

// newVoiceLifecycleDispatcher takes overflow as a REQUIRED parameter, not a
// variadic. It was variadic until #2868, when un-wiring it stopped being cheap:
// scheduleOverflowLocked returns before the tally when d.overflow is nil, so a
// deleted argument would silently delete every drop COUNT as well as the report
// -- compiling, all-green, and byte-identical to a replica that never overflows.
// Same failure shape ingress_gate_test.go already pins for a disabled gate.
func newVoiceLifecycleDispatcher(
	handler func(string, []byte),
	overflow func(voiceLifecycleDropCounts),
) *voiceLifecycleDispatcher {
	dispatcher := &voiceLifecycleDispatcher{
		rooms:   make(map[string]*voiceLifecycleDispatchRoom),
		handler: handler,
	}
	dispatcher.overflow = overflow
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
	roomKey, resolved := voiceLifecycleDispatchRoomKey(subject, data)
	event := voiceLifecycleDispatchEvent{
		subject:   subject,
		data:      append([]byte(nil), data...),
		roomKey:   roomKey,
		dropClass: classifyVoiceLifecycleDrop(subject, resolved),
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

// scheduleOverflowLocked accumulates the drop and coalesces overflow recovery
// onto the dedicated worker. The caller must hold d.mu.
//
// The count increments BEFORE the overflowPending check. While an overflow is
// pending every subsequent overflowed event is discarded, so counting after the
// check would under-report by exactly the coalescing factor - which is the
// factor that matters under saturation.
func (d *voiceLifecycleDispatcher) scheduleOverflowLocked(
	event voiceLifecycleDispatchEvent,
) {
	// Count FIRST. A nil reporter must lose the report, never the tally --
	// otherwise an un-wired dispatcher is indistinguishable from a healthy one.
	d.overflowCounts[event.dropClass]++
	if d.overflow == nil {
		return
	}
	if d.overflowPending {
		return
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
		counts, ok := d.nextOverflow()
		if !ok {
			return
		}
		// d.mu is NOT held here: nextOverflow released it before returning, so
		// the callback takes the reporter mutex with no dispatcher lock held and
		// d.mu stays a leaf.
		//
		// Keep it that way. An adversarial pass confirmed a callback that
		// re-enters enqueue does not deadlock TODAY -- there is no cycle in
		// either direction -- so this is a forward constraint, not a description
		// of a live hazard. It becomes one the moment the callback is invoked
		// while d.mu is held, or the reporter starts calling back in.
		d.overflow(counts)
	}
}

func (d *voiceLifecycleDispatcher) nextOverflow() (voiceLifecycleDropCounts, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for !d.overflowPending && !d.closed {
		d.overflowReady.Wait()
	}
	if d.closed {
		return voiceLifecycleDropCounts{}, false
	}
	counts := d.overflowCounts
	// Accumulated drop counts are DISCARDED here, not flushed. The final partial
	// window is lost at shutdown -- the same accepted residual recordClass
	// documents for shed-driven reporting, restated at the discard site so it
	// does not read as a plain state reset.
	d.overflowCounts = voiceLifecycleDropCounts{}
	d.overflowPending = false
	return counts, true
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
	d.overflowCounts = voiceLifecycleDropCounts{}
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

// voiceLifecycleDispatchRoomKey returns the room key and whether it was RESOLVED
// from the payload's channelId. A false report means the key came from the
// SHA-256 fallback arm, which is reserved for #2757 and whose body is unchanged.
func voiceLifecycleDispatchRoomKey(subject string, data []byte) (string, bool) {
	var envelope struct {
		ChannelID string `json:"channelId"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.ChannelID != "" {
		return envelope.ChannelID, true
	}
	digest := sha256.Sum256(append(append([]byte(nil), subject...), data...))
	return string(digest[:]), false
}
