package dm

// Database-backed tests for the DM clear-reap discovery sweeper (#3462), driven
// against the real purge.Engine. Unlike clear_reap_sweeper_test.go, these prove
// the discovery predicate and the engine actually agree: a conversation the
// query selects is one the engine will in fact reap, and a drained conversation
// drops out of the next discovery pass. No build tag, matching
// retirement_sweeper_test.go's neighbouring convention in this package.

import (
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// clearParticipant writes one Clear range for userID: includes_own, lower
// bound -infinity (I3), matching internal/purge's clearReapFixture.clear.
func clearParticipant(t *testing.T, db *sql.DB, conversationID, userID string, cutoff time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', $3, true)`, userID, conversationID, cutoff)
	require.NoError(t, err)
}

// clearAllParticipants records a Clear range at cutoff for every current
// participant of conversationID.
func clearAllParticipants(t *testing.T, db *sql.DB, conversationID string, cutoff time.Time) {
	t.Helper()
	for _, userID := range participantIDs(t, db, conversationID) {
		clearParticipant(t, db, conversationID, userID, cutoff)
	}
}

func participantIDs(t *testing.T, db *sql.DB, conversationID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, conversationID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func newClearReapBatchRunnerForTest(db *sql.DB, log *logger.Logger) *purge.Engine {
	return purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000)
}

func TestClearReapSweeperDiscoversFullyClearedConversationsButNotPartlyClearedOnes(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	future := time.Now().Add(time.Hour)

	full := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertRetirementMessage(t, db, full)
	clearAllParticipants(t, db, full, future)

	partial := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertRetirementMessage(t, db, partial)
	clearParticipant(t, db, partial, participantIDs(t, db, partial)[0], future)

	s := NewClearReapSweeper(db, nil, logger.NewWithWriter(io.Discard))

	discovered, err := s.candidateIDsAfter(context.Background(), "")

	require.NoError(t, err)
	assert.Contains(t, discovered, full)
	assert.NotContains(t, discovered, partial)
}

func TestClearReapSweeperNeverDiscoversPersonalConversations(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	personal := seedHiddenEmptyConversation(t, db, true, false, 1)
	insertRetirementMessage(t, db, personal)
	clearAllParticipants(t, db, personal, time.Now().Add(time.Hour))

	s := NewClearReapSweeper(db, nil, logger.NewWithWriter(io.Discard))

	discovered, err := s.candidateIDsAfter(context.Background(), "")

	require.NoError(t, err)
	assert.NotContains(t, discovered, personal)
}

func TestClearReapSweeperDiscoversZeroParticipantGroupsOnlyWhenMessagesRemain(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	withMessages := seedHiddenEmptyConversation(t, db, false, true, 2)
	insertRetirementMessage(t, db, withMessages)
	_, err := db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1`, withMessages)
	require.NoError(t, err)

	empty := seedHiddenEmptyConversation(t, db, false, true, 2)
	_, err = db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1`, empty)
	require.NoError(t, err)

	s := NewClearReapSweeper(db, nil, logger.NewWithWriter(io.Discard))

	discovered, err := s.candidateIDsAfter(context.Background(), "")

	require.NoError(t, err)
	assert.Contains(t, discovered, withMessages)
	assert.NotContains(t, discovered, empty)
}

func TestClearReapSweeperExcludesAnAlreadyReapedConversation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertRetirementMessage(t, db, conversationID)
	clearAllParticipants(t, db, conversationID, time.Now().Add(time.Hour))
	s := NewClearReapSweeper(db, newClearReapBatchRunnerForTest(db, log), log)

	before, err := s.candidateIDsAfter(context.Background(), "")
	require.NoError(t, err)
	require.Contains(t, before, conversationID)

	result, err := s.RunPass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Reaped)

	after, err := s.candidateIDsAfter(context.Background(), "")
	require.NoError(t, err)
	assert.NotContains(t, after, conversationID)
}

// Seeds more conversations than fit in one candidateLimit page and drains them
// through runPassAfter's own cursor, proving pagination reaches the tail page
// rather than starving it. Driven directly (not through RunWorker's ticker) so
// the assertion is deterministic instead of timing-based.
func TestClearReapSweeperDrainsMoreThanOnePageWithNoCandidateStarved(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	total := clearReapCandidateLimit + 5
	ids := make([]string, 0, total)
	for range total {
		conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
		insertRetirementMessage(t, db, conversationID)
		_, err := db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1`, conversationID)
		require.NoError(t, err)
		ids = append(ids, conversationID)
	}
	s := NewClearReapSweeper(db, newClearReapBatchRunnerForTest(db, log), log)

	var cursor string
	totalReaped := 0
	for range total { // generous bound; at most two pages are ever expected
		result, next, err := s.runPassAfter(context.Background(), cursor, 0)
		require.NoError(t, err)
		totalReaped += result.Reaped
		if result.Selected == 0 {
			break
		}
		cursor = next
	}

	assert.Equal(t, total, totalReaped, "every seeded conversation must be reaped, none starved")
	for _, id := range ids {
		assert.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, id), "conversation %s", id)
	}
}

func TestClearReapSweeperPreflightDrainsToEmpty(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	insertRetirementMessage(t, db, conversationID)
	clearAllParticipants(t, db, conversationID, time.Now().Add(time.Hour))
	s := NewClearReapSweeper(db, newClearReapBatchRunnerForTest(db, log), log)

	require.NoError(t, s.RunPreflight(context.Background()))

	remaining, err := s.candidateIDsAfter(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, remaining)
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, conversationID))
}
