package graphpresence

import (
	"sync"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// dispatchQueueDepth matches voicepresence's bound. Dispatch is sequential by
// design: every refresh enters the presence sender gate, so a cascade must not
// fan out concurrent gate acquisitions.
const dispatchQueueDepth = 256

// dispatchSink is the swap point for durable dispatch. The one implementation
// that exists is in-memory: it drops whatever is still queued at shutdown, so a
// plan that never reached its worker is lost with no durable record.
//
// #2448 did NOT replace it. That slice built a SIBLING rail
// (internal/activepresence, migration 000111) for the destructive active-category
// paths and left this leg untouched; retrofitting graphpresence onto durable
// dispatch is the shared enforcement outbox (#2635), whose acknowledgement is a
// row delete rather than a channel receive. Swapping it stays a
// construction-site change.
type dispatchSink interface {
	Enqueue(plan *Plan)
	Close()
}

type inMemorySink struct {
	queue   chan *Plan
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
	deliver func(*Plan)
	abandon func(*Plan, string)
	log     *logger.Logger
}

var _ dispatchSink = (*inMemorySink)(nil)

func newInMemorySink(
	deliver func(*Plan),
	abandon func(*Plan, string),
	log *logger.Logger,
) *inMemorySink {
	s := &inMemorySink{
		queue:   make(chan *Plan, dispatchQueueDepth),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
		deliver: deliver,
		abandon: abandon,
		log:     log,
	}
	go s.run()
	return s
}

// Enqueue hands a committed plan to the worker.
//
// The happy path is non-blocking. **Both fallbacks are not:** a closed sink or a
// full queue calls abandon synchronously on the caller's goroutine, which is the
// HTTP handler's, and abandon performs a hub disconnect. That work is bounded
// (dispatchTimeout) but it is still paid inline, precisely when the queue is
// saturated and load is highest. An earlier version of this comment claimed
// Enqueue "never blocks the request path"; it does on overflow, and saying
// otherwise hid a latency cliff rather than removing it (PR #2738 review).
//
// Overflow is left synchronous deliberately: spawning a goroutine per abandoned
// plan under saturation trades a bounded latency cost for unbounded goroutine
// growth at exactly the wrong moment.
//
// The closed check is its OWN select, before the send. A single select with
// both a ready done channel and a ready queue slot picks pseudo-randomly, so an
// enqueue would sometimes win against a closed sink and its plan would never be
// drained (the voicepresence precedent).
//
// It NARROWS the race; it does not close it. A caller can pass the check, then
// lose its slot to Close + drain, then land its plan in the buffer with no
// worker left to take it. Closing that properly needs a mutex shared with
// Close, which is not worth it here — and voicepresence documents the same
// residual rather than papering over it (PR #2738 review, @code-reviewer).
//
// The residual is benign in this process because Close is only ever called from
// shutdown, and the shutdown sequence runs it BEFORE hub.Shutdown, which then
// closes every local client connection anyway. A plan stranded in the buffer
// would have disconnected users the hub is about to disconnect regardless. If
// Close ever gains a non-shutdown caller, that argument dies with it.
func (s *inMemorySink) Enqueue(plan *Plan) {
	if plan == nil {
		return
	}
	select {
	case <-s.done:
		s.abandon(plan, "sink_closed")
		return
	default:
	}
	select {
	case s.queue <- plan:
	default:
		s.abandon(plan, "queue_full")
	}
}

// Close stops the worker and BLOCKS until it has finished draining.
//
// It previously returned as soon as `done` was closed, so a caller that closed
// and exited raced the worker: an in-flight deliver could be cut short and the
// drain's fail-closed abandons might never run, which defeats the point of
// having a drain at all (PR #2738 review, CodeRabbit).
func (s *inMemorySink) Close() {
	s.once.Do(func() { close(s.done) })
	<-s.stopped
}

func (s *inMemorySink) run() {
	defer close(s.stopped)
	for {
		select {
		case <-s.done:
			s.drain()
			return
		case plan := <-s.queue:
			s.deliver(plan)
		}
	}
}

// drain abandons whatever is still queued at shutdown. Dropping it silently
// would leave viewers holding state no one will ever clear.
func (s *inMemorySink) drain() {
	for {
		select {
		case plan := <-s.queue:
			s.abandon(plan, "shutdown_drain")
		default:
			return
		}
	}
}
