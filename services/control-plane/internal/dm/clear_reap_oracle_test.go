package dm

// The HiddenRangeFilter oracle (#3462 spec §12.2). A property test over a fixed
// seed drives RunClearReapBatch against about 60 random conversation states and
// checks it against an INDEPENDENT Go computation of the watermark W, never
// against DecideClearWatermark or ClearWatermarkLateral (that would be
// circular). For every current participant it reads the visible set through the
// production predicate, purge.HiddenRangeFilter, exactly as a real DM fetch
// would. No build tag, matching this package's other database-backed suites
// (retirement_sweeper_test.go, clear_reap_sweeper_integration_test.go).

import (
	"context"
	"database/sql"
	"io"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// clearReapOracleStates is deliberately modest: each state seeds up to ~30
// messages and drains a real batch transaction, so the count trades breadth for
// keeping the suite fast. Raise it if CI budget allows; the seed keeps any given
// count's failures reproducible.
const clearReapOracleStates = 200

type oracleMessage struct {
	id        string
	createdAt time.Time
}

func TestClearReapOracleDeletesExactlyWhatEveryParticipantHasCleared(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	engine := purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 500)
	//nolint:gosec // G404: a fixed seed makes the oracle reproducible; this is not security randomness.
	rng := rand.New(rand.NewSource(3462))

	var conversationIDs []string
	t.Cleanup(func() {
		for _, id := range conversationIDs {
			if _, err := db.Exec(`DELETE FROM message_purges WHERE context_id = $1`, id); err != nil {
				t.Errorf("oracle cleanup audit rows for %s: %v", id, err)
			}
			if _, err := db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, id); err != nil {
				t.Errorf("oracle cleanup conversation %s: %v", id, err)
			}
		}
	})

	start := time.Now()
	for i := 0; i < clearReapOracleStates; i++ {
		conversationID := runClearReapOracleState(t, db, engine, rng, i)
		conversationIDs = append(conversationIDs, conversationID)
	}
	t.Logf("clear reap oracle: %d states in %s", clearReapOracleStates, time.Since(start))
}

