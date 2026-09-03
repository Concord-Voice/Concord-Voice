package purge

// Database-backed tests for the purge Engine (#1352). These exercise Run,
// deleteBatched, and the audit lifecycle DIRECTLY rather than through the
// messages/dm handlers: CI measures coverage per package with no -coverpkg, so
// cross-package exercise is not attributed here — and the engine is the core of a
// destructive, irreversible feature, so it warrants tests of its own regardless.
//
// Skipped when DATABASE_URL is unset (CI sets it).

import (
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// engineFixture is a seeded server/channel with a known author.
type engineFixture struct {
	db        *sql.DB
	channelID string
	serverID  string
	authorID  string
	otherID   string
}

func seedEngineFixture(t *testing.T) engineFixture {
	t.Helper()
	db := sweepTestDB(t) // skips without DATABASE_URL

	author := seedUploader(t, db) // reuses the users seeder from reaper_sweep_test.go
	other := seedUploader(t, db)

	var serverID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO servers (name, owner_id) VALUES ('purge-engine-test', $1) RETURNING id`,
		author).Scan(&serverID))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID) })

	var channelID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO channels (server_id, name) VALUES ($1, 'general') RETURNING id`,
		serverID).Scan(&channelID))

	return engineFixture{db: db, channelID: channelID, serverID: serverID, authorID: author, otherID: other}
}

// seedMessages inserts n messages by the given author, each aged agoSecs seconds.
func (f engineFixture) seedMessages(t *testing.T, author string, n, agoSecs int) {
	t.Helper()
	_, err := f.db.Exec(`
		INSERT INTO messages (channel_id, user_id, content, created_at)
		SELECT $1, $2, 'msg-' || g, NOW() - make_interval(secs => $4)
		FROM generate_series(1, $3) g`, f.channelID, author, n, agoSecs)
	require.NoError(t, err)
}

func (f engineFixture) seedAttachedMessage(t *testing.T, author string, backend *string) (messageID, fileID, storageKey string) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO messages (channel_id, user_id, content)
		VALUES ($1, $2, 'attachment-message') RETURNING id`, f.channelID, author).Scan(&messageID))
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         channel_id, mime_type, file_size, storage_key)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text)
		RETURNING id, storage_key`, author, f.channelID).Scan(&fileID, &storageKey))
	if backend != nil {
		_, err := f.db.Exec(`UPDATE media_files SET storage_backend = $2 WHERE id = $1`, fileID, *backend)
		require.NoError(t, err)
	}
	_, err := f.db.Exec(`
		INSERT INTO message_attachments (message_id, file_id, position)
		VALUES ($1, $2, 0)`, messageID, fileID)
	require.NoError(t, err)
	return
}

