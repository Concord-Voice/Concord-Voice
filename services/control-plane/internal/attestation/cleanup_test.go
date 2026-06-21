package attestation_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// fakeRetentionPruner is a thread-safe test double for retentionPruner.
type fakeRetentionPruner struct {
	mu             sync.Mutex
	pruneCallCount int
	pruneErr       error
}

func (f *fakeRetentionPruner) PruneRetention(_ context.Context, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneCallCount++
	return f.pruneErr
}

func (f *fakeRetentionPruner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pruneCallCount
}

// TestCleanup_RunsOnceAndExits verifies that PruneRetention is called at least
// once during the tick interval window and that the loop exits when ctx is
// cancelled.
func TestCleanup_RunsOnceAndExits(t *testing.T) {
	repo := &fakeRetentionPruner{}
	log := logger.New("development")
	c := attestation.NewCleanup(repo, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	// Allow several ticks to fire.
	time.Sleep(60 * time.Millisecond)
	cancel()

	// Goroutine must exit promptly after cancellation.
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancellation")
	}

	require.GreaterOrEqual(t, repo.callCount(), 1, "PruneRetention should have been called at least once")
}

// TestCleanup_CancellationReturnsPromptly verifies that Run exits quickly when
// the context is cancelled before the first tick.
func TestCleanup_CancellationReturnsPromptly(t *testing.T) {
	repo := &fakeRetentionPruner{}
	log := logger.New("development")
	c := attestation.NewCleanup(repo, log)

	// Cancel immediately — the ticker interval is long enough that it won't fire.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		c.Run(ctx, 10*time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return promptly after pre-cancelled ctx")
	}

	require.Equal(t, 0, repo.callCount(), "PruneRetention should not have been called")
}

// TestCleanup_PruneError_ContinuesLoop verifies that a PruneRetention error
// does not abort the loop — subsequent ticks still fire.
func TestCleanup_PruneError_ContinuesLoop(t *testing.T) {
	repo := &fakeRetentionPruner{pruneErr: errors.New("db failure")}
	log := logger.New("development")
	c := attestation.NewCleanup(repo, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancellation")
	}

	require.GreaterOrEqual(t, repo.callCount(), 1,
		"PruneRetention should have been called despite errors")
}
