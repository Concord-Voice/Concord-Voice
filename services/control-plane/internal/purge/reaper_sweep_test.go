package purge

// Database-backed tests for the straggler sweep (#1352): the crash-recovery path
// that re-reaps blobs whose media_files row was soft-deleted but whose object the
// worker never drained. Skipped when DATABASE_URL is unset (CI sets it).

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// sweepTestDB opens the integration database, or skips.
func sweepTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL required for straggler-sweep coverage")
	}
	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedUploader creates a user row to satisfy media_files.uploader_id.
func seedUploader(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO users (id, username, email, password_hash)
		VALUES (gen_random_uuid(), 'sweep_' || substr(md5(random()::text), 1, 12),
		        'sweep_' || substr(md5(random()::text), 1, 12) || '@example.test', 'x')
		RETURNING id`).Scan(&id)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

// seedAttachmentChannel creates a server+channel so tier-2 media rows can satisfy
// media_files' valid_media_context CHECK, which requires a media_tier=2 row to carry a
// key_version AND exactly one of channel_id / conversation_id.
func seedAttachmentChannel(t *testing.T, db *sql.DB, owner string) string {
	t.Helper()
	var serverID, channelID string
	require.NoError(t, db.QueryRow(
		`INSERT INTO servers (name, owner_id) VALUES ('sweep-srv', $1) RETURNING id`, owner).Scan(&serverID))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID) })
	require.NoError(t, db.QueryRow(
		`INSERT INTO channels (server_id, name) VALUES ($1, 'general') RETURNING id`, serverID).Scan(&channelID))
	return channelID
}

// seedMediaFile inserts a tier-2 ATTACHMENT media_files row with an explicit
// deleted_at age (seconds ago; nil = not soft-deleted) and returns its storage key.
//
// Tier 2 is deliberate, not incidental: the sweep is bounded to media_tier = 2 — the
// only rows a purge can orphan — so a tier-1 seed here would never be selected and
// every assertion below would pass VACUOUSLY. The key is generated server-side
// (gen_random_uuid), mirroring the per-upload-unique `attachments/<fileID>` shape.
func seedMediaFile(t *testing.T, db *sql.DB, uploader, channelID string, deletedAgoSecs *int) string {
	t.Helper()
	var key string
	var err error

	if deletedAgoSecs == nil {
		err = db.QueryRow(`
			INSERT INTO media_files (uploader_id, file_type, media_tier, key_version, channel_id,
			                         mime_type, file_size, storage_key)
			VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
			        'attachments/' || gen_random_uuid()::text)
			RETURNING storage_key`, uploader, channelID).Scan(&key)
	} else {
		err = db.QueryRow(`
			INSERT INTO media_files (uploader_id, file_type, media_tier, key_version, channel_id,
			                         mime_type, file_size, storage_key, deleted_at)
			VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
			        'attachments/' || gen_random_uuid()::text, NOW() - make_interval(secs => $3))
			RETURNING storage_key`, uploader, channelID, *deletedAgoSecs).Scan(&key)
	}

	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM media_files WHERE storage_key = $1`, key) })
	return key
}

// recordingDeleter captures the keys handed to object storage.
type recordingDeleter struct{ keys []string }

func (r *recordingDeleter) DeleteObject(_ context.Context, key string) error {
	r.keys = append(r.keys, key)
	return nil
}

// reapedAt reads a row's blob_reaped_at marker (nil = not yet reaped).
func reapedAt(t *testing.T, db *sql.DB, key string) *time.Time {
	t.Helper()
	var at *time.Time
	require.NoError(t, db.QueryRow(
		`SELECT blob_reaped_at FROM media_files WHERE storage_key = $1`, key).Scan(&at))
	return at
}

// TestSweepOnce_ReapsOnlyEligibleStragglers locks the sweep predicate: a row past the
// grace period is reaped no matter how old it is; one inside the grace period is left
// alone (do not race the live worker); one never soft-deleted is untouched.
//
// The `ancient` case is the #1352 regression lock. An earlier revision bounded the
// sweep to a 24h look-back and assumed anything older was "already reaped" — so a key
// dropped by a full queue leaked its object forever. Age must NOT confer immunity:
// the only thing that retires a row is a confirmed reap (blob_reaped_at).
func TestSweepOnce_ReapsOnlyEligibleStragglers(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	channelID := seedAttachmentChannel(t, db, uploader)

	eligibleAge := stragglerGraceSeconds + 600 // comfortably past the grace window
	tooRecentAge := stragglerGraceSeconds / 2  // still being drained by the worker
	ancientAge := 90 * 24 * 3600               // 90 days — far beyond any look-back

	eligible := seedMediaFile(t, db, uploader, channelID, &eligibleAge)
	tooRecent := seedMediaFile(t, db, uploader, channelID, &tooRecentAge)
	ancient := seedMediaFile(t, db, uploader, channelID, &ancientAge)
	live := seedMediaFile(t, db, uploader, channelID, nil)

	store := &recordingDeleter{}
	r := NewReaper(db, logger.NewWithWriter(io.Discard), store)
	r.sweepOnce(context.Background())

	assert.Contains(t, store.keys, eligible, "straggler past the grace window must be reaped")
	assert.Contains(t, store.keys, ancient,
		"an old unreaped row must still be reaped — age must never age a blob out into a permanent leak")
	assert.NotContains(t, store.keys, tooRecent, "row inside the grace window is the worker's to drain")
	assert.NotContains(t, store.keys, live, "row that was never soft-deleted must never be reaped")

	assert.NotNil(t, reapedAt(t, db, eligible), "a confirmed reap must be marked")
	assert.Nil(t, reapedAt(t, db, tooRecent), "an unswept row must stay unmarked")
	assert.Nil(t, reapedAt(t, db, live), "a live row must never be marked")
}

