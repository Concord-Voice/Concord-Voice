//go:build integration

package purge

// Database-backed tests for the DM clear-reap terminal (#3462). They drive
// RunClearReapBatch directly against real Postgres, because the properties that
// matter — W derived in-lock from Clear ranges only, evidence iff deletion,
// enqueue only after commit — live in the SQL and the transaction, not in Go.
// Race and oracle coverage lives with the discovery sweeper's suites.
//
// Skipped when DATABASE_URL is unset (CI sets it).

import (
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// clearReapFixture is one seeded DM conversation. Message and cutoff times are
// offsets from base, an hour in the past, at microsecond precision so a value
// round-trips through timestamptz unchanged.
type clearReapFixture struct {
	db             *sql.DB
	conversationID string
	members        []string
	base           time.Time
}

func seedClearReapConversation(t *testing.T, group, personal bool, members int) clearReapFixture {
	t.Helper()
	db := sweepTestDB(t) // skips without DATABASE_URL
	users := make([]string, 0, members)
	for range members {
		users = append(users, seedUploader(t, db))
	}

	var conversationID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES ($1, $2, $3) RETURNING id`, group, personal, users[0]).Scan(&conversationID))
	// Registered after the users, so it runs before their cleanup (LIFO).
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM message_purges WHERE context_id = $1`, conversationID); err != nil {
			t.Errorf("cleanup clear reap audit rows: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, conversationID); err != nil {
			t.Errorf("cleanup clear reap conversation: %v", err)
		}
	})
	for _, userID := range users {
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, userID)
		require.NoError(t, err)
	}
	return clearReapFixture{
		db:             db,
		conversationID: conversationID,
		members:        users,
		base:           time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond),
	}
}

// at returns the instant n seconds after base.
func (f clearReapFixture) at(n int) time.Time { return f.base.Add(time.Duration(n) * time.Second) }

// between returns the instant half a second after at(n), so a cutoff never
// coincides with a message's created_at.
func (f clearReapFixture) between(n int) time.Time { return f.at(n).Add(500 * time.Millisecond) }

func (f clearReapFixture) seedMessage(t *testing.T, author string, createdAt time.Time) string {
	t.Helper()
	var id string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, created_at)
		VALUES ($1, $2, 'clear-reap', $3) RETURNING id`, f.conversationID, author, createdAt).Scan(&id))
	return id
}

// seedAttachedMessage inserts a DM message carrying one Tier-2 attachment.
func (f clearReapFixture) seedAttachedMessage(t *testing.T, author string, createdAt time.Time, content string) (messageID, fileID, storageKey string) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, created_at)
		VALUES ($1, $2, $3, $4) RETURNING id`, f.conversationID, author, content, createdAt).Scan(&messageID))
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version,
		                         conversation_id, mime_type, file_size, storage_key)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1,
		        'attachments/' || gen_random_uuid()::text)
		RETURNING id, storage_key`, author, f.conversationID).Scan(&fileID, &storageKey))
	_, err := f.db.Exec(`
		INSERT INTO dm_message_attachments (message_id, file_id, position)
		VALUES ($1, $2, 0)`, messageID, fileID)
	require.NoError(t, err)
	return messageID, fileID, storageKey
}

// clear records a Clear range: includes_own, lower bound -infinity (I3).
func (f clearReapFixture) clear(t *testing.T, userID string, cutoff time.Time) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', $3, true)`, userID, f.conversationID, cutoff)
	require.NoError(t, err)
}

// legacyHide records a pre-#2820 receiver-hide range (includes_own = false),
// which must never contribute to W.
func (f clearReapFixture) legacyHide(t *testing.T, userID string, cutoff time.Time) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', $3, false)`, userID, f.conversationID, cutoff)
	require.NoError(t, err)
}

