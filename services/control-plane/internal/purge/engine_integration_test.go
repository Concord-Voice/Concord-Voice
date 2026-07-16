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
