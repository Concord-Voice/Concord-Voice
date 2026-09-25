package dm_test

// Expiring a stale DM call row before a ring must retract that user's Private
// Call badge. The delete used to send nothing, so a viewer kept the badge, and
// with the row gone no durable evidence said a badge was still outstanding
// (#3444). The ring path now records a clear plan in the same transaction.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func privateCallPlanCount(t *testing.T, ts *testhelpers.TestServer, userID string) int {
	t.Helper()
	var count int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM presence_active_pending_plans
		WHERE user_id = $1 AND category = 'private_call'
	`, userID).Scan(&count))
	return count
}

func dmVoiceRowCount(t *testing.T, ts *testhelpers.TestServer, convID, userID string) int {
	t.Helper()
	var count int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2
	`, convID, userID).Scan(&count))
	return count
}

func insertStaleDMVoiceRow(t *testing.T, ts *testhelpers.TestServer, convID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, NOW() - INTERVAL '10 minutes', NOW() - INTERVAL '10 minutes')
	`, convID, userID)
	require.NoError(t, err)
}

func ringDM(t *testing.T, ts *testhelpers.TestServer, caller testhelpers.TestUser, convID string) int {
	t.Helper()
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil,
		testhelpers.AuthHeaders(caller.AccessToken))
	return w.Code
}

// dialRichPresenceViewer connects a websocket client that receives Rich
// Presence frames; the clear fan-out skips clients without the capability.
func dialRichPresenceViewer(t *testing.T, ts *testhelpers.TestServer, userID string) *gorillaWS.Conn {
	t.Helper()
	ticket := "dm-ring-clear-viewer-" + uuid.NewString()
	require.NoError(t, ts.Redis.Set(t.Context(), "ws_ticket:"+ticket, userID+":ring-viewer", time.Minute).Err())
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial(
		"ws"+wsServer.URL[4:]+"/api/v1/ws?ticket="+ticket+"&activity_rich_presence=1", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	readUntilDMEvent(t, conn, "connected")
	return conn
}

func TestRingDMCall_ExpiredStaleRowClearsViewerBadge(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "ring_clear_caller")
	callee := ts.CreateTestUser(t, "ring_clear_callee")
	viewer := ts.CreateTestUser(t, "ring_clear_viewer")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	insertStaleDMVoiceRow(t, ts, convID, callee.ID)
	viewerConn := dialRichPresenceViewer(t, ts, viewer.ID)

	require.Equal(t, http.StatusOK, ringDM(t, ts, caller, convID))
	require.Zero(t, dmVoiceRowCount(t, ts, convID, callee.ID))
	require.Equal(t, 1, privateCallPlanCount(t, ts, callee.ID),
		"expiring the stale row must leave a durable clear obligation")

	_, err := ts.ActivePlanReconciler.ReconcilePass(context.Background(), 10)
	require.NoError(t, err)
	frame := readUntilDMEvent(t, viewerConn, "rich_presence_clear")
	require.Equal(t, callee.ID, frame["user_id"])
	require.Equal(t, "private_call", frame["category"])
	require.Zero(t, privateCallPlanCount(t, ts, callee.ID), "the delivered plan is acknowledged")
}

// A viewer who could see only the expired call must still lose the badge, so
// the plan is recorded even though the user is live in another conversation.
func TestRingDMCall_ExpiredStaleRowRecordsPlanWhileAnotherCallIsLive(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "ring_live_caller")
	callee := ts.CreateTestUser(t, "ring_live_callee")
	other := ts.CreateTestUser(t, "ring_live_other")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	liveConvID := ts.CreateDMConversation(t, callee.ID, other.ID)
	insertStaleDMVoiceRow(t, ts, convID, callee.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)
	`, liveConvID, callee.ID)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, ringDM(t, ts, caller, convID))
	require.Zero(t, dmVoiceRowCount(t, ts, convID, callee.ID), "the stale row is expired")
	require.Equal(t, 1, dmVoiceRowCount(t, ts, liveConvID, callee.ID), "the live row is untouched")
	require.Equal(t, 1, privateCallPlanCount(t, ts, callee.ID))
}

func TestRingDMCall_ExpiresEveryStaleRowInOneRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "ring_both_caller")
	callee := ts.CreateTestUser(t, "ring_both_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	insertStaleDMVoiceRow(t, ts, convID, caller.ID)
	insertStaleDMVoiceRow(t, ts, convID, callee.ID)

	require.Equal(t, http.StatusOK, ringDM(t, ts, caller, convID))
	require.Zero(t, dmVoiceRowCount(t, ts, convID, caller.ID))
	require.Zero(t, dmVoiceRowCount(t, ts, convID, callee.ID))
	require.Equal(t, 1, privateCallPlanCount(t, ts, caller.ID))
	require.Equal(t, 1, privateCallPlanCount(t, ts, callee.ID))
}

// The delete and the plan commit together: a failed plan insert must fail the
// ring and keep the stale row, or the delete would drop the only evidence.
func TestRingDMCall_FailedPlanCaptureKeepsTheStaleRow(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "ring_fail_caller")
	callee := ts.CreateTestUser(t, "ring_fail_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	insertStaleDMVoiceRow(t, ts, convID, callee.ID)
	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_reject_ring_plan_capture() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced plan capture failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_ring_plan_capture
		BEFORE INSERT ON presence_active_pending_plans
		FOR EACH ROW EXECUTE FUNCTION test_reject_ring_plan_capture()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_reject_ring_plan_capture ON presence_active_pending_plans;
			DROP FUNCTION IF EXISTS test_reject_ring_plan_capture()`)
		require.NoError(t, cleanupErr)
	})

	require.Equal(t, http.StatusInternalServerError, ringDM(t, ts, caller, convID))
	require.Equal(t, 1, dmVoiceRowCount(t, ts, convID, callee.ID), "the delete must roll back")
	require.Zero(t, privateCallPlanCount(t, ts, callee.ID))
	require.False(t, dm.PendingDMCallExistsForTest(uuid.MustParse(convID)))
}