func (f engineFixture) countMessages(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM messages WHERE channel_id = $1`, f.channelID).Scan(&n))
	return n
}

func (f engineFixture) newEngine(maxBatch int) *Engine {
	log := logger.NewWithWriter(io.Discard)
	return NewEngine(f.db, log, NewReaper(f.db, log, nil), maxBatch)
}

// channelPlan builds an all-time, all-authors channel purge plan.
func (f engineFixture) channelPlan() Plan {
	return Plan{
		ContextType: ContextChannel,
		ContextID:   f.channelID,
		ServerID:    &f.serverID,
		ActorID:     f.authorID,
		Reason:      "manual",
		Deletes: []DeleteSpec{{
			MessagesTable:    "messages",
			ScopeColumn:      "channel_id",
			ScopeID:          f.channelID,
			AttachmentsTable: "message_attachments",
		}},
	}
}

// auditRow reads the single audit row written for this fixture's context.
func (f engineFixture) auditRow(t *testing.T) (status string, deleted, hidden int) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(
		`SELECT status, deleted_count, hidden_count FROM message_purges WHERE context_id = $1`,
		f.channelID).Scan(&status, &deleted, &hidden))
	return
}

func TestEngineRun_DeletesAllAndCompletesAudit(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 3, 0)
	f.seedMessages(t, f.otherID, 2, 0)
	require.Equal(t, 5, f.countMessages(t))

	res, err := f.newEngine(5000).Run(context.Background(), f.channelPlan())

	require.NoError(t, err)
	assert.Equal(t, 5, res.DeletedCount)
	assert.NotEmpty(t, res.PurgeID)
	assert.Equal(t, 0, f.countMessages(t))

	status, deleted, hidden := f.auditRow(t)
	assert.Equal(t, "completed", status)
	assert.Equal(t, 5, deleted)
	assert.Equal(t, 0, hidden, "engine leaves hidden_count to the DM caller")
}

func TestEngineDeleteOne_SerializesWithConcurrentAttachmentLink(t *testing.T) {
	f := seedEngineFixture(t)
	var messageID, fileID string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO messages (channel_id, user_id, content)
		VALUES ($1, $2, 'attachment-link-race') RETURNING id`, f.channelID, f.authorID).Scan(&messageID))
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         channel_id, mime_type, file_size, storage_key)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text)
		RETURNING id`, f.authorID, f.channelID).Scan(&fileID))

	linkTx, err := f.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = linkTx.Rollback() }()
	var lockedMessage string
	require.NoError(t, linkTx.QueryRow(
		`SELECT id FROM messages WHERE id = $1 FOR KEY SHARE`, messageID).Scan(&lockedMessage))
	var lockedFile string
	require.NoError(t, linkTx.QueryRow(
		`SELECT id FROM media_files WHERE id = $1 FOR KEY SHARE`, fileID).Scan(&lockedFile))
	_, err = linkTx.Exec(`
		INSERT INTO message_attachments (message_id, file_id, position)
		VALUES ($1, $2, 0)`, messageID, fileID)
	require.NoError(t, err)
	var linkTxID int64
	require.NoError(t, linkTx.QueryRow(`SELECT txid_current()`).Scan(&linkTxID))

	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() {
		if closeErr := probe.Close(); closeErr != nil {
			t.Errorf("close lock probe: %v", closeErr)
		}
	})
	deleteCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- f.newEngine(5000).DeleteOne(deleteCtx, messageID, f.channelPlan().Deletes[0])
	}()
	testdb.WaitForRowLockWaiter(t, probe, linkTxID)

	require.NoError(t, linkTx.Commit())
	require.NoError(t, <-done)

	var deletedAt sql.NullTime
	require.NoError(t, f.db.QueryRow(
		`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.True(t, deletedAt.Valid, "final-reference media must be retired after the link commits")
	var bridgeCount int
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM message_attachments WHERE message_id = $1 AND file_id = $2`, messageID, fileID).Scan(&bridgeCount))
	assert.Zero(t, bridgeCount, "the parent delete must remove the committed attachment bridge")
}

// TestEngineRun_LoopsUntilDrained proves the batch stride is not a cap: with
// maxBatch=2 and 7 rows the loop must run until the predicate matches nothing.
func TestEngineRun_LoopsUntilDrained(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 7, 0)

	res, err := f.newEngine(2).Run(context.Background(), f.channelPlan())

	require.NoError(t, err)
	assert.Equal(t, 7, res.DeletedCount, "all rows deleted despite a stride of 2")
	assert.Equal(t, 0, f.countMessages(t))
}

// TestEngineRun_AuthorFilterScopesDelete locks the by-author filter that the
// ManageOwn-forced-self path and #1353's purge-on-ban both depend on.
func TestEngineRun_AuthorFilterScopesDelete(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 3, 0)
	f.seedMessages(t, f.otherID, 2, 0)

	plan := f.channelPlan()
	plan.Deletes[0].Author = &f.authorID

	res, err := f.newEngine(5000).Run(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 3, res.DeletedCount)
	assert.Equal(t, 2, f.countMessages(t), "the other author's messages survive")
}

// TestEngineRun_TimeRangeScopesDelete locks the recent-ward window against the
// messages table, whose created_at is TIMESTAMP (not TIMESTAMPTZ) — the per-table
// cast exists precisely so this boundary is not shifted by the session timezone.
func TestEngineRun_TimeRangeScopesDelete(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 2, 0)          // now
	f.seedMessages(t, f.authorID, 3, 8*24*60*60) // 8 days ago

	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	plan := f.channelPlan()
	plan.RangeFrom = &cutoff

	res, err := f.newEngine(5000).Run(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 2, res.DeletedCount, "only messages inside the window are deleted")
	assert.Equal(t, 3, f.countMessages(t), "messages older than the cutoff survive")
}

// TestEngineRun_RejectsIllegalIdentifiersBeforeAnyWrite locks the SQL-injection
// gate: a spec outside the allow-list must abort Run before the audit row exists.
func TestEngineRun_RejectsIllegalIdentifiersBeforeAnyWrite(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 2, 0)

	plan := f.channelPlan()
	plan.Deletes[0].MessagesTable = "users" // not allow-listed

	res, err := f.newEngine(5000).Run(context.Background(), plan)

	require.Error(t, err)
	assert.Zero(t, res.DeletedCount)
	assert.Equal(t, 2, f.countMessages(t), "nothing deleted")

	var audits int
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM message_purges WHERE context_id = $1`, f.channelID).Scan(&audits))
	assert.Equal(t, 0, audits, "illegal spec must abort before the audit row is written")
}

