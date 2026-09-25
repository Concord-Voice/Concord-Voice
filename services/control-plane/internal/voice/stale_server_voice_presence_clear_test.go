package voice_test

// A lease-reaped Server Voice participant must retract its Rich Presence badge.
// The reaper used to delete the row and send nothing, so a viewer kept the
// badge. Once the terminal outbox row drained, no durable evidence said a badge
// was still outstanding, so #3444's settings guard could skip a revocation that
// viewer needed. The reaper now records a clear plan in its own transaction.

import (
	"context"
	"testing"
	"time"

	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func serverVoicePlanCount(t *testing.T, ts *testhelpers.TestServer, userID string) int {
	t.Helper()
	var count int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM presence_active_pending_plans
		WHERE user_id = $1 AND category = $2
	`, userID, string(presence.CategoryServerVoice)).Scan(&count))
	return count
}

// lapseHeartbeatKeys expires every Redis key the sender's voice heartbeat
// renews, which is what a reapable participant looks like in production: the
// lease and those keys run on the same clock.
func lapseHeartbeatKeys(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	ctx := context.Background()
	var keys []string
	var cursor uint64
	for {
		page, next, err := ts.Redis.Scan(ctx, cursor, "*"+userID+"*", 1000).Result()
		require.NoError(t, err)
		keys = append(keys, page...)
		if cursor = next; cursor == 0 {
			break
		}
	}
	var lapsed []string
	for _, key := range keys {
		ttl, err := ts.Redis.PTTL(ctx, key).Result()
		require.NoError(t, err)
		if ttl > 0 && ttl <= presence.ActivityStateTTL {
			require.NoError(t, ts.Redis.PExpire(ctx, key, time.Millisecond).Err())
			lapsed = append(lapsed, key)
		}
	}
	require.NotEmpty(t, lapsed, "the join must have written heartbeat-renewed keys")
	require.Eventually(t, func() bool {
		n, err := ts.Redis.Exists(ctx, lapsed...).Result()
		return err == nil && n == 0
	}, time.Second, 5*time.Millisecond)
}

// readServerVoiceClear reads the viewer socket until it sees a Server Voice
// clear for userID or the window closes. One reader with no deadline, because
// gorilla/websocket poisons a connection after a read timeout.
func readServerVoiceClear(conn *gorillaWS.Conn, userID string, window time.Duration) bool {
	seen := make(chan struct{})
	go func() {
		for {
			var envelope voiceWireEnvelope
			if err := conn.ReadJSON(&envelope); err != nil {
				return
			}
			if envelope.Type == "rich_presence_clear" && envelope.Data["user_id"] == userID &&
				envelope.Data["category"] == string(presence.CategoryServerVoice) {
				close(seen)
				return
			}
		}
	}()
	select {
	case <-seen:
		return true
	case <-time.After(window):
		return false
	}
}

func TestStaleServerVoiceReapClearsViewerBadge(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()

	sender := ts.CreateTestUser(t, "reap_clear_sender")
	viewer := ts.CreateTestUser(t, "reap_clear_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "Reap Clear Server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "reap-clear-voice")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)

	viewerConn := connectVoiceWireClient(t, ts, viewer)
	synchronizeVoiceWireClient(t, viewerConn)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": sender.ID,
		"username": sender.Username, "displayName": "Reap Sender",
		"timestamp": time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano),
	}))
	update := waitForVoiceWireType(t, viewerConn, "rich_presence_update")
	require.Equal(t, sender.ID, update.Data["user_id"], "viewer holds the sender's badge")

	lapseHeartbeatKeys(t, ts, sender.ID)
	ageObservedLease(t, ts.DB, channelID, sender.ID)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	require.Equal(t, 1, serverVoicePlanCount(t, ts, sender.ID),
		"the reap must leave a durable clear obligation")

	_, err = ts.ActivePlanReconciler.ReconcilePass(ctx, 10)
	require.NoError(t, err)
	require.True(t, readServerVoiceClear(viewerConn, sender.ID, 2*time.Second),
		"draining the plan must retract the viewer's badge")
	require.Zero(t, serverVoicePlanCount(t, ts, sender.ID), "the delivered plan is acknowledged")
}

// A viewer who could see only the reaped channel must still lose the badge, so
// the plan is recorded even though the user is live in another channel.
func TestStaleServerVoiceReapRecordsPlanWhileAnotherRowIsLive(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()

	sender := ts.CreateTestUser(t, "reap_live_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "Reap Live Server")
	staleChannel := ts.CreateVoiceChannel(t, serverID, "reap-stale-voice")
	liveChannel := ts.CreateVoiceChannel(t, serverID, "reap-live-voice")
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": staleChannel, "userId": sender.ID,
		"username": sender.Username, "displayName": "Reap Sender",
		"timestamp": time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano),
	}))
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
		liveChannel, sender.ID)
	require.NoError(t, err)

	ageObservedLease(t, ts.DB, staleChannel, sender.ID)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	require.False(t, voiceParticipantExists(t, ts.DB, staleChannel, sender.ID))
	require.True(t, voiceParticipantExists(t, ts.DB, liveChannel, sender.ID))
	require.Equal(t, 1, serverVoicePlanCount(t, ts, sender.ID))
}

// The reap, its outbox row and its plan commit together: a failed plan insert
// must fail the reap and keep the row, or the delete would drop the only
// evidence a badge is outstanding.
func TestStaleServerVoiceReapPlanFailureRollsBackTheReap(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()

	sender := ts.CreateTestUser(t, "reap_fail_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "Reap Fail Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "reap-fail-voice")
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": sender.ID,
		"username": sender.Username, "displayName": "Reap Sender",
		"timestamp": time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano),
	}))
	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_reject_reap_plan_capture() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced plan capture failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_reap_plan_capture
		BEFORE INSERT ON presence_active_pending_plans
		FOR EACH ROW EXECUTE FUNCTION test_reject_reap_plan_capture()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_reject_reap_plan_capture ON presence_active_pending_plans;
			DROP FUNCTION IF EXISTS test_reject_reap_plan_capture()`)
		require.NoError(t, cleanupErr)
	})

	ageObservedLease(t, ts.DB, channelID, sender.ID)
	sub.CompleteServerVoiceCleanupGraceForTest()
	removed, err := sub.ReconcileStaleServerVoiceParticipants(ctx, 10)
	require.ErrorContains(t, err, "capture reaped server voice presence clear")
	require.Zero(t, removed)
	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID), "the delete must roll back")
	var outboxRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM server_voice_terminal_outbox WHERE user_id = $1`, sender.ID).Scan(&outboxRows))
	require.Zero(t, outboxRows, "the outbox row must roll back with the delete")
}
