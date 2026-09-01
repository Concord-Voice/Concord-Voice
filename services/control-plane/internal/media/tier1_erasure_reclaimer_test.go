package media

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type reclaimDeleteStore struct {
	mu        sync.Mutex
	attempted []string
	deleted   []string
	err       error
	started   chan string
	allow     chan struct{}
	onDelete  func(string)
}

type reclaimLockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *reclaimLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *reclaimLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (s *reclaimDeleteStore) DeleteObject(_ context.Context, key string) error {
	if s.started != nil {
		s.started <- key
	}
	s.mu.Lock()
	s.attempted = append(s.attempted, key)
	s.mu.Unlock()
	if s.onDelete != nil {
		s.onDelete(key)
	}
	if s.allow != nil {
		<-s.allow
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.deleted = append(s.deleted, key)
	return nil
}

func (s *reclaimDeleteStore) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

func (s *reclaimDeleteStore) attempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.attempted...)
}

func insertDueObligation(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key, reconcile_after) VALUES ($1, now() - interval '1 minute')`, key)
	require.NoError(t, err)
}

func obligation(t *testing.T, db *sql.DB, key string) (attempts int, due time.Time) {
	t.Helper()
	require.NoError(t, db.QueryRow(`SELECT attempts, reconcile_after FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&attempts, &due))
	return attempts, due
}

func obligationCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM tier1_erasure_delete_obligations`).Scan(&n))
	return n
}

func insertExpiredProfileUploadIntent(t *testing.T, db *sql.DB, userID, key string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO tier1_profile_upload_intents (storage_key, user_id, profile_slot, expires_at)
		VALUES ($1, $2, 'avatar', clock_timestamp() - interval '1 minute')`, key, userID)
	require.NoError(t, err)
}

func TestTier1ErasureReclaimer_ExpiredProfileIntentTerminalizesWithoutStorageIO(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := testdb.CreateUser(t, db).String()
	key := "avatars/" + userID + "/" + uuid.NewString()
	insertExpiredProfileUploadIntent(t, db, userID, key)
	store := &reclaimDeleteStore{}

	more, err := NewTier1ErasureReclaimer(db, store, logger.New("test")).expireProfileUploadIntentsAtCutoff(
		context.Background(), 10, time.Now())
	require.NoError(t, err)
	assert.False(t, more)
	assert.Empty(t, store.attempts(), "intent expiry must only transfer the exact key to the permanent ledger")
	assert.Equal(t, 1, obligationCountForKey(t, db, key))
	assert.Zero(t, profileUploadIntentCount(t, db, key))
}

func TestTier1ErasureReclaimer_RetiresOnlyPublishedImmutableGeneration(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := testdb.CreateUser(t, db).String()
	publishedKey := "avatars/" + userID + "/" + uuid.NewString()
	ambiguousKey := "avatars/" + userID + "/" + uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO media_files (
			id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot, deleted_at
		) VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/png', 4, $2, 'avatar', NOW())`, userID, publishedKey)
	require.NoError(t, err)
	insertDueObligation(t, db, publishedKey)
	insertDueObligation(t, db, ambiguousKey)

	stats, err := NewTier1ErasureReclaimer(db, &reclaimDeleteStore{}, logger.New("test")).reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Deleted)
	assert.Zero(t, obligationCountForKey(t, db, publishedKey), "a published immutable generation has durable metadata proving it cannot late-PUT")
	assert.Equal(t, 1, obligationCountForKey(t, db, ambiguousKey), "missing metadata is ambiguous PutObject evidence and must remain permanent")
}

func TestTier1ErasureReclaimer_ExpiredProfileIntentSkipsPublicationLock(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := testdb.CreateUser(t, db).String()
	key := "avatars/" + userID + "/" + uuid.NewString()
	insertExpiredProfileUploadIntent(t, db, userID, key)

	publicationTx, err := db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := publicationTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback publication transaction: %v", rollbackErr)
		}
	}()
	_, err = publicationTx.Exec(`SELECT storage_key FROM tier1_profile_upload_intents WHERE storage_key = $1 FOR UPDATE`, key)
	require.NoError(t, err)

	more, err := NewTier1ErasureReclaimer(db, &reclaimDeleteStore{}, logger.New("test")).expireProfileUploadIntentsAtCutoff(
		context.Background(), 10, time.Now())
	require.NoError(t, err)
	assert.False(t, more)
	assert.Equal(t, 1, profileUploadIntentCount(t, db, key), "publication owns a locked intent")
	assert.Zero(t, obligationCountForKey(t, db, key), "a locked publication intent must not be terminalized")
}

