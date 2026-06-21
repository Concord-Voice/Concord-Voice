package voice_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTempGrant inserts a real temporary-SBAC override (is_temporary=true) via the
// exported grant path so the cleanup-trigger tests exercise the production WHERE
// is_temporary semantics rather than a hand-rolled row.
func seedTempGrant(t *testing.T, ts *testhelpers.TestServer, serverID, channelID, userID string) {
	t.Helper()
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	mgr := voice.NewTestTempGrantManager(ts.DB, log, ts.Hub, resolver, nil)
	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, userID))
}

func tempOverrideExists(t *testing.T, db *sql.DB, channelID, userID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(
		   SELECT 1 FROM channel_permission_overrides
		   WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary = true)`,
		channelID, userID,
	).Scan(&exists))
	return exists
}

func keyRevocationCount(t *testing.T, db *sql.DB, channelID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, channelID).Scan(&n))
	return n
}

// TestHandleLeft_TempGrantHolder_TriggersRevoke verifies that when a user holding a
// temporary SBAC grant gracefully leaves a voice channel, the voice.left handler
// converges on revokeTemporaryChannelAccess (#487 T8): the temp override is deleted
// and the channel CSK is rotated (one key_revocations row).
func TestHandleLeft_TempGrantHolder_TriggersRevoke(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "tgleave_owner")
	mover := ts.CreateTestUser(t, "tgleave_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempLeave Server")
	ts.AddMemberToServer(t, serverID, mover.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tgleave")

	seedTempGrant(t, ts, serverID, channelID, mover.ID)
	insertVoiceParticipant(t, ts.DB, channelID, mover.ID)
	require.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "temp grant should exist before leave")

	event := map[string]interface{}{
		"channelId": channelID,
		"userId":    mover.ID,
		"timestamp": "2026-06-15T00:00:00Z",
	}
	sub.HandleLeft(mustJSON(t, event))

	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, mover.ID), "participant row removed on leave")
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "temp override revoked on leave")
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID), "CSK rotated exactly once on temp-grant leave")
}

// TestHandleLeft_NoTempGrant_NoRevoke verifies the common no-temp-grant case skips
// the convergence path entirely: no key_revocations row is inserted when a plain
// participant (no temp override) leaves.
func TestHandleLeft_NoTempGrant_NoRevoke(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "ntgleave_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "NoTempLeave Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ntgleave")
	insertVoiceParticipant(t, ts.DB, channelID, owner.ID)

	event := map[string]interface{}{
		"channelId": channelID,
		"userId":    owner.ID,
		"timestamp": "2026-06-15T00:00:00Z",
	}
	sub.HandleLeft(mustJSON(t, event))

	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, owner.ID), "participant row removed on leave")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID), "no CSK rotation when no temp grant is held")
}

// TestHandleHeartbeat_StaleTempGrantHolder_TriggersRevoke verifies the
// server-authoritative crash-cleanup path: a temp-grant holder reconciled out by the
// heartbeat (client crash / network loss) converges on revoke (#487 T8).
func TestHandleHeartbeat_StaleTempGrantHolder_TriggersRevoke(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "tghb_owner")
	mover := ts.CreateTestUser(t, "tghb_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempHB Server")
	ts.AddMemberToServer(t, serverID, mover.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tghb")

	seedTempGrant(t, ts, serverID, channelID, mover.ID)
	// Age the grant past the 60s heartbeat grace (finding #7) so a stale removal of
	// an established grant still revokes; the grace only protects fresh grants.
	backdateGrantedAt(t, ts, channelID, mover.ID, 120)
	insertVoiceParticipant(t, ts.DB, channelID, owner.ID)
	insertVoiceParticipant(t, ts.DB, channelID, mover.ID)

	// Heartbeat reports only owner → mover is stale and reconciled out.
	event := map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{owner.ID},
		"timestamp": "2026-06-15T00:00:00Z",
	}
	sub.HandleHeartbeat(mustJSON(t, event))

	assert.True(t, voiceParticipantExists(t, ts.DB, channelID, owner.ID), "owner remains after heartbeat")
	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, mover.ID), "stale mover removed after heartbeat")
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "stale mover's temp grant revoked")
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID), "CSK rotated once on stale temp-grant removal")
}

// TestHandleHeartbeat_FreshTempGrantWithinGrace_NotRevoked verifies finding #7:
// a heartbeat that races a brand-new grant→join (grant younger than 60s, user not
// yet in the heartbeat's userIds) does NOT revoke the temp grant. The participant
// row is still reconciled out (transport-level truth), but the grant survives so a
// legitimately-moved user is not stripped of access mid-join.
func TestHandleHeartbeat_FreshTempGrantWithinGrace_NotRevoked(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "fghb_owner")
	mover := ts.CreateTestUser(t, "fghb_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "FreshHB Server")
	ts.AddMemberToServer(t, serverID, mover.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-fghb")

	// Fresh grant (granted_at = NOW(), within grace). The mover has a participant
	// row but the heartbeat does not yet list them (join racing the heartbeat).
	seedTempGrant(t, ts, serverID, channelID, mover.ID)
	insertVoiceParticipant(t, ts.DB, channelID, owner.ID)
	insertVoiceParticipant(t, ts.DB, channelID, mover.ID)

	event := map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{owner.ID},
		"timestamp": "2026-06-15T00:00:00Z",
	}
	sub.HandleHeartbeat(mustJSON(t, event))

	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, mover.ID), "stale participant row still reconciled out")
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "fresh grant within grace must survive the heartbeat")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID), "no CSK rotation for a within-grace fresh grant")
}

// TestHandleHeartbeat_StaleNoTempGrant_NoRevoke verifies a stale participant with no
// temp grant is removed without triggering CSK rotation.
func TestHandleHeartbeat_StaleNoTempGrant_NoRevoke(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "ntghb_owner")
	stale := ts.CreateTestUser(t, "ntghb_stale")
	serverID := ts.CreateTestServer(t, owner.ID, "NoTempHB Server")
	ts.AddMemberToServer(t, serverID, stale.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ntghb")
	insertVoiceParticipant(t, ts.DB, channelID, owner.ID)
	insertVoiceParticipant(t, ts.DB, channelID, stale.ID)

	event := map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{owner.ID},
		"timestamp": "2026-06-15T00:00:00Z",
	}
	sub.HandleHeartbeat(mustJSON(t, event))

	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, stale.ID), "stale participant removed")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID), "no CSK rotation for stale non-temp participant")
}