// TestSweepOnce_AdvancesPastReapedRows is the anti-starvation lock. A purge stamps
// thousands of rows with near-identical deleted_at, so an ORDER BY + LIMIT sweep whose
// candidate set never shrinks would re-select the SAME rows every tick and never reach
// the rest. Marking reaped rows is what makes consecutive ticks drain the backlog.
func TestSweepOnce_AdvancesPastReapedRows(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	channelID := seedAttachmentChannel(t, db, uploader)
	age := stragglerGraceSeconds + 600

	first := seedMediaFile(t, db, uploader, channelID, &age)
	second := seedMediaFile(t, db, uploader, channelID, &age)

	store := &recordingDeleter{}
	r := NewReaper(db, logger.NewWithWriter(io.Discard), store)

	r.sweepOnce(context.Background())
	require.Subset(t, store.keys, []string{first, second}, "both stragglers reaped")

	// Second tick: everything is marked, so there is nothing left to select.
	store.keys = nil
	r.sweepOnce(context.Background())
	assert.Empty(t, store.keys, "a reaped row must leave the candidate set, not be re-reaped forever")
}

// seedTier1MediaFile inserts a media_files row at the DETERMINISTIC tier-1 avatar
// key for the uploader — mirroring media.tier1StorageKey's `avatars/<userID>` — and
// returns that key. Two calls for the same uploader therefore collide on one key,
// exactly as the real avatar-replacement flow does. The key is built in SQL so the
// helper needs no Go-side string building.
func seedTier1MediaFile(t *testing.T, db *sql.DB, uploader string, deletedAgoSecs *int) string {
	t.Helper()
	var key string
	var err error
	if deletedAgoSecs == nil {
		err = db.QueryRow(`
			INSERT INTO media_files (uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
			VALUES ($1::uuid, 'photo', 1, 'image/png', 1, 'avatars/' || $2::text)
			RETURNING storage_key`, uploader, uploader).Scan(&key)
	} else {
		err = db.QueryRow(`
			INSERT INTO media_files (uploader_id, file_type, media_tier, mime_type, file_size, storage_key, deleted_at)
			VALUES ($1::uuid, 'photo', 1, 'image/png', 1, 'avatars/' || $2::text,
			        NOW() - make_interval(secs => $3))
			RETURNING storage_key`, uploader, uploader, *deletedAgoSecs).Scan(&key)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM media_files WHERE storage_key = $1`, key) })
	return key
}

// TestSweepOnce_IgnoresTier1Media is the data-loss lock, and it locks the SCOPE.
//
// The sweep exists for purge orphans, which are always tier-2 attachments. Tier-1
// media (avatars/banners/icons) has deterministic keys (`avatars/<userID>`) and its
// own cleanup at its own call sites — it was never this reaper's to touch. Widening
// the sweep to all tiers would put it in the path of live assets: tier-1 uploads
// write the object BEFORE inserting the row, so a sweep racing a re-upload would
// delete the current asset's object out from under it.
func TestSweepOnce_IgnoresTier1Media(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	channelID := seedAttachmentChannel(t, db, uploader)
	age := stragglerGraceSeconds + 600

	// The avatar-replacement shape: old row soft-deleted, new row live, SAME key.
	// (The partial unique index on storage_key permits this only because the old row
	// is soft-deleted — which is exactly why the real flow produces this state.)
	avatarKey := seedTier1MediaFile(t, db, uploader, &age)
	liveKey := seedTier1MediaFile(t, db, uploader, nil)
	require.Equal(t, avatarKey, liveKey, "tier-1 keys are deterministic: both rows share one key")

	// A genuine purge orphan: tier-2, soft-deleted, per-upload-unique key.
	orphan := seedMediaFile(t, db, uploader, channelID, &age)

	store := &recordingDeleter{}
	r := NewReaper(db, logger.NewWithWriter(io.Discard), store)
	r.sweepOnce(context.Background())

	assert.NotContains(t, store.keys, avatarKey,
		"tier-1 media is not the sweep's to reap — that key is the user's current avatar")
	assert.Contains(t, store.keys, orphan, "a tier-2 purge orphan must still be reaped")

	// Neither tier-1 row is touched at all — not deleted, and not marked (marking the
	// live row would pre-stamp it reaped and leak its object when it is soft-deleted).
	var anyMarked int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM media_files WHERE storage_key = $1 AND blob_reaped_at IS NOT NULL`,
		avatarKey).Scan(&anyMarked))
	assert.Equal(t, 0, anyMarked, "the sweep must not touch tier-1 rows at all")
}