// TestEngineRun_MultipleDeleteSpecs covers the server-purge fan-out shape: one spec
// per channel, aggregated into a single audit row.
func TestEngineRun_MultipleDeleteSpecs(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 2, 0)

	var second string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO channels (server_id, name) VALUES ($1, 'second') RETURNING id`,
		f.serverID).Scan(&second))
	_, err := f.db.Exec(`
		INSERT INTO messages (channel_id, user_id, content) VALUES ($1, $2, 'other-channel')`,
		second, f.authorID)
	require.NoError(t, err)

	plan := f.channelPlan()
	plan.ContextType = ContextServer
	plan.Deletes = append(plan.Deletes, DeleteSpec{
		MessagesTable:    "messages",
		ScopeColumn:      "channel_id",
		ScopeID:          second,
		AttachmentsTable: "message_attachments",
	})

	res, err := f.newEngine(5000).Run(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 3, res.DeletedCount, "counts aggregate across specs")
	assert.Equal(t, 0, f.countMessages(t))
}

// TestEngineRun_PartialDeleteIsAudited locks the audit's honesty when a purge is
// interrupted after some batches have already committed (client disconnect, DB blip).
// The rows are irreversibly gone, so `in_progress, deleted_count=0` would be a false
// compliance record that UNDERSTATES the deletion — the worse direction for Art.17.
//
// The failure is forced with a second spec whose identifiers are allow-list-valid but
// whose scope VALUE is not a uuid, so the query errors after spec 1 has committed.
func TestEngineRun_PartialDeleteIsAudited(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 3, 0)

	plan := f.channelPlan()
	plan.Deletes = append(plan.Deletes, DeleteSpec{
		MessagesTable:    "messages",
		ScopeColumn:      "channel_id",
		ScopeID:          "not-a-uuid", // valid identifiers, invalid value → query error
		AttachmentsTable: "message_attachments",
	})

	res, err := f.newEngine(5000).Run(context.Background(), plan)

	require.Error(t, err, "the failing spec must surface an error")
	assert.Equal(t, 3, res.DeletedCount, "the caller is told what was actually deleted")
	assert.Equal(t, 0, f.countMessages(t), "spec 1's deletes are committed and irreversible")

	status, deleted, _ := f.auditRow(t)
	assert.Equal(t, "in_progress", status, "an interrupted purge must not claim completion")
	assert.Equal(t, 3, deleted, "the audit must record the rows actually deleted, not 0")
}

func TestEngineDurableDeletedCount_ReadsAuditOrFallsBack(t *testing.T) {
	f := seedEngineFixture(t)
	e := f.newEngine(5000)
	purgeID, err := e.writeAuditInProgress(context.Background(), f.channelPlan())
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE message_purges SET deleted_count = 4 WHERE id = $1`, purgeID)
	require.NoError(t, err)

	assert.Equal(t, 4, e.durableDeletedCount(context.Background(), purgeID, 3))
	assert.Equal(t, 3, e.durableDeletedCount(context.Background(), f.serverID, 3))
}