// boundedOwnRange records an includes_own range with a FINITE lower bound. It
// is not a Clear (a Clear always starts at -infinity), so it must never
// contribute to W either.
func (f clearReapFixture) boundedOwnRange(t *testing.T, userID string, from, to time.Time) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, $3, $4, true)`, userID, f.conversationID, from, to)
	require.NoError(t, err)
}

func (f clearReapFixture) messageExists(t *testing.T, id string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM dm_messages WHERE id = $1)`, id).Scan(&exists))
	return exists
}

func (f clearReapFixture) countMessages(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, f.conversationID).Scan(&n))
	return n
}

func (f clearReapFixture) newEngine(maxBatch int) *Engine {
	log := logger.NewWithWriter(io.Discard)
	return NewEngine(f.db, log, NewReaper(f.db, log, nil), maxBatch)
}

func (f clearReapFixture) reap(t *testing.T, e *Engine) ClearReapResult {
	t.Helper()
	res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})
	require.NoError(t, err)
	return res
}

type clearReapAuditRow struct {
	id, status, reason, contextType string
	actorID, targetUserID, serverID sql.NullString
	rangeFrom, rangeTo              sql.NullTime
	deleted                         int
	completed                       bool
}

func (f clearReapFixture) audits(t *testing.T) []clearReapAuditRow {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT id, status, reason, context_type, actor_id, target_user_id, server_id,
		       range_from, range_to, deleted_count, completed_at IS NOT NULL
		  FROM message_purges WHERE context_id = $1 ORDER BY created_at, id`, f.conversationID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []clearReapAuditRow
	for rows.Next() {
		var a clearReapAuditRow
		require.NoError(t, rows.Scan(&a.id, &a.status, &a.reason, &a.contextType, &a.actorID, &a.targetUserID,
			&a.serverID, &a.rangeFrom, &a.rangeTo, &a.deleted, &a.completed))
		out = append(out, a)
	}
	require.NoError(t, rows.Err())
	return out
}

// requireClearAuditShape pins the §4 row: system-owned, no actor, target,
// server or lower bound, completed in the batch transaction.
func requireClearAuditShape(t *testing.T, a clearReapAuditRow, contextType ContextType, deleted int) {
	t.Helper()
	assert.Equal(t, "completed", a.status)
	assert.True(t, a.completed, "completed_at must be stamped")
	assert.Equal(t, ClearReason, a.reason)
	assert.Equal(t, string(contextType), a.contextType)
	assert.False(t, a.actorID.Valid, "actor_id must be NULL")
	assert.False(t, a.targetUserID.Valid, "target_user_id must be NULL")
	assert.False(t, a.serverID.Valid, "server_id must be NULL")
	assert.False(t, a.rangeFrom.Valid, "range_from must be NULL")
	assert.Equal(t, deleted, a.deleted)
}

func TestRunClearReapBatchDeletesBelowTheLowerCutoffAndAuditsIt(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	a, b := f.members[0], f.members[1]
	ids := make([]string, 0, 5)
	for i := 1; i <= 5; i++ {
		ids = append(ids, f.seedMessage(t, f.members[i%2], f.at(i)))
	}
	f.clear(t, a, f.between(1)) // an older Clear: MAX per participant takes the latest
	f.clear(t, a, f.between(4))
	f.clear(t, b, f.between(2)) // the lower cutoff: W
	// Two non-Clear ranges far above W (I3). Were either counted, b's cutoff
	// would rise past a's and W would become between(4), deleting a third row.
	f.legacyHide(t, b, f.at(9))
	f.boundedOwnRange(t, b, f.at(0), f.at(9))

	res := f.reap(t, f.newEngine(5000))

	assert.Equal(t, ClearReapReaped, res.Outcome)
	assert.Equal(t, 2, res.DeletedCount)
	assert.False(t, res.More)
	require.NotEmpty(t, res.PurgeID)
	for i, id := range ids {
		assert.Equal(t, i >= 2, f.messageExists(t, id), "message at offset %d", i+1)
	}
	audits := f.audits(t)
	require.Len(t, audits, 1)
	assert.Equal(t, res.PurgeID, audits[0].id)
	requireClearAuditShape(t, audits[0], ContextDM, 2)
	require.True(t, audits[0].rangeTo.Valid, "a finite W is recorded as range_to")
	assert.True(t, audits[0].rangeTo.Time.Equal(f.between(2)), "range_to %s must equal W %s", audits[0].rangeTo.Time, f.between(2))

	// A second pass finds nothing below W and writes no further evidence.
	again := f.reap(t, f.newEngine(5000))
	assert.Equal(t, ClearReapResult{Outcome: ClearReapReaped}, again)
	assert.Len(t, f.audits(t), 1)
}

func TestRunClearReapBatchSkipsWhenAParticipantHasNotCleared(t *testing.T) {
	f := seedClearReapConversation(t, true, false, 3)
	for i := 1; i <= 3; i++ {
		f.seedMessage(t, f.members[0], f.at(i))
	}
	f.clear(t, f.members[0], f.at(9))
	f.clear(t, f.members[1], f.at(9))
	// The third member's only range is a legacy hide: it is not a Clear (I3, I4).
	f.legacyHide(t, f.members[2], f.at(9))

	res := f.reap(t, f.newEngine(5000))

	assert.Equal(t, ClearReapResult{Outcome: ClearReapNotEligible}, res)
	assert.Equal(t, 3, f.countMessages(t))
	assert.Empty(t, f.audits(t))
}

func TestRunClearReapBatchNeverReapsPersonalConversations(t *testing.T) {
	f := seedClearReapConversation(t, false, true, 1)
	for i := 1; i <= 2; i++ {
		f.seedMessage(t, f.members[0], f.at(i))
	}
	f.clear(t, f.members[0], f.at(9))

	res := f.reap(t, f.newEngine(5000))

	assert.Equal(t, ClearReapResult{Outcome: ClearReapNotEligible}, res)
	assert.Equal(t, 2, f.countMessages(t))
	assert.Empty(t, f.audits(t))
}

func TestRunClearReapBatchReapsEverythingWhenNoParticipantRemains(t *testing.T) {
	f := seedClearReapConversation(t, true, false, 2)
	f.seedMessage(t, f.members[0], f.at(1))
	f.seedMessage(t, f.members[1], f.at(2))
	// Stamped in the future: W = +infinity bounds nothing.
	f.seedMessage(t, f.members[0], time.Now().Add(time.Hour))
	_, err := f.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1`, f.conversationID)
	require.NoError(t, err)

	res := f.reap(t, f.newEngine(5000))

	assert.Equal(t, ClearReapReaped, res.Outcome)
	assert.Equal(t, 3, res.DeletedCount)
	assert.Zero(t, f.countMessages(t))
	audits := f.audits(t)
	require.Len(t, audits, 1)
	requireClearAuditShape(t, audits[0], ContextGroup, 3)
	assert.False(t, audits[0].rangeTo.Valid, "W = +infinity is recorded as a NULL range_to (D9)")
}

