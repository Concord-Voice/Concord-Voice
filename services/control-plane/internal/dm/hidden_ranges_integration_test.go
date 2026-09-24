package dm

// Database-backed tests for the receiver-hide range store (#1352). mergeRanges is
// unit-tested pure in hidden_ranges_test.go; this covers the persisted side —
// insert-with-merge and the peer-message count that feeds the audit's hidden_count.
//
// Skipped when DATABASE_URL is unset (CI sets it).

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hiddenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	return db
}

func seedHiddenUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRow(`
		INSERT INTO users (id, username, email, password_hash)
		VALUES (gen_random_uuid(), 'hr_' || substr(md5(random()::text), 1, 12),
		        'hr_' || substr(md5(random()::text), 1, 12) || '@example.test', 'x')
		RETURNING id`).Scan(&id))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

// seedHiddenConv creates a 1:1 conversation between two fresh users.
func seedHiddenConv(t *testing.T, db *sql.DB) (convID, alice, bob string) {
	t.Helper()
	alice = seedHiddenUser(t, db)
	bob = seedHiddenUser(t, db)
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES (false, false, $1) RETURNING id`, alice).Scan(&convID))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, convID) })
	for _, u := range []string{alice, bob} {
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, u)
		require.NoError(t, err)
	}
	return convID, alice, bob
}

func seedHiddenMsg(t *testing.T, db *sql.DB, convID, userID string, agoSecs int) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at)
		VALUES ($1, $2, 'msg', 'text', NOW() - make_interval(secs => $3))`, convID, userID, agoSecs)
	require.NoError(t, err)
}

func TestApplyReceiverHideLocksParentsBeforeParticipant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "user parent", query: `SELECT id FROM users WHERE id = $1 FOR UPDATE`},
		{name: "conversation parent", query: `SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := hiddenTestDB(t)
			convID, actor, peer := seedHiddenConv(t, db)
			seedHiddenMsg(t, db, convID, peer, 60)
			purgeID := seedReceiverHideAudit(t, db, convID, actor)
			t.Cleanup(func() {
				if _, cleanupErr := db.Exec(`DELETE FROM message_purges WHERE id = $1`, purgeID); cleanupErr != nil {
					t.Errorf("failed to clean up receiver-hide audit row: %v", cleanupErr)
				}
			})

			probe, err := sql.Open("postgres", testdb.DatabaseURL())
			require.NoError(t, err)
			probe.SetMaxOpenConns(4)
			require.NoError(t, probe.Ping())
			t.Cleanup(func() { require.NoError(t, probe.Close()) })

			barrier, err := probe.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			t.Cleanup(func() {
				if rollbackErr := barrier.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					t.Errorf("failed to roll back lock barrier: %v", rollbackErr)
				}
			})
			var barrierTxID int64
			require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&barrierTxID))
			parentID := actor
			if tc.name == "conversation parent" {
				parentID = convID
			}
			var lockedID string
			require.NoError(t, barrier.QueryRow(tc.query, parentID).Scan(&lockedID))

			log := logger.NewWithWriter(io.Discard)
			h := NewHandler(HandlerDeps{
				DB: db, Log: log,
				PurgeEngine: purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000),
			})
			hideCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			hideDone := make(chan struct {
				hidden int
				err    error
			}, 1)
			go func() {
				hidden, hideErr := h.applyReceiverHide(hideCtx, actor, convID, nil, purgeID, 0)
				hideDone <- struct {
					hidden int
					err    error
				}{hidden: hidden, err: hideErr}
			}()

			testdb.WaitForRowLockWaiter(t, probe, barrierTxID)
			var participantID string
			require.NoError(t, barrier.QueryRow(`
				SELECT user_id FROM dm_participants
				WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE NOWAIT`, convID, actor).Scan(&participantID))
			assert.Equal(t, actor, participantID)

			require.NoError(t, barrier.Commit())
			select {
			case result := <-hideDone:
				require.NoError(t, result.err)
				assert.Equal(t, 1, result.hidden)
			case <-hideCtx.Done():
				require.FailNow(t, "receiver hide did not finish after releasing the parent lock: %v", hideCtx.Err())
			}

			var ranges, hiddenCount int
			var status string
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE conversation_id = $1 AND user_id = $2`, convID, actor).Scan(&ranges))
			require.NoError(t, db.QueryRow(`SELECT hidden_count, status FROM message_purges WHERE id = $1`, purgeID).Scan(&hiddenCount, &status))
			assert.Equal(t, 1, ranges)
			assert.Equal(t, 1, hiddenCount)
			assert.Equal(t, "completed", status, "the hide completes the audit row in its own transaction")
		})
	}
}

func seedReceiverHideAudit(t *testing.T, db *sql.DB, convID, actorID string) string {
	t.Helper()
	purgeID := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO message_purges (id, actor_id, context_type, context_id, target_user_id, status)
		VALUES ($1, $2, 'dm', $3, $2, 'in_progress')`, purgeID, actorID, convID)
	require.NoError(t, err)
	return purgeID
}

func storedRanges(t *testing.T, db *sql.DB, userID, convID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`,
		userID, convID).Scan(&n))
	return n
}

// insertRange runs InsertHiddenRange in its own transaction and commits.
func insertRange(t *testing.T, db *sql.DB, userID, convID string, from, to time.Time) int {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	hidden, err := InsertHiddenRange(context.Background(), tx, userID, convID, from, to)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return hidden
}

