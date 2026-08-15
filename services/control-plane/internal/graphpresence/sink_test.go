package graphpresence

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSinkDeliversEnqueuedPlan(t *testing.T) {
	delivered := make(chan *Plan, 1)
	s := newInMemorySink(func(p *Plan) { delivered <- p }, func(*Plan, string) {}, nil)
	defer s.Close()

	want := &Plan{}
	s.Enqueue(want)

	select {
	case got := <-delivered:
		assert.Same(t, want, got, "the delivered plan must be the enqueued plan")
	case <-time.After(2 * time.Second):
		require.Fail(t, "plan was never delivered")
	}
}

// After Close, an Enqueue must ABANDON rather than silently drop: a plan that
// enters a closed queue is never drained, which is a fail-open.
//
// This enqueues MANY times on purpose. A single post-close enqueue is a
// coin-flip detector, not a guard: against the defect this test exists to
// catch — a single fused `select` with both the closed `done` channel and a
// free queue slot ready — Go picks uniformly at random, so one enqueue would
// catch the bug only about half the time and the test would read as a flake.
// Requiring ALL postCloseEnqueues to abandon drives that false pass to 2^-N
// while staying fully deterministic against the correct implementation, which
// takes the closed branch every time.
func TestSinkAbandonsAfterClose(t *testing.T) {
	const postCloseEnqueues = 20

	var mu sync.Mutex
	var abandoned int
	s := newInMemorySink(func(*Plan) {}, func(*Plan, string) {
		mu.Lock()
		abandoned++
		mu.Unlock()
	}, nil)

	s.Close()
	for i := 0; i < postCloseEnqueues; i++ {
		s.Enqueue(&Plan{})
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, postCloseEnqueues, abandoned,
		"every post-close enqueue must abandon; a partial count means the "+
			"closed check and the send share one select")
}

func TestSinkCloseIsIdempotent(_ *testing.T) {
	s := newInMemorySink(func(*Plan) {}, func(*Plan, string) {}, nil)
	s.Close()
	s.Close() // must not panic on a double close of the done channel
}

// A queue at capacity must abandon rather than block the caller: Enqueue runs
// on the request path after commit.
func TestSinkAbandonsWhenQueueFull(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var abandoned int

	s := newInMemorySink(
		func(*Plan) { <-release },
		func(*Plan, string) { mu.Lock(); abandoned++; mu.Unlock() },
		nil,
	)
	defer func() { close(release); s.Close() }()

	for i := 0; i < dispatchQueueDepth+8; i++ {
		s.Enqueue(&Plan{})
	}

	mu.Lock()
	defer mu.Unlock()
	assert.NotZero(t, abandoned,
		"a full queue must abandon rather than block the request path")
}
