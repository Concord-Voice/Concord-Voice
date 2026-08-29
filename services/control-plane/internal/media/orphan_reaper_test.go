package media_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// fakeOrphanStore is the object-store half. It records the prefix and cutoff it
// was asked for, so a test can assert on the SCOPE of the listing as well as on
// what was deleted -- a reaper that lists the whole bucket and filters
// afterwards would pass a delete-only assertion.
type fakeOrphanStore struct {
	mu         sync.Mutex
	objects    []storage.StoredObject
	listPrefix string
	listCutoff time.Time
	listErr    error
	deleteErr  error
	deleted    []string
}

func (f *fakeOrphanStore) ListObjects(
	_ context.Context, prefix string, olderThan time.Time,
) ([]storage.StoredObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listPrefix = prefix
	f.listCutoff = olderThan
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.objects, nil
}

func (f *fakeOrphanStore) DeleteObject(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, key)
	return nil
}

func objectsFor(keys ...string) []storage.StoredObject {
	out := make([]storage.StoredObject, 0, len(keys))
	for _, k := range keys {
		out = append(out, storage.StoredObject{Key: k, Size: 1})
	}
	return out
}

// seedMediaRow inserts one tier-2 media_files row, matching what the reaper
// actually reclaims. The valid_media_context CHECK (000042, re-expressed by
// 000062) demands key_version plus exactly one of channel_id / conversation_id
// for that tier.
//
// An earlier revision of this comment claimed a tier-1 seed "would make every
// assertion pass VACUOUSLY". That was wrong and worth correcting rather than
// deleting, because it describes a coupling the code does not have:
// claimedKeysQuery carries NO media_tier predicate at all -- it matches on
// (storage_key, storage_backend) only. The reaper's tier-2 scoping is by key
// PREFIX, not by column, so a tier-1 row carrying an `attachments/` key would
// claim the object identically and every assertion would still hold for the
// right reason.
func seedMediaRow(t *testing.T, ts *testhelpers.TestServer, uploaderID, channelID, key string, backend *string) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version, channel_id,
		                         mime_type, file_size, storage_key, storage_backend)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 10, $3, $4)`,
		uploaderID, channelID, key, backend)
	require.NoError(t, err)
}

// seedChannel gives the tier-2 rows above a channel to hang off.
func seedChannel(t *testing.T, ts *testhelpers.TestServer, owner, name string) string {
	t.Helper()
	serverID := ts.CreateTestServer(t, owner, name+" server")
	return ts.CreateTestChannel(t, serverID, name)
}

// TestOrphanSweepReapsUnclaimedOnly is the core contract: an object a row
// claims survives, an object nothing claims does not.
func TestOrphanSweepReapsUnclaimedOnly(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "orphanreap1")

	channelID := seedChannel(t, ts, user.ID, "orphanreap1")
	claimed := "attachments/claimed-" + user.ID
	orphan := "attachments/orphan-" + user.ID
	seedMediaRow(t, ts, user.ID, channelID, claimed, nil)

	store := &fakeOrphanStore{objects: objectsFor(claimed, orphan)}
	reaper := media.NewOrphanReaper(ts.DB, store, string(storage.LegacyBackendID), logger.New("test"))

	res, err := reaper.SweepOrphans(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, res.Listed)
	assert.Equal(t, 1, res.Orphaned)
	assert.Equal(t, 1, res.Reaped)
	assert.Equal(t, 0, res.Failed)
	assert.Equal(t, []string{orphan}, store.deleted,
		"an object a media_files row claims must never be deleted")
}

// TestOrphanSweepSparesSoftDeletedRows pins the deliberate absence of a
// `deleted_at` predicate in claimedKeysQuery. A soft-deleted row still CLAIMS
// its object -- the straggler sweep owns it -- and two reclaimers racing the
// same object is how a blob_reaped_at marker comes to describe a delete nobody
// performed.
func TestOrphanSweepSparesSoftDeletedRows(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "orphanreap2")

	channelID := seedChannel(t, ts, user.ID, "orphanreap2")
	key := "attachments/softdeleted-" + user.ID
	seedMediaRow(t, ts, user.ID, channelID, key, nil)
	_, err := ts.DB.Exec(`UPDATE media_files SET deleted_at = NOW() WHERE storage_key = $1`, key)
	require.NoError(t, err)

	store := &fakeOrphanStore{objects: objectsFor(key)}
	reaper := media.NewOrphanReaper(ts.DB, store, string(storage.LegacyBackendID), logger.New("test"))

	res, err := reaper.SweepOrphans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.Orphaned)
	assert.Empty(t, store.deleted, "the straggler sweep owns a soft-deleted row's object")
}

// TestOrphanSweepIsPairKeyed proves the claim check is a statement about
// (bucket, key) rather than about the key alone. A row claiming this key on
// ANOTHER backend describes a different object and must not spare this one --
// sparing it is a leak in exactly the direction the delete rail exists to close.
func TestOrphanSweepIsPairKeyed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "orphanreap3")

	channelID := seedChannel(t, ts, user.ID, "orphanreap3")
	key := "attachments/pairkeyed-" + user.ID
	r2 := "r2"
	seedMediaRow(t, ts, user.ID, channelID, key, &r2)

	// Sweeping the LEGACY bucket: the only row naming this key lives on r2.
	store := &fakeOrphanStore{objects: objectsFor(key)}
	reaper := media.NewOrphanReaper(ts.DB, store, string(storage.LegacyBackendID), logger.New("test"))

	res, err := reaper.SweepOrphans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Reaped)
	assert.Equal(t, []string{key}, store.deleted,
		"a row on a different backend describes a different object and must not spare this one")

	// The mirror: sweeping r2 itself, the same row DOES claim the object.
	store2 := &fakeOrphanStore{objects: objectsFor(key)}
	reaper2 := media.NewOrphanReaper(ts.DB, store2, r2, logger.New("test"))
	res2, err := reaper2.SweepOrphans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res2.Orphaned)
	assert.Empty(t, store2.deleted)
}

// TestOrphanSweepScopeIsTier2Only is the guard against the widening that would
// blank live server icons. The reaper must ask the store only for the
// attachments prefix, and must ignore a tier-1 key even if one is handed back.
func TestOrphanSweepScopeIsTier2Only(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "orphanreap4")

	// A profile-media key with NO media_files row -- the normal state of a
	// server icon whose uploader was erased. It must not be touched.
	store := &fakeOrphanStore{objects: objectsFor(
		"server-icons/00000000-0000-0000-0000-0000000000ff",
		"avatars/"+user.ID,
	)}
	reaper := media.NewOrphanReaper(ts.DB, store, string(storage.LegacyBackendID), logger.New("test"))

	res, err := reaper.SweepOrphans(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "attachments/", store.listPrefix,
		"the listing itself must be scoped; filtering afterwards is not the same guarantee")
	assert.Equal(t, 0, res.Listed)
	assert.Empty(t, store.deleted, "tier-1 profile media is never an orphan-reaper candidate")
}

// TestOrphanSweepAppliesWriteRaceMargin pins the safety margin. Both attachment
// write paths put the OBJECT down before inserting its ROW, so an object
// younger than the margin may simply be one whose row is moments away.
func TestOrphanSweepAppliesWriteRaceMargin(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	store := &fakeOrphanStore{}
	reaper := media.NewOrphanReaper(ts.DB, store, string(storage.LegacyBackendID), logger.New("test"))

	before := time.Now()
	_, err := reaper.SweepOrphans(context.Background())
	require.NoError(t, err)

	assert.False(t, store.listCutoff.IsZero(), "the sweep must pass an age cutoff, not the zero time")
	assert.True(t, store.listCutoff.Before(before.Add(-23*time.Hour)),
		"cutoff %s leaves less than the write-race margin behind %s", store.listCutoff, before)
}

// TestOrphanSweepReportsTotallyFailedBatch: counting successes alone lets a
// completely broken reaper report as a clean one.
func TestOrphanSweepReportsTotallyFailedBatch(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "orphanreap5")

	store := &fakeOrphanStore{
		objects:   objectsFor("attachments/orphan-" + user.ID),
		deleteErr: errors.New("access denied"),
	}
	reaper := media.NewOrphanReaper(ts.DB, store, string(storage.LegacyBackendID), logger.New("test"))

	res, err := reaper.SweepOrphans(context.Background())
	require.Error(t, err, "a batch in which every delete failed is not a successful sweep")
	assert.Equal(t, 1, res.Orphaned)
	assert.Equal(t, 1, res.Failed)
	assert.Equal(t, 0, res.Reaped)
}

// TestOrphanSweepFailsClosedOnListError: an unreadable bucket proves nothing,
// so nothing may be deleted on the strength of it.
func TestOrphanSweepFailsClosedOnListError(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	store := &fakeOrphanStore{listErr: errors.New("bucket unreachable")}
	reaper := media.NewOrphanReaper(ts.DB, store, string(storage.LegacyBackendID), logger.New("test"))

	_, err := reaper.SweepOrphans(context.Background())
	require.Error(t, err)
	assert.Empty(t, store.deleted)
}
