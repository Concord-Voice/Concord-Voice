package friends

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// This test is in package friends, which cannot import internal/testhelpers:
// that package builds the whole router and therefore depends on this one. It
// uses internal/testhelpers/testdb instead, which has zero internal
// dependencies. Same reasoning as friend_code_claim_internal_test.go.
//
// Being in-package is what makes the #2854 stage C stale-probe branch testable
// DETERMINISTICALLY. Driven through HTTP the branch is unreachable on purpose:
// the probe and the transaction read the same committed state microseconds
// apart, so provoking a disagreement would need a real race. Calling
// executeBlockTx directly with the capture the probe WOULD have selected
// reproduces the disagreement exactly, with no timing at all.

func insertTestUser(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2 || '@test.local', 'u_' || left($2, 8), 'x', true, true)`,
		id, id.String(),
	)
	require.NoError(t, err)
	return id
}

// TestExecuteBlockTxFailsClosedWhenTheProbeWentStale is the C4 fail-closed arm.
//
// The probe said "no accepted edge", so BlockUser selected the ungated path and
// nil'd the capture. By the time the transaction opened, the edge existed. On
// that path CaptureInTx's own accepted-edge gate never runs, so proceeding
// would flip the friendship to blocked and revoke a LIVE audience with zero
// reconciliation — the counterpart keeps the principal's Custom Status, which
// carries no TTL and is never republished on a heartbeat (CWE-284).
func TestExecuteBlockTxFailsClosedWhenTheProbeWentStale(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	actor := insertTestUser(t, db)
	victim := insertTestUser(t, db)
	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		actor, victim)
	require.NoError(t, err)

	h := NewHandler(db, logger.New("test"), nil)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// nil is precisely what BlockUser passes when its probe reported no edge.
	plan, blockErr := h.executeBlockTx(context.Background(), nil, true, tx, actor.String(), victim.String())

	require.ErrorIs(t, blockErr, presencehook.ErrProbeStale,
		"a revoking write must never proceed ungated over a live audience")
	require.Nil(t, plan, "no plan may exist on this path — there is nothing to abandon")

	// The terminal the client sees: retryable, and honest that nothing landed.
	f := presencehook.Classify(blockErr)
	require.Equal(t, http.StatusServiceUnavailable, f.Status)
	require.Equal(t, "probe_stale", f.Code)
	value, ok := f.RetryAfterHeader()
	require.True(t, ok)
	require.Equal(t, "1", value)

	// Fail CLOSED means nothing was written, not merely that an error surfaced.
	var blocked int
	require.NoError(t, tx.QueryRow(
		`SELECT COUNT(*) FROM friendships
		 WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		   AND status = 'blocked'`, actor, victim).Scan(&blocked))
	require.Zero(t, blocked, "the block must NOT have been applied")
}

// TestExecuteBlockTxProceedsWhenTheProbeWasRight is the other half: with the
// edge genuinely absent, the ungated path writes the block as normal. It fails
// if the fail-closed guard is made unconditional, which would break every
// stranger block — the exact over-correction the C4 split exists to prevent.
func TestExecuteBlockTxProceedsWhenTheProbeWasRight(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	actor := insertTestUser(t, db)
	stranger := insertTestUser(t, db)

	h := NewHandler(db, logger.New("test"), nil)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	plan, blockErr := h.executeBlockTx(context.Background(), nil, true, tx, actor.String(), stranger.String())

	require.NoError(t, blockErr, "blocking a stranger ungated is the normal, intended path")
	require.Nil(t, plan, "an ungated capture yields no plan")

	var blocked int
	require.NoError(t, tx.QueryRow(
		`SELECT COUNT(*) FROM friendships
		 WHERE requester_id = $1 AND addressee_id = $2 AND status = 'blocked'`,
		actor, stranger).Scan(&blocked))
	require.Equal(t, 1, blocked, "the block must land")
}

// TestExecuteBlockTxOnAnUnwiredReplicaIsNotAStaleProbe is the regression for a
// defect this PR introduced and CodeRabbit caught: `nil` capture meant TWO
// different things, and the stale-probe check tested the wrong one.
//
// A nil capture is ALSO the documented UNWIRED fallback — a replica with no
// graph-presence capture at all. requireGraphPresenceCaptureWired does not rule
// that out, because it early-returns when activityService is nil. So testing
// `gated == nil` for staleness made EVERY block of a real friend return 503
// probe_stale on such a replica: a fail-closed refusal of a safety affordance
// that should simply proceed, on a deployment with no presence rail to
// reconcile in the first place.
//
// probeSkippedGate=false is the unwired case. The accepted edge below is what
// made the old code fire; it must now be irrelevant.
func TestExecuteBlockTxOnAnUnwiredReplicaIsNotAStaleProbe(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	actor := insertTestUser(t, db)
	friend := insertTestUser(t, db)
	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		actor, friend)
	require.NoError(t, err)

	h := NewHandler(db, logger.New("test"), nil) // no SetGraphPresenceCapture: UNWIRED

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// nil capture because nothing is wired — NOT because a probe skipped the gate.
	plan, blockErr := h.executeBlockTx(context.Background(), nil, false, tx, actor.String(), friend.String())

	require.NoError(t, blockErr,
		"an unwired replica must block normally; it is not a stale probe")
	require.NotErrorIs(t, blockErr, presencehook.ErrProbeStale)
	require.Nil(t, plan, "an unwired capture yields no plan")

	var blocked int
	require.NoError(t, tx.QueryRow(
		`SELECT COUNT(*) FROM friendships
		 WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		   AND status = 'blocked'`, actor, friend).Scan(&blocked))
	require.Equal(t, 1, blocked, "the block MUST land on an unwired replica")
}