func obligationCountForKey(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&count))
	return count
}

func profileUploadIntentCount(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tier1_profile_upload_intents WHERE storage_key = $1`, key).Scan(&count))
	return count
}

func TestTier1ErasureReclaimer_ReclaimDueDeletesThenAcknowledges(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	key := "avatars/reclaim-success"
	insertDueObligation(t, db, key)
	store := &reclaimDeleteStore{started: make(chan string, 4)}
	r := NewTier1ErasureReclaimer(db, store, logger.New("test"))

	stats, err := r.reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, Tier1ErasureReclaimStats{Claimed: 1, Deleted: 1}, stats)
	assert.Equal(t, []string{key}, store.calls(), "the legacy delete must precede acknowledgement")
	attempts, due := obligation(t, db, key)
	assert.Equal(t, 1, attempts)
	assert.WithinDuration(t, time.Now().Add(time.Minute), due, 10*time.Second,
		"the first successful delete must retain a short confirmation retry")
	var lastDeleteAt *time.Time
	require.NoError(t, db.QueryRow(`SELECT last_delete_at FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&lastDeleteAt))
	require.NotNil(t, lastDeleteAt, "successful delete must timestamp the permanent tombstone")
	_, err = db.Exec(`UPDATE tier1_erasure_delete_obligations SET reconcile_after = now() - interval '1 minute' WHERE storage_key = $1`, key)
	require.NoError(t, err)
	stats, err = r.reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Deleted)
	assert.Equal(t, []string{key, key}, store.calls())
	_, due = obligation(t, db, key)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), due, time.Minute,
		"later successful deletes must use the daily confirmation cadence")
}

func TestTier1ErasureReclaimer_NilReceiverIsSafe(t *testing.T) {
	var r *Tier1ErasureReclaimer
	require.NotPanics(t, func() {
		r.Start(context.Background())()
		r.Wake()
		r.start(context.Background(), nil)()
		_, err := r.reclaimDue(context.Background(), 1)
		require.ErrorContains(t, err, "database is unavailable")
	})
}

func TestTier1ErasureReclaimer_StartLogsClaimFailure(t *testing.T) {
	var output reclaimLockedBuffer
	r := NewTier1ErasureReclaimer(nil, nil, logger.NewWithWriter(&output))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), "failure_class")
	}, time.Second, time.Millisecond)
}

func TestTier1ErasureReclaimer_DeleteFailureRetainsAndAdvances(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	key := "avatars/reclaim-delete-failure"
	insertDueObligation(t, db, key)
	store := &reclaimDeleteStore{err: errors.New("private backend failure")}
	r := NewTier1ErasureReclaimer(db, store, logger.New("test"))

	stats, err := r.reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Claimed)
	assert.Equal(t, 1, stats.Retained)
	assert.Equal(t, []string{key}, store.attempts())
	attempts, due := obligation(t, db, key)
	assert.Equal(t, 1, attempts)
	assert.True(t, due.After(time.Now()), "failed work must be rescheduled")
}

func TestTier1ErasureReclaimer_NilLegacyStoreRetainsRetryableRow(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	key := "avatars/reclaim-no-store"
	insertDueObligation(t, db, key)
	r := NewTier1ErasureReclaimer(db, nil, logger.New("test"))

	stats, err := r.reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Retained)
	assert.Equal(t, 1, obligationCount(t, db))
}