// I10: a Clear range hides pinned messages too, so a pin below W is reaped like
// any other row. A keep-pins predicate on the victim select turns this red.
func TestRunClearReapBatchReapsPinnedMessagesBelowWatermark(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	a, b := f.members[0], f.members[1]
	pinnedBelow := f.seedMessage(t, a, f.at(1))
	f.seedMessage(t, b, f.at(2))
	pinnedAbove := f.seedMessage(t, a, f.at(4))
	_, err := f.db.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = ANY(ARRAY[$1::uuid, $3::uuid])`,
		pinnedBelow, a, pinnedAbove)
	require.NoError(t, err)
	f.clear(t, a, f.between(3))
	f.clear(t, b, f.between(3))

	res := f.reap(t, f.newEngine(5000))

	assert.Equal(t, 2, res.DeletedCount)
	assert.False(t, f.messageExists(t, pinnedBelow), "a pinned message below W must be reaped")
	assert.True(t, f.messageExists(t, pinnedAbove), "a message above W must survive, pinned or not")
}

func TestRunClearReapBatchStridesAndReportsMore(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	for i := 1; i <= 5; i++ {
		f.seedMessage(t, f.members[i%2], f.at(i))
	}
	f.clear(t, f.members[0], f.between(5))
	f.clear(t, f.members[1], f.between(5))
	e := f.newEngine(2)

	got := make([]ClearReapResult, 0, 3)
	for range 3 {
		got = append(got, f.reap(t, e))
	}

	for i, want := range []struct {
		deleted int
		more    bool
	}{{2, true}, {2, true}, {1, false}} {
		assert.Equal(t, ClearReapReaped, got[i].Outcome, "batch %d", i)
		assert.Equal(t, want.deleted, got[i].DeletedCount, "batch %d", i)
		assert.Equal(t, want.more, got[i].More, "batch %d", i)
	}
	assert.Zero(t, f.countMessages(t))
	audits := f.audits(t)
	require.Len(t, audits, 3, "one audit row per non-empty batch (R4)")
	for i, deleted := range []int{2, 2, 1} {
		requireClearAuditShape(t, audits[i], ContextDM, deleted)
	}
}

// D1: a batch with nothing below W writes no evidence at all.
func TestRunClearReapBatchWritesNoAuditWhenNothingIsBelowTheWatermark(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	for i := 1; i <= 3; i++ {
		f.seedMessage(t, f.members[0], f.at(i))
	}
	f.clear(t, f.members[0], f.at(0))
	f.clear(t, f.members[1], f.at(9))

	res := f.reap(t, f.newEngine(5000))

	assert.Equal(t, ClearReapResult{Outcome: ClearReapReaped}, res)
	assert.Equal(t, 3, f.countMessages(t))
	assert.Empty(t, f.audits(t))
}

func TestRunClearReapBatchRetiresAndQueuesAttachment(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	messageID, fileID, key := f.seedAttachedMessage(t, f.members[0], f.at(1), "clear-reap-attachment")
	f.clear(t, f.members[0], f.between(1))
	f.clear(t, f.members[1], f.between(1))
	e := f.newEngine(5000)

	res := f.reap(t, e)

	assert.Equal(t, 1, res.DeletedCount)
	assert.False(t, f.messageExists(t, messageID))
	var deletedAt sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.True(t, deletedAt.Valid, "the unshared Tier-2 file is soft-deleted")
	require.Len(t, e.reaper.jobs, 1, "exactly one blob ref is enqueued after commit")
	assert.Equal(t, key, (<-e.reaper.jobs).Key)
}

// I8 + I9: a commit that fails leaves every row, every link and every file in
// place, enqueues nothing, and leaves no evidence.
func TestRunClearReapBatchCommitFailureRollsBackAndDoesNotEnqueue(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	const content = "clear-reap-commit-failure"
	messageID, fileID, _ := f.seedAttachedMessage(t, f.members[0], f.at(1), content)
	f.clear(t, f.members[0], f.between(1))
	f.clear(t, f.members[1], f.between(1))
	_, err := f.db.Exec(`
		CREATE FUNCTION test_reject_clear_reap_commit() RETURNS trigger AS $$
		BEGIN
			IF OLD.content = 'clear-reap-commit-failure' THEN RAISE EXCEPTION 'forced clear reap commit failure'; END IF;
			RETURN OLD;
		END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER test_reject_clear_reap_commit
		AFTER DELETE ON dm_messages
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION test_reject_clear_reap_commit()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, cleanupErr := f.db.Exec(`DROP TRIGGER IF EXISTS test_reject_clear_reap_commit ON dm_messages; DROP FUNCTION IF EXISTS test_reject_clear_reap_commit()`); cleanupErr != nil {
			t.Errorf("cleanup clear reap commit failure trigger: %v", cleanupErr)
		}
	})
	e := f.newEngine(5000)

	res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})

	require.ErrorContains(t, err, "purge: commit clear reap batch")
	assert.NotContains(t, err.Error(), f.conversationID, "errors carry no IDs (I6)")
	assert.Equal(t, ClearReapResult{}, res)
	assert.True(t, f.messageExists(t, messageID))
	var bridges int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM dm_message_attachments WHERE message_id = $1`, messageID).Scan(&bridges))
	assert.Equal(t, 1, bridges)
	var deletedAt sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
	assert.Empty(t, e.reaper.jobs, "nothing may be enqueued before a confirmed commit (I8)")
	assert.Empty(t, f.audits(t), "the audit row shares the rolled-back transaction (I9)")
}

// I9 in the other direction: when the evidence cannot be written, the deletion
// it would describe does not happen either.
func TestRunClearReapBatchAuditFailureRollsBackTheDeletion(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	messageID, fileID, _ := f.seedAttachedMessage(t, f.members[0], f.at(1), "clear-reap-audit-failure")
	f.clear(t, f.members[0], f.between(1))
	f.clear(t, f.members[1], f.between(1))
	_, err := f.db.Exec(`
		CREATE FUNCTION test_reject_clear_reap_audit() RETURNS trigger AS $$
		BEGIN
			IF NEW.reason = 'clear' THEN RAISE EXCEPTION 'forced clear reap audit failure'; END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_clear_reap_audit
		BEFORE INSERT ON message_purges
		FOR EACH ROW EXECUTE FUNCTION test_reject_clear_reap_audit()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, cleanupErr := f.db.Exec(`DROP TRIGGER IF EXISTS test_reject_clear_reap_audit ON message_purges; DROP FUNCTION IF EXISTS test_reject_clear_reap_audit()`); cleanupErr != nil {
			t.Errorf("cleanup clear reap audit failure trigger: %v", cleanupErr)
		}
	})
	e := f.newEngine(5000)

	_, err = e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})

	require.ErrorContains(t, err, "purge: write clear reap audit")
	assert.True(t, f.messageExists(t, messageID))
	var deletedAt sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
	assert.Empty(t, e.reaper.jobs)
}