// runClearReapOracleState seeds one random conversation, drains the reap, and
// asserts the invariants. It returns the conversation id for cleanup.
func runClearReapOracleState(t *testing.T, db *sql.DB, engine *purge.Engine, rng *rand.Rand, seq int) string {
	t.Helper()
	base := time.Now().UTC().Add(-time.Duration(48+seq) * time.Hour).Truncate(time.Microsecond)
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Second) }

	isPersonal := rng.Intn(10) == 0 // ~10% (spec §12.2)
	numParticipants := rng.Intn(5)  // 0-4
	isGroup := numParticipants > 2 || (numParticipants > 0 && rng.Intn(2) == 0)

	creator := dbtest.CreateUser(t, db).String()
	var conversationID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES ($1, $2, $3) RETURNING id`, isGroup, isPersonal, creator).Scan(&conversationID))

	participants := make([]string, 0, numParticipants)
	if numParticipants > 0 {
		participants = append(participants, creator)
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, creator)
		require.NoError(t, err)
		for p := 1; p < numParticipants; p++ {
			user := dbtest.CreateUser(t, db).String()
			_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, user)
			require.NoError(t, err)
			participants = append(participants, user)
		}
	}

	// Per-participant Clear ranges: about 70% of current participants clear
	// (some with two ranges, so MAX-per-participant is exercised); the rest are
	// rejoiners/newcomers with no Clear range, which must block the reap (I4).
	clearCutoff := make(map[string]time.Time)
	for _, p := range participants {
		if rng.Intn(10) < 7 {
			cutoff := at(rng.Intn(40)).Add(time.Duration(rng.Intn(1000)) * time.Millisecond)
			if rng.Intn(3) == 0 {
				earlier := cutoff.Add(-time.Duration(1+rng.Intn(20)) * time.Second)
				_, err := db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
					VALUES ($1, $2, '-infinity', $3, true)`, p, conversationID, earlier)
				require.NoError(t, err)
			}
			_, err := db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
				VALUES ($1, $2, '-infinity', $3, true)`, p, conversationID, cutoff)
			require.NoError(t, err)
			clearCutoff[p] = cutoff
		}
		// A legacy receiver-hide and a Hide, both of which must never move W (I3).
		if rng.Intn(2) == 0 {
			_, err := db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
				VALUES ($1, $2, '-infinity', $3, false)`, p, conversationID, at(60+rng.Intn(20)))
			require.NoError(t, err)
		}
		if rng.Intn(2) == 0 {
			_, err := db.Exec(`UPDATE dm_participants SET hidden_at = $3 WHERE conversation_id = $1 AND user_id = $2`,
				conversationID, p, at(rng.Intn(40)))
			require.NoError(t, err)
		}
	}

	// A departed member with a leftover Clear range: the hidden_range row
	// survives; the dm_participants row does not (I3: only CURRENT participants
	// set W).
	if rng.Intn(2) == 0 {
		departed := dbtest.CreateUser(t, db)
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, departed)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
			VALUES ($1, $2, '-infinity', $3, true)`, departed, conversationID, at(1))
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, departed)
		require.NoError(t, err)
	}

	// 10-30 messages at spread timestamps; some pinned (I10), some call events
	// stamped in the past.
	numMessages := 10 + rng.Intn(21)
	seeded := make([]oracleMessage, 0, numMessages)
	for m := 0; m < numMessages; m++ {
		author := creator
		if len(participants) > 0 {
			author = participants[rng.Intn(len(participants))]
		}
		createdAt := at(rng.Intn(80))
		msgType := "user"
		if rng.Intn(7) == 0 {
			msgType = "call_event"
		}
		var id string
		require.NoError(t, db.QueryRow(`
			INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at)
			VALUES ($1, $2, 'oracle', $3, $4) RETURNING id`, conversationID, author, msgType, createdAt).Scan(&id))
		if rng.Intn(5) == 0 {
			_, err := db.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = $1`, id, author)
			require.NoError(t, err)
		}
		seeded = append(seeded, oracleMessage{id: id, createdAt: createdAt})
	}

	visibleBefore := make(map[string]map[string]bool, len(participants))
	for _, p := range participants {
		visibleBefore[p] = clearReapVisibleSet(t, db, conversationID, p)
	}

	// Independent expected-W computation (never via DecideClearWatermark or
	// ClearWatermarkLateral -- that would be circular).
	unbounded := len(participants) == 0
	eligible := !isPersonal && (unbounded || len(clearCutoff) == len(participants))
	var w time.Time
	if eligible && !unbounded {
		first := true
		for _, cutoff := range clearCutoff {
			if first || cutoff.Before(w) {
				w = cutoff
				first = false
			}
		}
	}
	expectedDeleted := make(map[string]bool)
	if eligible {
		for _, m := range seeded {
			if unbounded || m.createdAt.Before(w) {
				expectedDeleted[m.id] = true
			}
		}
	}

	// Drain.
	for {
		res, err := engine.RunClearReapBatch(context.Background(), purge.ClearReapPlan{ConversationID: conversationID})
		require.NoError(t, err)
		if res.Outcome != purge.ClearReapReaped || !res.More {
			break
		}
	}

	surviving := clearReapSurvivingIDs(t, db, conversationID)
	actualDeleted := make(map[string]bool)
	for _, m := range seeded {
		if !surviving[m.id] {
			actualDeleted[m.id] = true
		}
	}

	require.Equal(t, sortedKeys(expectedDeleted), sortedKeys(actualDeleted),
		"state %d: deleted set must equal {m : created_at < W} exactly (completeness+soundness)", seq)

	for _, p := range participants {
		for id := range actualDeleted {
			assert.False(t, visibleBefore[p][id], "state %d: participant %s could see deleted message %s", seq, p, id)
		}
		after := clearReapVisibleSet(t, db, conversationID, p)
		assert.Equal(t, sortedKeys(visibleBefore[p]), sortedKeys(after), "state %d: participant %s's visible set must be unchanged", seq, p)
	}

	if unbounded && !isPersonal {
		assert.Equal(t, sortedKeys(messageIDSet(seeded)), sortedKeys(actualDeleted), "state %d: zero participants means every message is deleted", seq)
	}
	if isPersonal {
		assert.Empty(t, actualDeleted, "state %d: is_personal must never be reaped (I5)", seq)
	}

	return conversationID
}

func clearReapVisibleSet(t *testing.T, db *sql.DB, conversationID, userID string) map[string]bool {
	t.Helper()
	//nolint:gosec // G202: HiddenRangeFilter is a compile-time SQL fragment; values are parameterized.
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
	query := `SELECT dm.id FROM dm_messages dm WHERE dm.conversation_id = $1` + purge.HiddenRangeFilter("dm", 2)
	rows, err := db.Query(query, conversationID, userID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out[id] = true
	}
	require.NoError(t, rows.Err())
	return out
}

func clearReapSurvivingIDs(t *testing.T, db *sql.DB, conversationID string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM dm_messages WHERE conversation_id = $1`, conversationID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out[id] = true
	}
	require.NoError(t, rows.Err())
	return out
}

func messageIDSet(msgs []oracleMessage) map[string]bool {
	out := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		out[m.id] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