func TestTier1ErasureReclaimer_RetryAfterRestartRemovesRetainedRow(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	key := "avatars/reclaim-restart"
	insertDueObligation(t, db, key)
	failed := &reclaimDeleteStore{err: errors.New("temporary")}
	_, err := NewTier1ErasureReclaimer(db, failed, logger.New("test")).reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE tier1_erasure_delete_obligations SET reconcile_after = now() - interval '1 minute' WHERE storage_key = $1`, key)
	require.NoError(t, err)
	store := &reclaimDeleteStore{}
	stats, err := NewTier1ErasureReclaimer(db, store, logger.New("test")).reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Deleted)
	assert.Equal(t, []string{key}, store.calls())
	assert.Equal(t, 1, obligationCount(t, db))
}

func TestTier1ErasureReclaimer_AcknowledgementFailureRetainsForIdempotentRetry(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	key := "avatars/reclaim-ack-failure"
	insertDueObligation(t, db, key)
	_, err := db.Exec(`DROP TRIGGER IF EXISTS reclaim_ack_trigger ON tier1_erasure_delete_obligations`)
	require.NoError(t, err)
	_, err = db.Exec(`DROP FUNCTION IF EXISTS reclaim_ack_fail()`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE FUNCTION reclaim_ack_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'acknowledgement failure'; END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reclaim_ack_trigger BEFORE UPDATE ON tier1_erasure_delete_obligations FOR EACH ROW WHEN (OLD.storage_key = 'avatars/reclaim-ack-failure' AND NEW.last_delete_at IS DISTINCT FROM OLD.last_delete_at) EXECUTE FUNCTION reclaim_ack_fail()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, dropErr := db.Exec(`DROP TRIGGER IF EXISTS reclaim_ack_trigger ON tier1_erasure_delete_obligations`); dropErr != nil {
			t.Errorf("drop test trigger: %v", dropErr)
		}
		if _, dropErr := db.Exec(`DROP FUNCTION IF EXISTS reclaim_ack_fail()`); dropErr != nil {
			t.Errorf("drop test function: %v", dropErr)
		}
	})
	store := &reclaimDeleteStore{}
	stats, err := NewTier1ErasureReclaimer(db, store, logger.New("test")).reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Retained)
	assert.Equal(t, []string{key}, store.attempts(), "object delete must happen once before failed tombstone update")
	_, err = db.Exec(`DROP TRIGGER reclaim_ack_trigger ON tier1_erasure_delete_obligations`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE tier1_erasure_delete_obligations SET reconcile_after = now() - interval '1 minute' WHERE storage_key = $1`, key)
	require.NoError(t, err)
	stats, err = NewTier1ErasureReclaimer(db, store, logger.New("test")).reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Deleted)
	assert.Equal(t, []string{key, key}, store.attempts(), "retry must repeat the idempotent object delete")
	assert.Equal(t, 1, obligationCount(t, db))
}

func TestTier1ErasureReclaimer_ClaimPreventsDuplicateDeleteWithinRetryWindow(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	key := "avatars/reclaim-concurrent"
	insertDueObligation(t, db, key)
	store := &reclaimDeleteStore{started: make(chan string, 1), allow: make(chan struct{})}
	var release sync.Once
	releaseDelete := func() { release.Do(func() { close(store.allow) }) }
	first := NewTier1ErasureReclaimer(db, store, logger.New("test"))
	type result struct {
		stats Tier1ErasureReclaimStats
		err   error
	}
	done := make(chan result, 1)
	go func() { stats, err := first.reclaimDue(context.Background(), 10); done <- result{stats, err} }()
	select {
	case got := <-store.started:
		require.Equal(t, key, got)
	case <-time.After(2 * time.Second):
		releaseDelete()
		got := <-done
		assert.Equal(t, 1, got.stats.Claimed, "first reclaimer must claim before external I/O")
		t.Fatal("first reclaimer did not reach external delete")
	}
	secondDone := make(chan result, 1)
	go func() {
		stats, err := NewTier1ErasureReclaimer(db, store, logger.New("test")).reclaimDue(context.Background(), 10)
		secondDone <- result{stats, err}
	}()
	select {
	case got := <-secondDone:
		require.NoError(t, got.err)
		assert.Zero(t, got.stats.Claimed, "a claimed row is invisible while external deletion is in flight")
	case <-time.After(2 * time.Second):
		releaseDelete()
		t.Fatal("concurrent reclaimer did not return")
	}
	releaseDelete()
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, 1, got.stats.Deleted)
	assert.Equal(t, []string{key}, store.calls())
	assert.Equal(t, 1, obligationCount(t, db), "claim completion must retain the tombstone")
}

