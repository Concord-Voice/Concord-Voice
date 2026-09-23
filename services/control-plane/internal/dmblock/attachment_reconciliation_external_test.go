package dmblock_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func TestReconcileDue_ReapsEmptyConversationAttachmentAfterCommit(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversation, fileID, key := seedEmptyConversationAttachmentFixture(t, db)
	log := logger.NewWithWriter(io.Discard)
	store := &recordingEmptyConversationStore{
		db: db, conversationID: conversation, keys: make(chan string, 1), committed: make(chan bool, 1),
	}
	reaper := purge.NewReaper(db, log, store)
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reaper.StartWorker(workerCtx)
	retirer := newPurgeAttachmentRetirer(purge.NewEngine(db, log, reaper, 5000))
	reconciler := dmblock.New(db, log)
	reconciler.SetPurgeEngine(retirer)

	count, err := reconciler.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	select {
	case got := <-store.keys:
		require.Equal(t, key, got)
	case <-time.After(5 * time.Second):
		t.Fatal("empty-conversation attachment was not sent to the delete store")
	}
	select {
	case committed := <-store.committed:
		require.True(t, committed, "object deletion must observe the committed conversation delete")
	case <-time.After(5 * time.Second):
		t.Fatal("empty-conversation attachment commit observation was not recorded")
	}
	assertEmptyConversationRetired(t, db, conversation, fileID, 0)
}

func TestReconcileDue_DoesNotEnqueueEmptyConversationAttachmentBeforeCommit(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversation, fileID, _ := seedEmptyConversationAttachmentFixture(t, db)
	_, err := db.Exec(`
		CREATE FUNCTION test_reject_empty_conversation_delete() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced empty conversation delete failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_empty_conversation_delete
		BEFORE DELETE ON dm_conversations
		FOR EACH ROW EXECUTE FUNCTION test_reject_empty_conversation_delete()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.Exec(`
			DROP TRIGGER IF EXISTS test_reject_empty_conversation_delete ON dm_conversations;
			DROP FUNCTION IF EXISTS test_reject_empty_conversation_delete()`)
		if cleanupErr != nil {
			t.Errorf("cleanup empty conversation delete trigger: %v", cleanupErr)
		}
	})

	log := logger.NewWithWriter(io.Discard)
	store := &recordingEmptyConversationStore{
		db: db, conversationID: conversation, keys: make(chan string, 1), committed: make(chan bool, 1),
	}
	reaper := purge.NewReaper(db, log, store)
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reaper.StartWorker(workerCtx)
	retirer := newPurgeAttachmentRetirer(purge.NewEngine(db, log, reaper, 5000))
	reconciler := dmblock.New(db, log)
	reconciler.SetPurgeEngine(retirer)

	count, err := reconciler.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Zero(t, count)
	assertEmptyConversationRetired(t, db, conversation, fileID, 1)
	select {
	case refs := <-retirer.enqueued:
		t.Fatalf("failed transaction queued object deletion for %+v", refs)
	default:
	}
}

func TestReconcileDue_RefusesCrossContextAttachmentBridge(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	conversation, fileID, _ := seedEmptyConversationAttachmentFixture(t, db)
	var uploader, serverID, channelID, messageID string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, conversation).Scan(&uploader))
	require.NoError(t, db.QueryRow(`INSERT INTO servers (name, owner_id) VALUES ('dmblock bridge', $1) RETURNING id`, uploader).Scan(&serverID))
	require.NoError(t, db.QueryRow(`INSERT INTO channels (server_id, name) VALUES ($1, 'bridge') RETURNING id`, serverID).Scan(&channelID))
	require.NoError(t, db.QueryRow(`INSERT INTO messages (channel_id, user_id, content) VALUES ($1, $2, 'cross-context bridge') RETURNING id`, channelID, uploader).Scan(&messageID))
	_, err := db.Exec(`INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, 0)`, messageID, fileID)
	require.NoError(t, err)

	log := logger.NewWithWriter(io.Discard)
	retirer := newPurgeAttachmentRetirer(purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000))
	reconciler := dmblock.New(db, log)
	reconciler.SetPurgeEngine(retirer)
	count, err := reconciler.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Zero(t, count)
	assertEmptyConversationRetired(t, db, conversation, fileID, 1)
	var serverMessageCount, channelBridgeCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, messageID).Scan(&serverMessageCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_attachments WHERE message_id = $1 AND file_id = $2`, messageID, fileID).Scan(&channelBridgeCount))
	require.Equal(t, 1, serverMessageCount)
	require.Equal(t, 1, channelBridgeCount)
	select {
	case refs := <-retirer.enqueued:
		t.Fatalf("cross-context attachment bridge queued object deletion for %+v", refs)
	default:
	}
}

