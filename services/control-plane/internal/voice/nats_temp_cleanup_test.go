package voice_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
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

func TestHandleLeft_PermanentOverrideRemovesParticipant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "left-permanent-owner")
	target := ts.CreateTestUser(t, "left-permanent-target")
	serverID := ts.CreateTestServer(t, owner.ID, "Permanent Leave Server")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-left-permanent")
	ts.CreateChannelOverride(t, channelID, "user", target.ID, 0, 0)
	insertVoiceParticipant(t, ts.DB, channelID, target.ID)

	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": target.ID, "timestamp": "2026-06-15T00:00:00Z",
	}))

	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, target.ID), "voice.left must remove the participant")
	var overridePresent bool
	require.NoError(t, ts.DB.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM channel_permission_overrides
			WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND NOT is_temporary
		)`, channelID, target.ID).Scan(&overridePresent))
	assert.True(t, overridePresent, "voice.left must preserve the permanent override")
	assert.Zero(t, keyRevocationCount(t, ts.DB, channelID), "permanent override must not rotate the channel key")
}

func TestReconcileOrphanedTemporaryGrant_NullGrantedAtIsEligible(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "orphan_null_grant_owner")
	mover := ts.CreateTestUser(t, "orphan_null_grant_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "Null Grant Reconciliation")
	ts.AddMemberToServer(t, serverID, mover.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-null-grant-reconciliation")
	seedTempGrant(t, ts, serverID, channelID, mover.ID)
	_, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID),
		"a fresh grant must survive the grant-to-join grace")

	_, err = ts.DB.Exec(`
		UPDATE channel_permission_overrides SET granted_at = NULL
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary = TRUE`,
		channelID, mover.ID)
	require.NoError(t, err)

	_, err = sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID),
		"legacy NULL-timestamp grants must converge on the five-second reconciliation rail")
}

func TestHandleLeft_NoParticipantDeletionDoesNotRevokeTempGrant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "left_missing_owner")
	mover := ts.CreateTestUser(t, "left_missing_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "Missing Participant Leave")
	ts.AddMemberToServer(t, serverID, mover.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-left-missing")
	seedTempGrant(t, ts, serverID, channelID, mover.ID)
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)

	// ClearServerVoice has no committed participant deletion to which cleanup can
	// attach; the grant and key material must remain retryable and untouched.
	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    mover.ID,
		"timestamp": "2026-03-30T00:00:00Z",
	}))

	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID))
	assert.Zero(t, keyRevocationCount(t, ts.DB, channelID))
}

func TestHandleLeft_OnlyMoveGrantedTemporaryGrantIsRevoked(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "left_reason_owner")
	target := ts.CreateTestUser(t, "left_reason_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Temporary Reason Server")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-left-reason")
	seedTempGrant(t, ts, serverID, channelID, target.ID)
	tgSeedChannelKey(t, ts.DB, channelID, target.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, target.ID)
	_, err := ts.DB.Exec(`
		UPDATE channel_permission_overrides
		SET temporary_reason = 'manual_temporary_access', granted_at = NOW() - INTERVAL '120 seconds'
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
		  AND is_temporary = TRUE`, channelID, target.ID)
	require.NoError(t, err)
	insertVoiceParticipant(t, ts.DB, channelID, target.ID)

	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": target.ID, "timestamp": "2026-06-15T00:00:00Z",
	}))

	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, target.ID))
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID), "non-move temporary override must survive terminal cleanup")
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, target.ID))
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, target.ID))
	assert.Zero(t, tgKeyRevocationCount(t, ts.DB, channelID))

	sweeper := newTempGrantSweeper(t, ts)
	count, err := sweeper.SweepOrphanedTempGrants(context.Background())
	require.NoError(t, err)
	assert.Zero(t, count, "orphan sweep must ignore non-move temporary grants")
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID))
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, target.ID))
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, target.ID))
	assert.Zero(t, tgKeyRevocationCount(t, ts.DB, channelID))
}

const lateGrantTerminalCommitBarrierLockKey int64 = -2907001

func TestHandleLeft_LateGrantCommitsAfterTerminal(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	log := logger.New("test")
	mgr := voice.NewTestTempGrantManager(
		ts.DB, log, ts.Hub,
		rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log), nil,
	)
	owner := ts.CreateTestUser(t, "late_grant_owner")
	mover := ts.CreateTestUser(t, "late_grant_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "Late Grant Server")
	ts.AddMemberToServer(t, serverID, mover.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-late-grant")
	insertVoiceParticipant(t, ts.DB, channelID, mover.ID)

	barrierConn, err := ts.DB.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, barrierConn.Close()) })
	_, err = barrierConn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, lateGrantTerminalCommitBarrierLockKey)
	require.NoError(t, err)
	released := false
	releaseBarrier := func() error {
		if released {
			return nil
		}
		released = true
		_, unlockErr := barrierConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lateGrantTerminalCommitBarrierLockKey)
		return unlockErr
	}
	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_hold_late_grant_terminal_commit() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(-2907001::bigint);
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER test_hold_late_grant_terminal_commit
			AFTER DELETE ON voice_participants DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION test_hold_late_grant_terminal_commit();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if unlockErr := releaseBarrier(); unlockErr != nil {
			t.Errorf("release late-grant terminal commit barrier: %v", unlockErr)
		}
		if _, cleanupErr := ts.DB.Exec(`DROP TRIGGER IF EXISTS test_hold_late_grant_terminal_commit ON voice_participants; DROP FUNCTION IF EXISTS test_hold_late_grant_terminal_commit();`); cleanupErr != nil {
			t.Errorf("drop late-grant terminal commit barrier: %v", cleanupErr)
		}
	})

	terminalDone := make(chan struct{})
	go func() {
		sub.HandleLeft(mustJSON(t, map[string]interface{}{
			"channelId": channelID, "userId": mover.ID, "timestamp": "2026-06-15T00:00:00Z",
		}))
		close(terminalDone)
	}()
	// The deferred trigger runs only after the terminal transaction has completed
	// its locked override recheck and participant delete, but before it commits.
	dbtest.WaitForAdvisoryLockWaiter(t, ts.DB, lateGrantTerminalCommitBarrierLockKey)

	grantDone := make(chan error, 1)
	go func() {
		grantDone <- mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	}()
	visibilityLockKey, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	dbtest.WaitForAdvisoryLockWaiter(t, ts.DB, visibilityLockKey)
	require.NoError(t, releaseBarrier())

	select {
	case <-terminalDone:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal handler did not finish after lifecycle lock release")
	}
	select {
	case grantErr := <-grantDone:
		require.NoError(t, grantErr)
	case <-time.After(10 * time.Second):
		t.Fatal("late grant did not finish after terminal commit")
	}

	assert.False(t, voiceParticipantExists(t, ts.DB, channelID, mover.ID), "terminal participant deletion must commit first")
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "later serialized grant intent must land after terminal cleanup")
	assert.Zero(t, keyRevocationCount(t, ts.DB, channelID), "terminal saw no grant and must not rotate the channel key")
}

func TestHandleLeft_AmbiguousCommitFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "ambiguous_leave_owner")
	mover := ts.CreateTestUser(t, "ambiguous_leave_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "Ambiguous Leave Server")
	ts.AddMemberToServer(t, serverID, mover.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ambiguous-leave")
	seedTempGrant(t, ts, serverID, channelID, mover.ID)
	insertVoiceParticipant(t, ts.DB, channelID, mover.ID)
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)

	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_fail_terminal_commit() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'simulated terminal commit failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER test_fail_terminal_commit
			AFTER DELETE ON voice_participants DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION test_fail_terminal_commit();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`DROP TRIGGER IF EXISTS test_fail_terminal_commit ON voice_participants; DROP FUNCTION IF EXISTS test_fail_terminal_commit();`)
	})

	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": mover.ID, "timestamp": "2026-06-15T00:00:00Z",
	}))

	assert.True(t, voiceParticipantExists(t, ts.DB, channelID, mover.ID), "ambiguous commit must not claim participant deletion")
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "ambiguous commit must preserve retryable grant")
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "ambiguous commit must preserve retryable key")
	assert.Zero(t, keyRevocationCount(t, ts.DB, channelID), "ambiguous commit must not announce a rotation")
}

func TestHandleHeartbeat_AmbiguousCommitEmitsNoRosterOrCountRemoval(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	owner := ts.CreateTestUser(t, "heartbeat_ambiguous_owner")
	stale := ts.CreateTestUser(t, "heartbeat_ambiguous_stale")
	viewer := ts.CreateTestUser(t, "heartbeat_ambiguous_viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "Ambiguous Heartbeat Server")
	ts.AddMemberToServer(t, serverID, stale.ID, "member")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ambiguous-heartbeat")

	conn := connectVoiceWireClient(t, ts, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_server", "data": map[string]interface{}{"server_id": serverID},
	}))
	synchronizeVoiceWireClient(t, conn)
	waitForVoiceWireType(t, conn, "server_voice_counts") // Drain the subscription catch-up.
	// Keep the observer connected so a pre-disconnect roster/count emission is
	// visible instead of being hidden by the conservative recovery disconnect.
	sub.SetDisconnectAllRichPresenceClientsHookForTest(func() {})
	// Insert after the on-subscribe count catch-up so the assertion observes only
	// the heartbeat's own output; this direct fixture write emits no count signal.
	insertVoiceParticipant(t, ts.DB, channelID, stale.ID)
	cacheKey := "perm:" + serverID + ":" + stale.ID + ":" + channelID
	require.NoError(t, ts.Redis.Set(context.Background(), cacheKey, int64(rbac.PermJoinVoice), time.Minute).Err())

	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_fail_heartbeat_terminal_commit() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'simulated heartbeat terminal commit failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER test_fail_heartbeat_terminal_commit
			AFTER DELETE ON voice_participants DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION test_fail_heartbeat_terminal_commit();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`DROP TRIGGER IF EXISTS test_fail_heartbeat_terminal_commit ON voice_participants; DROP FUNCTION IF EXISTS test_fail_heartbeat_terminal_commit();`)
	})

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{},
		"timestamp": "2026-06-15T00:00:00Z",
	}))

	require.True(t, voiceParticipantExists(t, ts.DB, channelID, stale.ID),
		"ambiguous commit must leave the participant durable")
	cacheExists, err := ts.Redis.Exists(context.Background(), cacheKey).Result()
	require.NoError(t, err)
	assert.Zero(t, cacheExists, "ambiguous participant deletion must invalidate cached channel authority")
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	for {
		var envelope voiceWireEnvelope
		if err := conn.ReadJSON(&envelope); err != nil {
			break // Reconciliation failure may fail closed by disconnecting the client.
		}
		if envelope.Type == "voice_state_update" && envelope.Data["action"] == "left" &&
			envelope.Data["channel_id"] == channelID && envelope.Data["user_id"] == stale.ID {
			require.Fail(t, "ambiguous commit emitted a stale participant roster-left update")
		}
		if envelope.Type == "server_voice_counts" {
			counts, ok := envelope.Data["counts"].(map[string]interface{})
			if ok {
				if _, sent := counts[serverID]; sent {
					require.Failf(t, "ambiguous commit emitted a server voice count update", "%v", envelope.Data)
				}
			}
		}
	}
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

	assert.True(t, voiceParticipantExists(t, ts.DB, channelID, mover.ID), "fresh grant retains the participant retry handle")
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
