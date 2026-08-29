package media

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Worker plumbing and failure paths for the tier-2 orphan reaper.
//
// INTERNAL package, unlike orphan_reaper_test.go, for one reason: the fan-out
// consumes fakeSweepRegistry (multi_backend_sweep_test.go) and the workers are
// unexported. Splitting on that line keeps the behavioural tests in media_test
// where they exercise the real exported surface.

// countingStore records sweeps so a test can prove a worker ran without
// touching a real bucket.
type countingStore struct {
	mu     sync.Mutex
	sweeps int
	err    error
}

func (c *countingStore) ListObjects(
	_ context.Context, _ string, _ time.Time,
) ([]storage.StoredObject, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweeps++
	return nil, c.err
}

func (c *countingStore) DeleteObject(_ context.Context, _ string) error { return nil }

func (c *countingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sweeps
}

// TestStartOrphanSweepWorkers_CoversEveryRegisteredBackend: one worker per
// registered backend, for the same reason the session sweeper fans out — a
// reaper holds one client and therefore enumerates one bucket, so wiring only
// the legacy client leaves every vendor-resident orphan invisible, and
// invisible SILENTLY (Listed falls to zero on a bucket nobody enumerated).
//
// The context is pre-cancelled so no worker issues an object-store call against
// the zero-value client the registry fake hands back; what is under test here is
// the enumeration decision, not the sweep.
func TestStartOrphanSweepWorkers_CoversEveryRegisteredBackend(t *testing.T) {
	reg := &fakeSweepRegistry{ids: []storage.BackendID{storage.LegacyBackendID, "r2-useast", "r2-eu"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := StartOrphanSweepWorkers(ctx, nil, reg, sweepTestLogger(), time.Hour)

	assert.Equal(t, 3, started, "every registered backend needs its own reaper, not just the legacy one")
}

// TestStartOrphanSweepWorkers_SkipsUnresolvableBackend: a backend that cannot be
// resolved is not swept, and "started nothing" must be distinguishable from
// "swept nothing" — which is exactly what the return value is for.
func TestStartOrphanSweepWorkers_SkipsUnresolvableBackend(t *testing.T) {
	reg := &fakeSweepRegistry{
		ids:    []storage.BackendID{storage.LegacyBackendID, "r2-useast"},
		broken: map[storage.BackendID]bool{"r2-useast": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := StartOrphanSweepWorkers(ctx, nil, reg, sweepTestLogger(), time.Hour)

	assert.Equal(t, 1, started, "an unresolvable backend goes unreaped rather than silently counted")
}

// TestStartOrphanSweepWorkers_NilRegistryStartsNothing covers the embedder with
// no object storage at all.
func TestStartOrphanSweepWorkers_NilRegistryStartsNothing(t *testing.T) {
	assert.Zero(t, StartOrphanSweepWorkers(
		context.Background(), nil, nil, sweepTestLogger(), time.Hour))
}

// TestStartOrphanSweepWorker_SweepsAtStartup pins the startup sweep. A ticker
// alone never covers orphans stranded while the process was DOWN, which is the
// case this reaper exists for — an account erased during an outage strands its
// objects and nothing else will ever look for them.
func TestStartOrphanSweepWorker_SweepsAtStartup(t *testing.T) {
	store := &countingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startOrphanSweepWorker(ctx,
		NewOrphanReaper(nil, store, string(storage.LegacyBackendID), sweepTestLogger()),
		sweepTestLogger(), time.Hour)

	require.Eventually(t, func() bool { return store.count() >= 1 }, 2*time.Second, 10*time.Millisecond,
		"the worker must sweep once at startup, not wait a full interval")
}

// TestStartOrphanSweepWorker_DeadContextSweepsNothing: a context already
// cancelled means the process is shutting down, so the startup sweep must not
// fire. Checked before the run rather than only in the select, which would
// catch it one full interval late.
func TestStartOrphanSweepWorker_DeadContextSweepsNothing(t *testing.T) {
	store := &countingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	startOrphanSweepWorker(ctx,
		NewOrphanReaper(nil, store, string(storage.LegacyBackendID), sweepTestLogger()),
		sweepTestLogger(), time.Hour)

	assert.Never(t, func() bool { return store.count() > 0 }, 300*time.Millisecond, 20*time.Millisecond,
		"a dead context must not start object-store work")
}

// TestStartOrphanSweepWorker_LogsFailedSweep: a failing sweep is reported, not
// swallowed. The reaper is the only thing that can find these objects, so a
// silently broken one leaks indefinitely with nothing to indicate it.
func TestStartOrphanSweepWorker_LogsFailedSweep(t *testing.T) {
	var buf syncBuffer
	store := &countingStore{err: errors.New("bucket unreachable")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startOrphanSweepWorker(ctx,
		NewOrphanReaper(nil, store, string(storage.LegacyBackendID), logger.NewWithWriter(&buf)),
		logger.NewWithWriter(&buf), time.Hour)

	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), "attachment orphan sweep FAILED")
	}, 2*time.Second, 10*time.Millisecond, "a failed sweep must be alertable, not silent")
}

// TestOrphanReaperBackendLabel keeps an unlabelled reaper distinguishable in a
// log line from one covering the legacy backend by name.
func TestOrphanReaperBackendLabel(t *testing.T) {
	assert.Equal(t, "(unlabelled)", NewOrphanReaper(nil, nil, "", sweepTestLogger()).backendLabel())
	assert.Equal(t, "r2-useast", NewOrphanReaper(nil, nil, "r2-useast", sweepTestLogger()).backendLabel())
}

// TestOrphanReaperBackendArg pins the NULL mapping. media_files.storage_backend
// is NULL for every pre-ADR-0038 object and no row ever stores the literal
// "legacy", so the legacy reaper must query with NULL or its pair-keyed claim
// check matches nothing and every legacy object reads as an orphan.
func TestOrphanReaperBackendArg(t *testing.T) {
	assert.Nil(t, NewOrphanReaper(nil, nil, string(storage.LegacyBackendID), sweepTestLogger()).backendArg(),
		"the legacy backend is stored as NULL, not as the literal 'legacy'")
	assert.Nil(t, NewOrphanReaper(nil, nil, "", sweepTestLogger()).backendArg())
	assert.Equal(t, "r2-useast", NewOrphanReaper(nil, nil, "r2-useast", sweepTestLogger()).backendArg())
}

// TestSweepOrphansFailsClosedOnClaimCheckError is the most important failure
// path in the file. An unanswerable claim check is the one condition under
// which deleting would be GUESSING, and the guess destroys data — so the sweep
// aborts the batch rather than treating "I could not ask" as "nothing claims
// it".
func TestSweepOrphansFailsClosedOnClaimCheckError(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid/invalid?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close()) // every query now fails with sql.ErrConnDone

	store := &fakeListStore{objects: []storage.StoredObject{{Key: "attachments/orphan-1"}}}
	reaper := NewOrphanReaper(db, store, string(storage.LegacyBackendID), sweepTestLogger())

	res, sweepErr := reaper.SweepOrphans(context.Background())

	require.Error(t, sweepErr, "an unanswerable claim check must abort the batch")
	assert.Contains(t, sweepErr.Error(), "claim check failed")
	assert.Zero(t, res.Reaped)
	assert.Empty(t, store.deleted, "nothing may be deleted on the strength of a check that did not run")
}