// A batch's audit increment must commit with its delete. Otherwise an audit-update
// failure could leave irreversible deletion with an understated audit row.
func TestEngineRun_RollsBackDeleteWhenBatchAuditUpdateFails(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 3, 0)

	_, err := f.db.Exec(`
		CREATE FUNCTION test_reject_purge_batch_audit_update() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced purge audit update failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_purge_batch_audit_update
		BEFORE UPDATE OF deleted_count ON message_purges
		FOR EACH ROW EXECUTE FUNCTION test_reject_purge_batch_audit_update()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := f.db.Exec(`
			DROP TRIGGER IF EXISTS test_reject_purge_batch_audit_update ON message_purges;
			DROP FUNCTION IF EXISTS test_reject_purge_batch_audit_update()`)
		if cleanupErr != nil {
			t.Errorf("cleanup batch audit failure trigger: %v", cleanupErr)
		}
	})

	res, err := f.newEngine(5000).Run(context.Background(), f.channelPlan())
	require.Error(t, err)
	assert.Zero(t, res.DeletedCount)
	assert.Equal(t, 3, f.countMessages(t), "a failed audit increment must roll back the matching delete")

	status, deleted, _ := f.auditRow(t)
	assert.Equal(t, "in_progress", status)
	assert.Zero(t, deleted)
}

// TestEngineFinalizeHidden covers the DM receiver-hide audit enrichment.
func TestEngineFinalizeHidden(t *testing.T) {
	f := seedEngineFixture(t)
	f.seedMessages(t, f.authorID, 1, 0)
	e := f.newEngine(5000)

	res, err := e.Run(context.Background(), f.channelPlan())
	require.NoError(t, err)

	require.NoError(t, e.FinalizeHidden(context.Background(), res.PurgeID, 4))

	_, _, hidden := f.auditRow(t)
	assert.Equal(t, 4, hidden)
}

// TestEngineRun_EmptyContextIsNoOp: purging a context with nothing to delete still
// completes cleanly and audits a zero count (no error, no partial state).
func TestEngineRun_EmptyContextIsNoOp(t *testing.T) {
	f := seedEngineFixture(t)

	res, err := f.newEngine(5000).Run(context.Background(), f.channelPlan())

	require.NoError(t, err)
	assert.Equal(t, 0, res.DeletedCount)
	status, deleted, _ := f.auditRow(t)
	assert.Equal(t, "completed", status)
	assert.Equal(t, 0, deleted)
}

// TestEngineRun_DMMessagesSpec exercises the second allow-listed store, whose
// created_at is TIMESTAMPTZ and therefore takes the other per-table cast.
func TestEngineRun_DMMessagesSpec(t *testing.T) {
	db := sweepTestDB(t)
	alice := seedUploader(t, db)
	bob := seedUploader(t, db)

	var convID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES (false, false, $1) RETURNING id`, alice).Scan(&convID))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, convID) })

	for _, u := range []string{alice, bob} {
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, u)
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO dm_messages (conversation_id, user_id, content, type)
			VALUES ($1, $2, 'dm-msg', 'text')`, convID, u)
		require.NoError(t, err)
	}

	log := logger.NewWithWriter(io.Discard)
	e := NewEngine(db, log, NewReaper(db, log, nil), 5000)

	res, err := e.Run(context.Background(), Plan{
		ContextType: ContextDM,
		ContextID:   convID,
		ActorID:     alice,
		Reason:      "manual",
		Deletes: []DeleteSpec{{
			MessagesTable:    "dm_messages",
			ScopeColumn:      "conversation_id",
			ScopeID:          convID,
			AttachmentsTable: "dm_message_attachments",
			Author:           &alice,
		}},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, res.DeletedCount, "only alice's DM message is deleted")

	var remaining int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, convID).Scan(&remaining))
	assert.Equal(t, 1, remaining, "bob's message survives the author-scoped delete")
}

func TestCaptureConversationBlobsTx_ReturnsTier2ConversationMediaForBridgeAudit(t *testing.T) {
	db := sweepTestDB(t)
	uploader := seedUploader(t, db)
	var conversationID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES (true, false, $1) RETURNING id`, uploader).Scan(&conversationID))
	t.Cleanup(func() {
		if _, cleanupErr := db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, conversationID); cleanupErr != nil {
			t.Errorf("cleanup captured conversation: %v", cleanupErr)
		}
	})

	var liveID, liveKey, r2ID, r2Key, unreapedID, unreapedKey, reapedID, reapedKey string
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text)
		RETURNING id, storage_key`, uploader, conversationID).Scan(&liveID, &liveKey))
	r2 := "r2-useast"
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key, storage_backend)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text, $3)
		RETURNING id, storage_key`, uploader, conversationID, r2).Scan(&r2ID, &r2Key))
	deletedKey := "attachments/capture-deleted"
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key, deleted_at)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1, $3, NOW())
		RETURNING id`,
		uploader, conversationID, deletedKey).Scan(&unreapedID))
	unreapedKey = deletedKey
	reapedKey = "attachments/capture-reaped"
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key,
		                         deleted_at, blob_reaped_at)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1, $3, NOW(), NOW())
		RETURNING id`, uploader, conversationID, reapedKey).Scan(&reapedID))
	channelID := seedAttachmentChannel(t, db, uploader)
	nonConversationKey := seedMediaFile(t, db, uploader, channelID, nil)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			t.Errorf("rollback capture transaction: %v", rollbackErr)
		}
	}()

	fileIDs, refs, err := (&Engine{}).CaptureConversationBlobsTx(context.Background(), tx, conversationID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{liveID, r2ID, unreapedID, reapedID}, fileIDs)
	byKey := map[string]*string{}
	for _, ref := range refs {
		byKey[ref.Key] = ref.Backend
	}
	assert.Contains(t, byKey, liveKey)
	assert.Nil(t, byKey[liveKey])
	if assert.NotNil(t, byKey[r2Key]) {
		assert.Equal(t, r2, *byKey[r2Key])
	}
	assert.Contains(t, byKey, unreapedKey)
	assert.NotContains(t, byKey, reapedKey)
	assert.NotContains(t, byKey, nonConversationKey)
}

