package members

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// package members cannot import internal/testhelpers — that package builds the
// router and therefore depends on this one. testdb has zero internal deps.

func banTestUser(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2 || '@test.local', 'u_' || left($2, 8), 'x', true, true)`,
		id, id.String())
	require.NoError(t, err)
	return id
}

func banTestServer(t *testing.T, db *sql.DB, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'unwired-ban-test', $2)`, id, owner)
	require.NoError(t, err)
	return id
}

// TestExecBanTxOnAnUnwiredReplicaIsNotAStaleProbe is the regression for a defect
// this PR introduced and CodeRabbit caught: `nil` capture meant TWO different
// things, and the stale-probe check tested the wrong one.
//
// A nil capture is ALSO the documented UNWIRED fallback — a replica with no
// graph-presence capture at all. requireGraphPresenceCaptureWired does not rule
// that out, because it early-returns when activityService is nil. So testing
// `gated == nil` for staleness made EVERY ban of a real member return 503
// probe_stale on such a replica: a fail-closed refusal of a MODERATION action,
// on a deployment with no presence rail to reconcile in the first place.
//
// The handler below has no capture wired, and the target IS a member — the
// exact combination the old code turned into ErrProbeStale.
func TestExecBanTxOnAnUnwiredReplicaIsNotAStaleProbe(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	owner := banTestUser(t, db)
	target := banTestUser(t, db)
	serverID := banTestServer(t, db, owner)

	_, err := db.Exec(
		`INSERT INTO server_members (server_id, user_id, role, joined_at)
		 VALUES ($1, $2, 'member', NOW())`, serverID, target)
	require.NoError(t, err)

	// UNWIRED: no SetGraphPresenceCapture. h.graphPresence stays nil.
	h := &Handler{db: db, log: logger.New("test")}
	require.False(t, h.HasGraphPresenceCapture(), "precondition: the capture must be unwired")

	// probedMember=false is what the probe reports for a target it could not
	// confirm; on a wired replica that selects the ungated path. Unwired, it
	// must make no difference at all.
	err = h.execBanTx(context.Background(), serverID.String(), target.String(), owner.String(), nil, false)

	require.NoError(t, err, "an unwired replica must ban normally; it is not a stale probe")
	require.NotErrorIs(t, err, presencehook.ErrProbeStale)

	var bans int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM server_bans WHERE server_id = $1 AND user_id = $2`,
		serverID, target).Scan(&bans))
	require.Equal(t, 1, bans, "the ban MUST land on an unwired replica")
}