// fakeListStore returns a fixed listing and records deletes. Separate from
// countingStore because this one needs to hand back candidates.
type fakeListStore struct {
	objects []storage.StoredObject
	deleted []string
}

func (f *fakeListStore) ListObjects(
	_ context.Context, _ string, _ time.Time,
) ([]storage.StoredObject, error) {
	return f.objects, nil
}

func (f *fakeListStore) DeleteObject(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}

// syncBuffer is a mutex-guarded buffer: the worker logs from its own goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestStartOrphanSweepWorker_RecursOnTickAndStopsOnCancel covers the two halves
// of the loop the startup sweep does not reach.
//
// The tick half matters on its own: a reaper that sweeps once at boot and never
// again looks identical in every log to one that is working, because a healthy
// sweep with nothing to reclaim is silent by design (only res.Orphaned > 0
// logs). Nothing else in the system would notice it had stopped.
func TestStartOrphanSweepWorker_RecursOnTickAndStopsOnCancel(t *testing.T) {
	store := &countingStore{}
	var buf syncBuffer
	ctx, cancel := context.WithCancel(context.Background())

	startOrphanSweepWorker(ctx,
		NewOrphanReaper(nil, store, string(storage.LegacyBackendID), logger.NewWithWriter(&buf)),
		logger.NewWithWriter(&buf), 20*time.Millisecond)

	// >= 2 proves the ticker fired: the startup sweep alone accounts for one.
	require.Eventually(t, func() bool { return store.count() >= 2 }, 3*time.Second, 10*time.Millisecond,
		"the sweep must recur on the interval, not only at startup")

	cancel()
	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), "attachment orphan sweep worker stopped")
	}, 2*time.Second, 10*time.Millisecond, "the worker must stop on context cancellation")

	settled := store.count()
	assert.Never(t, func() bool { return store.count() > settled+1 }, 200*time.Millisecond, 20*time.Millisecond,
		"a cancelled worker must stop sweeping, not merely log that it stopped")
}
