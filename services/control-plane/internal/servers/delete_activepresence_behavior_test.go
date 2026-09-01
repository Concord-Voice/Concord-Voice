package servers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func insertVoiceParticipant(t *testing.T, ts *testhelpers.TestServer, channelID, userID string, eventAt time.Time) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)`, channelID, userID, eventAt)
	require.NoError(t, err)
}

func serverDeletePath(serverID string) string { return "/api/v1/servers/" + serverID }

func TestDeleteServerRejectsRawActiveVoiceOverflow(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delactiveoverflowowner")
	serverID := ts.CreateTestServer(t, owner.ID, "DelActiveOverflow")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice")
	eventAt := time.Date(2026, time.January, 16, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 17; i++ {
		user := ts.CreateTestUser(t, "delactiveoverflow"+uuid.NewString()[:8])
		ts.AddMemberToServer(t, serverID, user.ID, "member")
		insertVoiceParticipant(t, ts, channelID, user.ID, eventAt)
	}

	logs := ts.CaptureLogs(t)
	w := ts.DoRequest(http.MethodDelete, serverDeletePath(serverID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusConflict, w.Code)
	assertNeutralActivityConflict(t, w)
	require.Contains(t, logs.String(), "failure_class=candidate_bound")
	require.Equal(t, 1, countRows(t, ts, `SELECT count(*) FROM servers WHERE id = $1`, serverID))
	require.Equal(t, 17, countRows(t, ts, `SELECT count(*) FROM voice_participants WHERE channel_id = $1`, channelID))
	require.Zero(t, countRows(t, ts, `SELECT count(*) FROM presence_active_pending_plans`))
}

func TestDeleteServerRejectsAmbiguousActiveVoiceSender(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delactiveduplicateowner")
	sender := ts.CreateTestUser(t, "delactiveduplicatesender")
	serverID := ts.CreateTestServer(t, owner.ID, "DelActiveDuplicate")
	ts.AddMemberToServer(t, serverID, sender.ID, "member")
	first := ts.CreateVoiceChannel(t, serverID, "first")
	second := ts.CreateVoiceChannel(t, serverID, "second")
	eventAt := time.Date(2026, time.January, 16, 11, 0, 0, 0, time.UTC)
	insertVoiceParticipant(t, ts, first, sender.ID, eventAt)
	insertVoiceParticipant(t, ts, second, sender.ID, eventAt.Add(time.Minute))

	logs := ts.CaptureLogs(t)
	w := ts.DoRequest(http.MethodDelete, serverDeletePath(serverID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusConflict, w.Code)
	assertNeutralActivityConflict(t, w)
	require.Contains(t, logs.String(), "failure_class=candidate_ambiguous")
	require.Equal(t, 1, countRows(t, ts, `SELECT count(*) FROM servers WHERE id = $1`, serverID))
	require.Equal(t, 2, countRows(t, ts, `SELECT count(*) FROM voice_participants WHERE user_id = $1`, sender.ID))
	require.Zero(t, countRows(t, ts, `SELECT count(*) FROM presence_active_pending_plans`))
}

func TestDeleteServerBoundsActiveVoiceSendersNotMembers(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delactiveboundowner")
	serverID := ts.CreateTestServer(t, owner.ID, "DelActiveBound")
	for i := 0; i < 65; i++ {
		member := ts.CreateTestUser(t, "delactiveordinary"+uuid.NewString()[:8])
		ts.AddMemberToServer(t, serverID, member.ID, "member")
	}
	channelID := ts.CreateVoiceChannel(t, serverID, "voice")
	eventAt := time.Date(2026, time.January, 16, 12, 0, 0, 0, time.UTC)
	insertVoiceParticipant(t, ts, channelID, owner.ID, eventAt)

	w := ts.DoRequest(http.MethodDelete, serverDeletePath(serverID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	require.Zero(t, countRows(t, ts, `SELECT count(*) FROM servers WHERE id = $1`, serverID))
	require.Zero(t, countRows(t, ts, `SELECT count(*) FROM voice_participants WHERE channel_id = $1`, channelID))
	require.Zero(t, countRows(t, ts, `SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, owner.ID))
}

func countRows(t *testing.T, ts *testhelpers.TestServer, query string, args ...any) int {
	t.Helper()
	var count int
	require.NoError(t, ts.DB.QueryRow(query, args...).Scan(&count))
	return count
}

func assertNeutralActivityConflict(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, "Server activity changed; retry deletion", body["error"])
}