func TestTier1ErasureReclaimer_BoundedLimit(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	for _, key := range []string{"avatars/reclaim-limit-1", "avatars/reclaim-limit-2", "avatars/reclaim-limit-3"} {
		insertDueObligation(t, db, key)
	}
	stats, err := NewTier1ErasureReclaimer(db, &reclaimDeleteStore{}, logger.New("test")).reclaimDue(context.Background(), 2)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Claimed, "the pass must drain all due rows in bounded batches")
	assert.Equal(t, 3, obligationCount(t, db))

	stats, err = NewTier1ErasureReclaimer(db, &reclaimDeleteStore{}, logger.New("test")).reclaimDue(context.Background(), 0)
	require.NoError(t, err)
	assert.Zero(t, stats.Claimed)
	assert.Equal(t, 3, obligationCount(t, db))
}

func TestTier1ErasureReclaimer_DrainsFixedCutoffWithoutReclaimingRowsMadeDue(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	const keyPrefix = "avatars/reclaim-cutoff-"
	for i := 0; i < tier1ErasureReclaimBatchMax+1; i++ {
		insertDueObligation(t, db, fmt.Sprintf("%s%03d", keyPrefix, i))
	}
	tailKey := keyPrefix + "100"
	_, err := db.Exec(`UPDATE tier1_erasure_delete_obligations
		SET reconcile_after = now() - interval '1 minute'
		WHERE storage_key LIKE $1`, keyPrefix+"%")
	require.NoError(t, err)

	deleteCount := 0
	store := &reclaimDeleteStore{onDelete: func(string) {
		deleteCount++
		if deleteCount != tier1ErasureReclaimBatchMax {
			return
		}
		_, updateErr := db.Exec(`UPDATE tier1_erasure_delete_obligations
			SET reconcile_after = clock_timestamp()
			WHERE storage_key LIKE $1 AND storage_key < $2`, keyPrefix+"%", tailKey)
		require.NoError(t, updateErr)
	}}
	r := NewTier1ErasureReclaimer(db, store, logger.New("test"))

	stats, err := r.reclaimDue(context.Background(), tier1ErasureReclaimBatchMax)
	require.NoError(t, err)
	assert.Equal(t, tier1ErasureReclaimBatchMax+1, stats.Claimed)
	assert.Equal(t, tier1ErasureReclaimBatchMax+1, stats.Deleted)
	expectedKeys := make([]string, 0, tier1ErasureReclaimBatchMax+1)
	for i := 0; i <= tier1ErasureReclaimBatchMax; i++ {
		expectedKeys = append(expectedKeys, fmt.Sprintf("%s%03d", keyPrefix, i))
	}
	attempts := store.attempts()
	assert.ElementsMatch(t, expectedKeys, attempts, "each original key must be attempted exactly once")
	assert.Contains(t, attempts, tailKey, "the pass must reach the 101st original key")
}

