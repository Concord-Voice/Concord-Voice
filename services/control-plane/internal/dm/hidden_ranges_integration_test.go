package dm

// Database-backed tests for the receiver-hide range store (#1352). mergeRanges is
// unit-tested pure in hidden_ranges_test.go; this covers the persisted side —
// insert-with-merge and the peer-message count that feeds the audit's hidden_count.
//
// Skipped when DATABASE_URL is unset (CI sets it).

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hiddenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL required for hidden-range store coverage")
	}
	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
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