func TestInsertHiddenRange_CountsOnlyPeerMessagesInWindow(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, bob := seedHiddenConv(t, db)

	seedHiddenMsg(t, db, convID, bob, 60)         // peer, in window
	seedHiddenMsg(t, db, convID, bob, 120)        // peer, in window
	seedHiddenMsg(t, db, convID, bob, 30*24*3600) // peer, OUTSIDE window
	seedHiddenMsg(t, db, convID, alice, 60)       // actor's own — never counted

	from := time.Now().UTC().Add(-1 * time.Hour)
	hidden := insertRange(t, db, alice, convID, from, time.Now().UTC())

	assert.Equal(t, 2, hidden, "counts only the peer's messages inside the window")
	assert.Equal(t, 1, storedRanges(t, db, alice, convID))
}

// TestInsertHiddenRange_MergesOverlapping locks the merge-on-insert: two overlapping
// hides collapse to a single stored row rather than accumulating unboundedly.
func TestInsertHiddenRange_MergesOverlapping(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, _ := seedHiddenConv(t, db)
	now := time.Now().UTC()

	insertRange(t, db, alice, convID, now.Add(-4*time.Hour), now.Add(-2*time.Hour))
	insertRange(t, db, alice, convID, now.Add(-3*time.Hour), now) // overlaps the first

	assert.Equal(t, 1, storedRanges(t, db, alice, convID), "overlapping ranges merge into one row")

	var from, to time.Time
	require.NoError(t, db.QueryRow(`
		SELECT hidden_from, hidden_to FROM dm_message_hidden_ranges
		WHERE user_id = $1 AND conversation_id = $2`, alice, convID).Scan(&from, &to))
	assert.WithinDuration(t, now.Add(-4*time.Hour), from, time.Second, "merged range spans the earliest start")
	assert.WithinDuration(t, now, to, time.Second, "…through the latest end")
}

func TestInsertHiddenRange_KeepsDisjointRanges(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, _ := seedHiddenConv(t, db)
	now := time.Now().UTC()

	insertRange(t, db, alice, convID, now.Add(-10*time.Hour), now.Add(-9*time.Hour))
	insertRange(t, db, alice, convID, now.Add(-2*time.Hour), now)

	assert.Equal(t, 2, storedRanges(t, db, alice, convID), "non-overlapping hides stay separate")
}

// TestInsertHiddenRange_IsPerUser: one participant's hide must not affect the other's
// view — the whole point of the receiver-hide.
func TestInsertHiddenRange_IsPerUser(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, bob := seedHiddenConv(t, db)
	now := time.Now().UTC()

	insertRange(t, db, alice, convID, now.Add(-1*time.Hour), now)

	assert.Equal(t, 1, storedRanges(t, db, alice, convID))
	assert.Equal(t, 0, storedRanges(t, db, bob, convID), "peer has no hidden ranges")
}

// TestInsertHiddenRange_ErrorPropagates: a rolled-back/closed tx surfaces the error
// rather than silently reporting zero hidden.
func TestInsertHiddenRange_ErrorPropagates(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, _ := seedHiddenConv(t, db)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback()) // tx is now unusable

	_, err = InsertHiddenRange(context.Background(), tx, alice, convID,
		time.Now().UTC().Add(-time.Hour), time.Now().UTC())
	assert.Error(t, err)
}

func TestInsertClearRange_PreservesInfinityAndProvenance(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, _ := seedHiddenConv(t, db)
	cutoff := time.Now().UTC()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, InsertClearRange(context.Background(), tx, alice, convID, cutoff))
	require.NoError(t, tx.Commit())

	var from string
	var includesOwn bool
	require.NoError(t, db.QueryRow(`
		SELECT hidden_from::text, includes_own
		FROM dm_message_hidden_ranges
		WHERE user_id = $1 AND conversation_id = $2`, alice, convID).Scan(&from, &includesOwn))
	assert.Equal(t, "-infinity", from)
	assert.True(t, includesOwn, "Clear ranges hide the actor's own messages")
}

func TestHiddenRanges_DoNotMergeAcrossProvenance(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, _ := seedHiddenConv(t, db)
	now := time.Now().UTC()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, InsertClearRange(context.Background(), tx, alice, convID, now.Add(-time.Hour)))
	require.NoError(t, tx.Commit())

	insertRange(t, db, alice, convID, now.Add(-2*time.Hour), now)

	var count int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM dm_message_hidden_ranges
		WHERE user_id = $1 AND conversation_id = $2`, alice, convID).Scan(&count))
	assert.Equal(t, 2, count, "legacy receiver-hide and Clear provenance remain distinct")
}

func TestHiddenRanges_FilterOwnAndPeerMessagesByProvenance(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, bob := seedHiddenConv(t, db)
	now := time.Now().UTC()
	for _, message := range []struct {
		user string
		at   time.Time
		body string
	}{
		{alice, now.Add(-2 * time.Hour), "own-old"},
		{bob, now.Add(-2 * time.Hour), "peer-old"},
		{alice, now.Add(-time.Minute), "own-new"},
		{bob, now.Add(-time.Minute), "peer-new"},
	} {
		_, err := db.Exec(`
			INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at)
			VALUES ($1, $2, $3, 'text', $4)`, convID, message.user, message.body, message.at)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, InsertClearRange(context.Background(), tx, alice, convID, now.Add(-time.Hour)))
	require.NoError(t, tx.Commit())

	//nolint:gosec // G202: fixed aliases and parameter positions are composed with the shared filter helper; values remain parameterized.
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf -- fixed alias and placeholder are supplied by the trusted shared visibility helper; convID and viewer remain bound parameters.
	query := `SELECT m.content FROM dm_messages m WHERE m.conversation_id = $1` + purge.HiddenRangeFilter("m", 2) + ` ORDER BY m.created_at`
	rows, err := db.Query(query, convID, alice)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var visible []string
	for rows.Next() {
		var content string
		require.NoError(t, rows.Scan(&content))
		visible = append(visible, content)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, []string{"own-new", "peer-new"}, visible)
}