func TestCaptureConversationBlobsTx_RequiresActiveTransaction(t *testing.T) {
	e := &Engine{}
	_, _, err := e.CaptureConversationBlobsTx(context.Background(), nil, "conversation")
	assert.EqualError(t, err, "purge: capture conversation blobs requires a transaction")

	db := sweepTestDB(t)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	_, _, err = e.CaptureConversationBlobsTx(context.Background(), tx, "conversation")
	assert.ErrorIs(t, err, sql.ErrTxDone)
}

func TestEngineRun_PreservesSharedAttachmentUntilFinalReference(t *testing.T) {
	f := seedEngineFixture(t)
	_, fileID, key := f.seedAttachedMessage(t, f.authorID, nil)
	var secondID string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO messages (channel_id, user_id, content)
		VALUES ($1, $2, 'shared-attachment-message') RETURNING id`, f.channelID, f.otherID).Scan(&secondID))
	_, err := f.db.Exec(`INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, 1)`, secondID, fileID)
	require.NoError(t, err)

	e := f.newEngine(5000)
	firstPlan := f.channelPlan()
	firstPlan.Deletes[0].Author = &f.authorID
	res, err := e.Run(context.Background(), firstPlan)
	require.NoError(t, err)
	assert.Equal(t, 1, res.DeletedCount)

	var deletedAt *time.Time
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "a shared attachment must remain live while another message references it")
	assert.Empty(t, e.reaper.jobs, "shared attachment must not be queued before its final reference is deleted")

	secondPlan := f.channelPlan()
	secondPlan.Deletes[0].Author = &f.otherID
	res, err = e.Run(context.Background(), secondPlan)
	require.NoError(t, err)
	assert.Equal(t, 1, res.DeletedCount)
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.NotNil(t, deletedAt)
	select {
	case ref := <-e.reaper.jobs:
		assert.Equal(t, key, ref.Key)
		assert.Nil(t, ref.Backend)
	default:
		t.Fatal("final-reference attachment was not queued")
	}
}

func TestEngineRun_QueuesExactLegacyAndR2Backends(t *testing.T) {
	f := seedEngineFixture(t)
	r2 := "r2-useast"
	_, _, legacyKey := f.seedAttachedMessage(t, f.authorID, nil)
	_, _, r2Key := f.seedAttachedMessage(t, f.authorID, &r2)

	e := f.newEngine(5000)
	res, err := e.Run(context.Background(), f.channelPlan())
	require.NoError(t, err)
	assert.Equal(t, 2, res.DeletedCount)

	got := map[string]*string{}
	for len(got) < 2 {
		select {
		case ref := <-e.reaper.jobs:
			got[ref.Key] = ref.Backend
		default:
			t.Fatal("expected both final-reference attachments in the reaper queue")
		}
	}
	assert.Contains(t, got, legacyKey)
	assert.Nil(t, got[legacyKey])
	assert.Contains(t, got, r2Key)
	if assert.NotNil(t, got[r2Key]) {
		assert.Equal(t, r2, *got[r2Key])
	}
}

func TestDeleteOne_PreservesCrossBridgeReference(t *testing.T) {
	f := seedEngineFixture(t)
	messageID, fileID, key := f.seedAttachedMessage(t, f.authorID, nil)
	var conversationID, dmMessageID string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES (false, false, $1) RETURNING id`, f.authorID).Scan(&conversationID))
	t.Cleanup(func() {
		if _, cleanupErr := f.db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, conversationID); cleanupErr != nil {
			t.Errorf("cleanup cross-bridge conversation: %v", cleanupErr)
		}
	})
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type)
		VALUES ($1, $2, 'cross-bridge-reference', 'text') RETURNING id`, conversationID, f.authorID).Scan(&dmMessageID))
	_, err := f.db.Exec(`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, 0)`, dmMessageID, fileID)
	require.NoError(t, err)

	e := f.newEngine(5000)
	channelSpec := DeleteSpec{MessagesTable: "messages", ScopeColumn: "channel_id", ScopeID: f.channelID, AttachmentsTable: "message_attachments"}
	require.NoError(t, e.DeleteOne(context.Background(), messageID, channelSpec))

	var deletedAt *time.Time
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "a DM bridge reference must protect a channel attachment")
	assert.Empty(t, e.reaper.jobs)

	dmSpec := DeleteSpec{MessagesTable: "dm_messages", ScopeColumn: "conversation_id", ScopeID: conversationID, AttachmentsTable: "dm_message_attachments"}
	require.NoError(t, e.DeleteOne(context.Background(), dmMessageID, dmSpec))
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.NotNil(t, deletedAt)
	select {
	case ref := <-e.reaper.jobs:
		assert.Equal(t, key, ref.Key)
		assert.Nil(t, ref.Backend)
	default:
		t.Fatal("final cross-bridge reference was not queued")
	}
}

func TestDeleteOne_QueuesExactBackendAfterCommit(t *testing.T) {
	f := seedEngineFixture(t)
	r2 := "r2-useast"
	legacyMessageID, _, legacyKey := f.seedAttachedMessage(t, f.authorID, nil)
	r2MessageID, _, r2Key := f.seedAttachedMessage(t, f.authorID, &r2)
	e := f.newEngine(5000)
	spec := func() DeleteSpec {
		return DeleteSpec{MessagesTable: "messages", ScopeColumn: "channel_id", ScopeID: f.channelID, AttachmentsTable: "message_attachments"}
	}
	require.NoError(t, e.DeleteOne(context.Background(), legacyMessageID, spec()))
	require.NoError(t, e.DeleteOne(context.Background(), r2MessageID, spec()))

	refs := map[string]*string{}
	for len(refs) < 2 {
		select {
		case ref := <-e.reaper.jobs:
			refs[ref.Key] = ref.Backend
		default:
			t.Fatal("expected both committed deletes in the reaper queue")
		}
	}
	assert.Contains(t, refs, legacyKey)
	assert.Nil(t, refs[legacyKey])
	assert.Contains(t, refs, r2Key)
	if assert.NotNil(t, refs[r2Key]) {
		assert.Equal(t, r2, *refs[r2Key])
	}

	var conversationID, legacyDMMessageID, r2DMMessageID string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES (false, false, $1) RETURNING id`, f.authorID).Scan(&conversationID))
	t.Cleanup(func() {
		if _, cleanupErr := f.db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, conversationID); cleanupErr != nil {
			t.Errorf("cleanup DM backend fixture: %v", cleanupErr)
		}
	})
	for _, messageID := range []*string{&legacyDMMessageID, &r2DMMessageID} {
		require.NoError(t, f.db.QueryRow(`
			INSERT INTO dm_messages (conversation_id, user_id, content)
			VALUES ($1, $2, 'backend-delete-test') RETURNING id`, conversationID, f.authorID).Scan(messageID))
	}
	var legacyDMKey, r2DMKey string
	var legacyDMFileID, r2DMFileID string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text)
		RETURNING id, storage_key`, f.authorID, conversationID).Scan(&legacyDMFileID, &legacyDMKey))
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key, storage_backend)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text, $3)
		RETURNING id, storage_key`, f.authorID, conversationID, r2).Scan(&r2DMFileID, &r2DMKey))
	_, err := f.db.Exec(`
		INSERT INTO dm_message_attachments (message_id, file_id, position)
		VALUES ($1, $2, 0), ($3, $4, 0)`, legacyDMMessageID, legacyDMFileID, r2DMMessageID, r2DMFileID)
	require.NoError(t, err)
	dmSpec := DeleteSpec{
		MessagesTable: "dm_messages", ScopeColumn: "conversation_id", ScopeID: conversationID,
		AttachmentsTable: "dm_message_attachments",
	}
	require.NoError(t, e.DeleteOne(context.Background(), legacyDMMessageID, dmSpec))
	require.NoError(t, e.DeleteOne(context.Background(), r2DMMessageID, dmSpec))
	dmRefs := map[string]*string{}
	for len(dmRefs) < 2 {
		select {
		case ref := <-e.reaper.jobs:
			dmRefs[ref.Key] = ref.Backend
		default:
			t.Fatal("expected both committed DM deletes in the reaper queue")
		}
	}
	assert.Contains(t, dmRefs, legacyDMKey)
	assert.Nil(t, dmRefs[legacyDMKey])
	assert.Contains(t, dmRefs, r2DMKey)
	if assert.NotNil(t, dmRefs[r2DMKey]) {
		assert.Equal(t, r2, *dmRefs[r2DMKey])
	}
}