func TestTier1ErasureReclaimer_StartProcessesDueObligationAtStartup(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	key := "avatars/reclaim-startup"
	insertDueObligation(t, db, key)
	store := &reclaimDeleteStore{started: make(chan string, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewTier1ErasureReclaimer(db, store, logger.New("test"))
	r.Start(ctx)
	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("startup reclaim did not run")
	}
	require.Eventually(t, func() bool { return len(store.calls()) == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, obligationCount(t, db), "startup reclaim must retain the permanent tombstone")
}

func TestTier1ErasureReclaimer_WaitBlocksUntilInFlightDeleteFinishes(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	firstKey := "avatars/reclaim-shutdown-first"
	secondKey := "avatars/reclaim-shutdown-second"
	insertDueObligation(t, db, firstKey)
	insertDueObligation(t, db, secondKey)
	store := &reclaimDeleteStore{started: make(chan string, 2), allow: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	r := NewTier1ErasureReclaimer(db, store, logger.New("test"))
	wait := r.Start(ctx)

	select {
	case got := <-store.started:
		require.Equal(t, firstKey, got)
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("startup reclaim did not reach the first delete")
	}

	waitDone := make(chan struct{})
	go func() {
		wait()
		close(waitDone)
	}()
	cancel()
	select {
	case <-waitDone:
		t.Fatal("reclaimer waiter returned while delete was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(store.allow)
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reclaimer waiter did not return after in-flight delete was released")
	}

	assert.Equal(t, []string{firstKey}, store.attempts(), "cancellation must prevent a second claimed key from starting external deletion")
	assert.Equal(t, []string{firstKey}, store.calls(), "the in-flight delete must finish before shutdown returns")
	var lastDeleteAt *time.Time
	require.NoError(t, db.QueryRow(`SELECT last_delete_at FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, firstKey).Scan(&lastDeleteAt))
	assert.NotNil(t, lastDeleteAt, "the in-flight delete must be acknowledged before shutdown returns")
}

func TestTier1ErasureReclaimer_WakeIsNonblockingAndCoalesced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan int, 10001)
	release := make(chan struct{}, 2)
	var passes atomic.Int32
	r := NewTier1ErasureReclaimer(nil, nil, logger.New("test"))
	r.start(ctx, func(runCtx context.Context) {
		pass := int(passes.Add(1))
		select {
		case entered <- pass:
		case <-runCtx.Done():
			return
		}
		select {
		case <-release:
		case <-runCtx.Done():
		}
	})
	select {
	case pass := <-entered:
		require.Equal(t, 1, pass, "startup must run exactly one initial pass")
	case <-time.After(2 * time.Second):
		t.Fatal("startup pass did not begin")
	}
	wakesDone := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			r.Wake()
		}
		close(wakesDone)
	}()
	select {
	case <-wakesDone:
	case <-time.After(time.Second):
		t.Fatal("Wake must remain nonblocking under pressure")
	}
	release <- struct{}{}
	select {
	case pass := <-entered:
		require.Equal(t, 2, pass, "coalesced wakes must schedule one follow-up pass")
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced wake pass did not begin")
	}
	select {
	case release <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("failed to release follow-up pass")
	}
	select {
	case pass := <-entered:
		t.Fatalf("unexpected third pass after coalesced wake: %d", pass)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTier1ErasureReclaimer_LogsNoKeyOrRawError(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	key := "avatars/private-reclaim-log-key"
	raw := "private backend error"
	insertDueObligation(t, db, key)
	store := &reclaimDeleteStore{err: errors.New(raw)}
	var buf bytes.Buffer
	stats, err := NewTier1ErasureReclaimer(db, store, logger.NewWithWriter(&buf)).reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Claimed)
	assert.Equal(t, 1, stats.Retained)
	assert.Equal(t, []string{key}, store.attempts())
	assert.Equal(t, 1, obligationCount(t, db))
	assert.NotContains(t, buf.String(), key)
	assert.NotContains(t, buf.String(), raw)
}

func TestTier1ErasureReclaimer_LogReclaimFailureFiltersCancellation(t *testing.T) {
	var output reclaimLockedBuffer
	r := NewTier1ErasureReclaimer(nil, nil, logger.NewWithWriter(&output))

	r.logReclaimFailure(errors.New("database unavailable"), "claim")
	assert.Contains(t, output.String(), `failure_class=claim`)
	assert.NotContains(t, output.String(), "database unavailable")

	output.mu.Lock()
	output.buf.Reset()
	output.mu.Unlock()
	r.logReclaimFailure(context.Canceled, "claim")
	assert.Empty(t, output.String(), "cancellation should not produce a failure log")
	r.logReclaimFailure(context.DeadlineExceeded, "claim")
	assert.Empty(t, output.String(), "deadline cancellation should not produce a failure log")
}

func TestTier1ErasureReclaimer_ClosedDatabaseReturnsClaimErrors(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	var output reclaimLockedBuffer
	r := NewTier1ErasureReclaimer(db, nil, logger.NewWithWriter(&output))

	_, err = r.reclaimDue(context.Background(), 1)
	require.ErrorContains(t, err, "capture tier1 erasure reclaim cutoff")

	_, _, err = r.reclaimDueAtCutoff(context.Background(), 1, time.Now())
	require.ErrorContains(t, err, "begin tier1 erasure claim")

	_, err = r.claimTier1ErasureBatch(context.Background(), 1, time.Now())
	require.ErrorContains(t, err, "begin tier1 erasure claim")

	more, err := r.expireProfileUploadIntentsAtCutoff(context.Background(), 1, time.Now())
	assert.False(t, more)
	require.ErrorContains(t, err, "begin profile upload intent expiry")

	r.reclaimFairDue(context.Background())
	assert.Contains(t, output.String(), `failure_class=cutoff`)
}