func TestRunClearReapBatchReportsGoneForAMissingConversation(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	_, err := f.db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, f.conversationID)
	require.NoError(t, err)
	e := f.newEngine(5000)

	for _, id := range []string{f.conversationID, uuid.NewString()} {
		res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: id})
		require.NoError(t, err)
		assert.Equal(t, ClearReapResult{Outcome: ClearReapGone}, res)
	}
}

// Only direct SQL can write an 'infinity' cutoff. lib/pq cannot scan it into a
// time.Time, so the watermark read fails and the batch deletes nothing.
func TestRunClearReapBatchFailsClosedOnAnInfiniteClearCutoff(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	f.seedMessage(t, f.members[0], f.at(1))
	for _, userID := range f.members {
		_, err := f.db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
			VALUES ($1, $2, '-infinity', 'infinity', true)`, userID, f.conversationID)
		require.NoError(t, err)
	}

	res, err := f.newEngine(5000).RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})

	require.ErrorContains(t, err, "purge: compute clear watermark")
	assert.Equal(t, ClearReapResult{}, res)
	assert.Equal(t, 1, f.countMessages(t))
	assert.Empty(t, f.audits(t))
}

func TestRunClearReapBatchCancelledContextDeletesNothing(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	f.seedMessage(t, f.members[0], f.at(1))
	f.clear(t, f.members[0], f.at(9))
	f.clear(t, f.members[1], f.at(9))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.newEngine(5000).RunClearReapBatch(ctx, ClearReapPlan{ConversationID: f.conversationID})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, f.countMessages(t))
	assert.Empty(t, f.audits(t))
}