type recordingEmptyConversationStore struct {
	db             *sql.DB
	conversationID string
	keys           chan string
	committed      chan bool
}

type purgeAttachmentRetirer struct {
	engine   *purge.Engine
	enqueued chan []dmblock.AttachmentBlobRef
}

func newPurgeAttachmentRetirer(engine *purge.Engine) *purgeAttachmentRetirer {
	return &purgeAttachmentRetirer{engine: engine, enqueued: make(chan []dmblock.AttachmentBlobRef, 1)}
}

func (r *purgeAttachmentRetirer) CaptureConversationBlobsTx(
	ctx context.Context, tx *sql.Tx, conversationID string,
) ([]string, []dmblock.AttachmentBlobRef, error) {
	fileIDs, refs, err := r.engine.CaptureConversationBlobsTx(ctx, tx, conversationID)
	if err != nil {
		return nil, nil, err
	}
	converted := make([]dmblock.AttachmentBlobRef, 0, len(refs))
	for _, ref := range refs {
		converted = append(converted, dmblock.AttachmentBlobRef{Key: ref.Key, Backend: ref.Backend})
	}
	return fileIDs, converted, nil
}

func (r *purgeAttachmentRetirer) EnqueueBlobDeletes(refs []dmblock.AttachmentBlobRef) {
	r.enqueued <- append([]dmblock.AttachmentBlobRef(nil), refs...)
	converted := make([]media.BlobRef, 0, len(refs))
	for _, ref := range refs {
		converted = append(converted, media.BlobRef{Key: ref.Key, Backend: ref.Backend})
	}
	r.engine.EnqueueBlobDeletes(converted)
}

func (s *recordingEmptyConversationStore) ResolveDeleter(backend *string) (media.ObjectDeleter, error) {
	if backend == nil || *backend != "r2-useast" {
		return nil, fmt.Errorf("unexpected empty-conversation backend %v", backend)
	}
	return s, nil
}

func (s *recordingEmptyConversationStore) DeleteObject(_ context.Context, key string) error {
	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM dm_conversations WHERE id = $1`, s.conversationID).Scan(&remaining); err != nil {
		return err
	}
	s.keys <- key
	s.committed <- remaining == 0
	return nil
}

func seedEmptyConversationAttachmentFixture(t *testing.T, db *sql.DB) (conversation, fileID, key string) {
	t.Helper()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	if a.String() > b.String() {
		a, b = b, a
	}
	operation := uuid.New()
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES (true, false, $1) RETURNING id`, a).Scan(&conversation))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, a, b)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES ($1, $2, $3, true, true)`, a, b, operation)
	require.NoError(t, err)
	var messageID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content)
		VALUES ($1, $2, 'empty-conversation attachment') RETURNING id`, conversation, a).Scan(&messageID))
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key, storage_backend)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text, 'r2-useast')
		RETURNING id, storage_key`, a, conversation).Scan(&fileID, &key))
	_, err = db.Exec(`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, 0)`, messageID, fileID)
	require.NoError(t, err)
	return conversation, fileID, key
}

func assertEmptyConversationRetired(t *testing.T, db *sql.DB, conversation, fileID string, want int) {
	t.Helper()
	for _, check := range []struct {
		query string
		arg   string
	}{
		{`SELECT count(*) FROM dm_conversations WHERE id = $1`, conversation},
		{`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, conversation},
		{`SELECT count(*) FROM dm_message_attachments WHERE file_id = $1`, fileID},
		{`SELECT count(*) FROM media_files WHERE id = $1`, fileID},
	} {
		var count int
		require.NoError(t, db.QueryRow(check.query, check.arg).Scan(&count))
		require.Equal(t, want, count, check.query)
	}
}