// TestReapSweptBlob_LiveKeyGuard tests the guard DIRECTLY rather than through the
// sweep. The media_tier=2 bound means no live row can share a swept key today, so
// exercising the guard via sweepOnce would pass vacuously — a test that cannot fail.
// The guard is defense-in-depth for the day that bound is widened, so it gets a test
// that actually reaches it.
func TestReapSweptBlob_LiveKeyGuard(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	age := stragglerGraceSeconds + 600

	sharedKey := seedTier1MediaFile(t, db, uploader, &age) // soft-deleted
	seedTier1MediaFile(t, db, uploader, nil)               // live, same key

	store := &recordingDeleter{}
	r := NewReaper(db, logger.NewWithWriter(io.Discard), store)
	r.reapSweptBlob(context.Background(), sharedKey)

	assert.Empty(t, store.keys,
		"must NOT delete an object a live row still points at, even when handed the key directly")

	var softMarked, liveMarked int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FILTER (WHERE deleted_at IS NOT NULL AND blob_reaped_at IS NOT NULL),
		       count(*) FILTER (WHERE deleted_at IS NULL AND blob_reaped_at IS NOT NULL)
		FROM media_files WHERE storage_key = $1`, sharedKey).Scan(&softMarked, &liveMarked))
	assert.Equal(t, 1, softMarked, "the soft-deleted row is marked so a sweep would advance past it")
	assert.Equal(t, 0, liveMarked, "a LIVE row must never be marked reaped")
}

// failingDeleter simulates object storage rejecting the delete.
type failingDeleter struct{ calls int }

func (f *failingDeleter) DeleteObject(_ context.Context, _ string) error {
	f.calls++
	return errors.New("storage unavailable")
}

// TestSweepOnce_FailedDeleteIsRetriedNotMarked: marking is gated on a SUCCESSFUL
// delete. Recording a reap that never happened would retire the row from the sweep
// and leak the object forever — the precise failure this whole marker exists to stop.
func TestSweepOnce_FailedDeleteIsRetriedNotMarked(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	channelID := seedAttachmentChannel(t, db, uploader)
	age := stragglerGraceSeconds + 600
	key := seedMediaFile(t, db, uploader, channelID, &age)

	failing := &failingDeleter{}
	r := NewReaper(db, logger.NewWithWriter(io.Discard), failing)

	r.sweepOnce(context.Background())
	assert.Nil(t, reapedAt(t, db, key), "a failed delete must NOT be marked reaped")

	r.sweepOnce(context.Background())
	assert.Equal(t, 2, failing.calls, "the unmarked row must be retried on the next tick")
}

// TestCollectStragglers_ReturnsKeys exercises the query path directly.
func TestCollectStragglers_ReturnsKeys(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	channelID := seedAttachmentChannel(t, db, uploader)
	age := stragglerGraceSeconds + 600
	key := seedMediaFile(t, db, uploader, channelID, &age)

	r := NewReaper(db, logger.NewWithWriter(io.Discard), nil)
	keys, err := r.collectStragglers(context.Background())

	require.NoError(t, err)
	assert.Contains(t, keys, key)
}

// TestSweepOnce_ToleratesNilStore: dev/no-object-store must not panic — the metadata
// soft-delete still runs (media.CleanupObject tolerates a nil store).
func TestSweepOnce_ToleratesNilStore(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	channelID := seedAttachmentChannel(t, db, uploader)
	age := stragglerGraceSeconds + 600
	seedMediaFile(t, db, uploader, channelID, &age)

	r := NewReaper(db, logger.NewWithWriter(io.Discard), nil)
	assert.NotPanics(t, func() { r.sweepOnce(context.Background()) })
}

// TestSweepStragglers_StopsOnContextCancel: the periodic loop is a process-lifetime
// goroutine, so it must exit promptly when the app context is cancelled rather than
// leaking past shutdown.
func TestSweepStragglers_StopsOnContextCancel(t *testing.T) {
	db := sweepTestDB(t)
	r := NewReaper(db, logger.NewWithWriter(io.Discard), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.SweepStragglers(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SweepStragglers did not return after context cancellation")
	}
}

// TestCollectStragglers_QueryErrorPropagates: a closed pool surfaces an error rather
// than silently reporting zero stragglers.
func TestCollectStragglers_QueryErrorPropagates(t *testing.T) {
	db, err := sql.Open("postgres", "host=127.0.0.1 port=1 user=nobody dbname=nope sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	r := NewReaper(db, logger.NewWithWriter(io.Discard), nil)
	_, err = r.collectStragglers(context.Background())
	assert.Error(t, err)
}
