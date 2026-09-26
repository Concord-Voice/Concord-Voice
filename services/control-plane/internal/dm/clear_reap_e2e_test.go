package dm

// End-to-end coverage for the DM clear-reap terminal (#3462 spec §12.3): the
// full expiry -> clear reap -> retirement pre-bind order (§7.1/D12), composed
// here by calling each RunPreflight directly in that order (cmd/server's
// runDMCleanupPreflight lives in package main and cannot be imported), plus the
// privacy invariants (I6). No build tag, matching this package's other
// database-backed suites (retirement_sweeper_test.go,
// clear_reap_sweeper_integration_test.go).

import (
	"context"
	"database/sql"
	"io"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// newClearReapExpirySweeperForTest builds the real expiration.Sweeper the way
// cmd/server wires it, so the e2e test drives the actual expiry->reap->retire
// order rather than a stand-in for the first step.
func newClearReapExpirySweeperForTest(t *testing.T, db *sql.DB, engine *purge.Engine, log *logger.Logger) *expiration.Sweeper {
	t.Helper()
	s, err := expiration.NewSweeper(expiration.SweeperDeps{
		DB:               db,
		RunExpiryBatch:   engine.RunExpiryBatch,
		EmitDMPurged:     func(context.Context, string, time.Time) {},
		EmitServerPurged: func(context.Context, string, time.Time) {},
		Log:              log,
	})
	require.NoError(t, err)
	return s
}

// A zero-participant group conversation, still carrying a message, is reaped
// (W is unbounded: I3's "no current participant" case) and then retired by the
// SAME ordered preflight pass -- no second pass required (spec §7.1).
func TestClearReapE2EZeroParticipantGroupIsReapedThenRetiredInOnePreflightPass(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	engine := purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000)

	conversationID := seedHiddenEmptyConversation(t, db, false, true, 2) // group
	insertRetirementMessage(t, db, conversationID)
	_, err := db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1`, conversationID)
	require.NoError(t, err)

	expiry := newClearReapExpirySweeperForTest(t, db, engine, log)
	clearReap := NewClearReapSweeper(db, engine, log)
	retirement := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), log)

	require.NoError(t, expiry.RunPreflight(context.Background()))
	require.NoError(t, clearReap.RunPreflight(context.Background()))
	require.NoError(t, retirement.RunPreflight(context.Background()))

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID),
		"the zero-participant conversation must be retired in this pass")
}

// A 1:1 conversation both parties have hidden (§2820) AND cleared (#3462)
// retires in the same ordered pass: the reap empties dm_messages, and the
// existing retirement predicate (no visible participant, zero messages)
// matches it immediately.
func TestClearReapE2EHiddenAndClearedOneToOneRetiresInOnePreflightPass(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	engine := purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000)

	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2) // 1:1, both hidden already
	insertRetirementMessage(t, db, conversationID)
	clearAllParticipants(t, db, conversationID, time.Now().Add(time.Hour))

	expiry := newClearReapExpirySweeperForTest(t, db, engine, log)
	clearReap := NewClearReapSweeper(db, engine, log)
	retirement := newRetirementSweeperForTest(t, db, newRetirementRail(t, db), log)

	require.NoError(t, expiry.RunPreflight(context.Background()))
	require.NoError(t, clearReap.RunPreflight(context.Background()))
	require.NoError(t, retirement.RunPreflight(context.Background()))

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID),
		"a 1:1 conversation both parties hid and cleared must retire in this pass")
}

// A completed call-event stamped in the past can land AFTER a full reap (real
// callers: internal/dm/call_events.go). Discovery re-finds the conversation
// because it never assumes the prefix below W stays empty, and the next pass
// deletes the row.
func TestClearReapE2EPastStampedCallEventIsRediscoveredAndReaped(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	engine := purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000)

	conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
	cutoff := time.Now().Add(time.Hour)
	clearAllParticipants(t, db, conversationID, cutoff)
	insertRetirementMessage(t, db, conversationID) // created_at defaults to NOW(), below cutoff

	s := NewClearReapSweeper(db, engine, log)
	require.NoError(t, s.RunPreflight(context.Background()))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, conversationID))

	afterFirstReap, err := s.candidateIDsAfter(context.Background(), "")
	require.NoError(t, err)
	assert.NotContains(t, afterFirstReap, conversationID, "an already-reaped conversation drops out of discovery")

	_, err = db.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at)
		SELECT id, created_by, 'call', 'call_event', $2 FROM dm_conversations WHERE id = $1`,
		conversationID, time.Now().Add(-time.Minute))
	require.NoError(t, err)

	rediscovered, err := s.candidateIDsAfter(context.Background(), "")
	require.NoError(t, err)
	assert.Contains(t, rediscovered, conversationID, "discovery must re-find a call-event stamped below W")

	result, err := s.RunPass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Reaped)
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, conversationID))
}

// Privacy (I6). A pass that includes a reaper queue-overflow must log no
// conversation/user/message ID -- only an aggregate drop count -- and there is
// no WebSocket path for the reap to emit on at all.
//
// A literal "capacity-1 reaper" cannot be built from this package:
// purge.Reaper's fields (log, jobs) are unexported even to purge's own test
// files that need a small queue (see internal/purge/reaper_test.go's
// TestReaper_EnqueueDropLogIsOneAggregateLineWithoutKeys, which constructs one
// via a package-local struct literal). That test already pins the "one
// aggregate line, no key" behavior for a minimal queue. This test instead
// drives a REAL overflow of the production 4096-slot queue (internal/purge/
// reaper.go's unexported blobQueueSize) through the same exported
// Engine.EnqueueBlobDeletes path a large preflight reap uses, over a log
// shared with a genuine discovery pass.
func TestClearReapE2EPrivacyNoUUIDInLogsAndNoWebSocketPath(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	logs := &retirementLogBuffer{}
	log := logger.NewWithWriter(logs)
	engine := purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000)

	for i := 0; i < 2; i++ {
		conversationID := seedHiddenEmptyConversation(t, db, false, false, 2)
		clearAllParticipants(t, db, conversationID, time.Now().Add(time.Hour))
		insertRetirementMessage(t, db, conversationID)
	}
	s := NewClearReapSweeper(db, engine, log)
	require.NoError(t, s.RunPreflight(context.Background()))

	const blobQueueSize = 4096 // internal/purge/reaper.go's unexported constant
	const overflowBy = 8
	overflow := make([]media.BlobRef, blobQueueSize+overflowBy)
	for i := range overflow {
		overflow[i] = media.NewBlobRef("attachments/"+uuid.NewString(), sql.NullString{})
	}
	engine.EnqueueBlobDeletes(overflow)

	output := logs.String()
	assert.NotRegexp(t, regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), output,
		"a pass including a queue overflow must log no UUID-shaped string (I6); got:\n%s", output)
	assert.Contains(t, output, "dropped=8", "the overflow's aggregate drop count must still be visible")
	assert.NotContains(t, output, "attachments/", "a dropped ref's key must never reach the log (I6)")

	// No WebSocket spy is wired because none is possible: ClearReapSweeper
	// (dm/clear_reap_sweeper.go) and the engine terminal it drives
	// (purge/clear_reap.go) take no websocket.Hub, Deliverer or ActivePlanRail
	// anywhere in their construction or fields -- verified at source (neither
	// file imports "websocket", "Hub", or any presence/activepresence type).
	// NewClearReapSweeper's only dependencies are *sql.DB, the narrow
	// ClearReapBatchRunner interface, and a logger, so there is structurally no path
	// by which a clear-reap pass could reach a hub to emit on, let alone spy on.
}
