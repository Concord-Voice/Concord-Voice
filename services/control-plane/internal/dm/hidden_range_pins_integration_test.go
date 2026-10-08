package dm

// Database-backed tests for the receiver-hide range store now that a pinned
// message is never hidden (#3458 §18). Skipped when DATABASE_URL is unset.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedDMAt inserts a message by userID at exactly at, pinned by its author when asked.
func seedDMAt(t *testing.T, db *sql.DB, convID, userID, content string, at time.Time, pinned bool) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, pinned_at, pinned_by)
		VALUES ($1, $2, $3, 'text', $4, CASE WHEN $5 THEN NOW() END, CASE WHEN $5 THEN $2::uuid END)`,
		convID, userID, content, at, pinned)
	require.NoError(t, err)
}

// A pinned peer message is not a hole in the stored range and not a hidden
// message: the window is one row, and the count covers only unpinned peers.
func TestInsertHiddenRange_OneRangeCountsUnpinnedPeers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := hiddenTestDB(t)
	convID, alice, bob := seedHiddenConv(t, db)
	seedDMAt(t, db, convID, bob, "peer-pinned", now.Add(-3*time.Second), true)
	seedDMAt(t, db, convID, bob, "peer-plain", now.Add(-2*time.Second), false)
	seedDMAt(t, db, convID, alice, "own", now.Add(-time.Second), false)

	hidden := insertRange(t, db, alice, convID, now.Add(-time.Hour), now)

	assert.Equal(t, 1, hidden, "only the unpinned peer message is counted")
	assert.Equal(t, 1, storedRanges(t, db, alice, convID), "the pin cuts no hole")
}

// The hide re-takes the conversation row before counting, so a DM pin or
// unpin (which holds it FOR NO KEY UPDATE) cannot change the count mid-hide.
func TestInsertHiddenRange_WaitsForConversationLock(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, _ := seedHiddenConv(t, db)

	side, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = side.Rollback() })
	_, err = side.Exec(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID)
	require.NoError(t, err)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`SET LOCAL lock_timeout = '200ms'`)
	require.NoError(t, err)
	_, err = InsertHiddenRange(context.Background(), tx, alice, convID,
		time.Now().UTC().Add(-time.Hour), time.Now().UTC())

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "want a lock timeout, got %v", err)
	assert.Equal(t, "55P03", string(pqErr.Code))
}

// The one-statement re-insert stores -infinity and microsecond bounds exactly.
func TestInsertHiddenRange_ReinsertRoundTripsBounds(t *testing.T) {
	db := hiddenTestDB(t)
	convID, alice, _ := seedHiddenConv(t, db)
	base := time.Date(2026, 9, 1, 8, 30, 15, 123456000, time.UTC)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, InsertClearRange(context.Background(), tx, alice, convID, base.Add(-time.Hour)))
	require.NoError(t, tx.Commit())
	insertRange(t, db, alice, convID, base, base.Add(20*time.Microsecond))

	rows, err := db.Query(`
		SELECT hidden_from::text, hidden_to, includes_own FROM dm_message_hidden_ranges
		WHERE user_id = $1 AND conversation_id = $2 ORDER BY hidden_from`, alice, convID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	type stored struct {
		from string
		to   time.Time
		own  bool
	}
	var got []stored
	for rows.Next() {
		var s stored
		require.NoError(t, rows.Scan(&s.from, &s.to, &s.own))
		s.to = s.to.UTC()
		got = append(got, s)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2)
	assert.Equal(t, stored{"-infinity", base.Add(-time.Hour), true}, got[0])
	from, err := parseHiddenRangeTime(got[1].from)
	require.NoError(t, err)
	assert.Equal(t, base, from.UTC(), "the hide window starts at its microsecond-exact lower bound")
	assert.Equal(t, base.Add(20*time.Microsecond), got[1].to)
	assert.False(t, got[1].own, "a legacy purge range hides peers only")
}