func TestDeleteOne_RollsBackAndDoesNotEnqueueOnRetirementFailure(t *testing.T) {
	f := seedEngineFixture(t)
	messageID, fileID, _ := f.seedAttachedMessage(t, f.authorID, nil)
	_, err := f.db.Exec(`
		CREATE FUNCTION test_reject_single_media_retirement() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced media retirement failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_single_media_retirement
		BEFORE UPDATE OF deleted_at ON media_files
		FOR EACH ROW EXECUTE FUNCTION test_reject_single_media_retirement()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := f.db.Exec(`
			DROP TRIGGER IF EXISTS test_reject_single_media_retirement ON media_files;
			DROP FUNCTION IF EXISTS test_reject_single_media_retirement()`)
		if cleanupErr != nil {
			t.Errorf("cleanup single retirement failure trigger: %v", cleanupErr)
		}
	})

	e := f.newEngine(5000)
	spec := DeleteSpec{MessagesTable: "messages", ScopeColumn: "channel_id", ScopeID: f.channelID, AttachmentsTable: "message_attachments"}
	require.Error(t, e.DeleteOne(context.Background(), messageID, spec))
	var messageCount int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, messageID).Scan(&messageCount))
	assert.Equal(t, 1, messageCount, "retirement failure must roll back the parent delete")
	var deletedAt *time.Time
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.Nil(t, deletedAt)
	assert.Empty(t, e.reaper.jobs, "a failed transaction must not enqueue a blob")
}

func TestDeleteOne_CommitFailureRollsBackAndDoesNotEnqueue(t *testing.T) {
	f := seedEngineFixture(t)
	messageID, fileID, _ := f.seedAttachedMessage(t, f.authorID, nil)
	_, err := f.db.Exec(`
		CREATE FUNCTION test_reject_single_delete_commit() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced single delete commit failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER test_reject_single_delete_commit
		AFTER DELETE ON messages
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION test_reject_single_delete_commit()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := f.db.Exec(`
			DROP TRIGGER IF EXISTS test_reject_single_delete_commit ON messages;
			DROP FUNCTION IF EXISTS test_reject_single_delete_commit()`)
		if cleanupErr != nil {
			t.Errorf("cleanup single delete commit failure trigger: %v", cleanupErr)
		}
	})

	e := f.newEngine(5000)
	spec := DeleteSpec{MessagesTable: "messages", ScopeColumn: "channel_id", ScopeID: f.channelID, AttachmentsTable: "message_attachments"}
	require.ErrorContains(t, e.DeleteOne(context.Background(), messageID, spec), "commit single delete")
	var messageCount, bridgeCount int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, messageID).Scan(&messageCount))
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM message_attachments WHERE message_id = $1 AND file_id = $2`, messageID, fileID).Scan(&bridgeCount))
	assert.Equal(t, 1, messageCount, "commit failure must roll back the parent delete")
	assert.Equal(t, 1, bridgeCount, "commit failure must retain the attachment bridge")
	var deletedAt *time.Time
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "commit failure must not retire media")
	assert.Empty(t, e.reaper.jobs, "a failed commit must not enqueue a blob")
}
