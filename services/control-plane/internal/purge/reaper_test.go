package purge

import (
	"context"
	"database/sql"
	"io"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq" // register the "postgres" driver for the fast-failing test handle
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func testLogger() *logger.Logger { return logger.NewWithWriter(io.Discard) }

// failingDB returns a lazily-opened *sql.DB pointed at a closed port so any query
// fails fast with connection-refused. media.CleanupObject calls store.DeleteObject
// BEFORE the DB soft-delete, so the fake deleter still observes every key even
// though the metadata UPDATE errors (which CleanupObject logs and swallows). This
// keeps the reaper drain test hermetic (no real database required).
func failingDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", "host=127.0.0.1 port=1 connect_timeout=1 sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeDeleter records every storage key it is asked to delete and (optionally)
// signals each on a channel so a test can wait for the worker to drain.
type fakeDeleter struct {
	mu   sync.Mutex
	keys []string
	seen chan string
}

func (f *fakeDeleter) DeleteObject(_ context.Context, key string) error {
	f.mu.Lock()
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	if f.seen != nil {
		f.seen <- key
	}
	return nil
}

func TestReaper_EnqueueDrainsEveryKeyToDeleter(t *testing.T) {
	fake := &fakeDeleter{seen: make(chan string, 8)}
	r := NewReaper(failingDB(t), testLogger(), media.NewDeleterResolver(nil, fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.StartWorker(ctx)

	keys := []string{"attachments/a", "attachments/b", "attachments/c"}
	refs := make([]media.BlobRef, 0, len(keys))
	for _, k := range keys {
		// Nil backend == NULL storage_backend == the legacy backend.
		refs = append(refs, media.BlobRef{Key: k})
	}
	r.EnqueueBlobDeletes(refs) // must not block

	got := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for len(got) < len(keys) {
		select {
		case k := <-fake.seen:
			got[k] = true
		case <-timeout:
			t.Fatalf("timed out waiting for worker to drain; got %v", got)
		}
	}
	for _, k := range keys {
		assert.True(t, got[k], "deleter never received %q", k)
	}
}

func TestReaper_EnqueueIsNonBlockingWhenQueueFull(t *testing.T) {
	// No worker draining: fill past the bounded buffer and assert Enqueue returns
	// promptly (drops overflow) instead of deadlocking the request path.
	r := NewReaper(failingDB(t), testLogger(), nil)

	refs := make([]media.BlobRef, blobQueueSize+128)
	for i := range refs {
		refs[i] = media.BlobRef{Key: "attachments/overflow"}
	}

	done := make(chan struct{})
	go func() {
		r.EnqueueBlobDeletes(refs)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("EnqueueBlobDeletes blocked when the queue was full")
	}
}

func TestReaper_EnqueueSkipsEmptyKeys(t *testing.T) {
	r := NewReaper(failingDB(t), testLogger(), nil)
	// Empty keys must never be enqueued (a "" storage key would be a bogus delete).
	r.EnqueueBlobDeletes([]media.BlobRef{{}, {}, {}})
	assert.Equal(t, 0, len(r.jobs), "empty keys should be skipped, not queued")
}

func TestStragglerSweepQuery_Shape(t *testing.T) {
	q := stragglerSweepQuery
	// The predicate that makes the sweep safe: soft-delete scoped, unreaped-only,
	// grace-guarded, and re-issuable idempotently.
	assert.Contains(t, q, "media_files")
	assert.Contains(t, q, "storage_key")
	assert.Contains(t, q, "deleted_at IS NOT NULL")
	assert.Contains(t, q, "blob_reaped_at IS NULL")    // retires reaped rows — see below
	assert.Contains(t, q, "make_interval(secs => $1)") // grace lower bound
	assert.Contains(t, q, "LIMIT $2")                  // per-tick stride

	// #1352 regression lock, cheap enough to assert on the SQL itself. There must be
	// no upper time bound: one aged a row out of the sweep permanently, leaking any
	// blob whose key the queue dropped. The behavioural locks live in
	// reaper_sweep_test.go; this catches a reintroduction even without a database.
	assert.NotContains(t, q, "deleted_at >=",
		"the sweep must have no upper time bound — age must never retire an unreaped blob")
}

func TestReaper_CollectStragglers_SurfacesQueryError(t *testing.T) {
	// The sweep path must surface a query error (so sweepOnce can log it) rather than
	// silently succeeding — exercised against the fast-failing DB handle.
	r := NewReaper(failingDB(t), testLogger(), nil)
	_, err := r.collectStragglers(context.Background())
	require.Error(t, err)
}

func TestReaper_SweepOnce_DoesNotPanicOnQueryError(t *testing.T) {
	// sweepOnce swallows the error (best-effort) and must not panic.
	r := NewReaper(failingDB(t), testLogger(), media.NewDeleterResolver(nil, &fakeDeleter{}))
	r.sweepOnce(context.Background())
}
