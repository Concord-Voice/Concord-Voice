package servers_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

type serverDeleteRecoveryReader struct {
	subject uuid.UUID
	state   presence.ActivityState
}

func (r serverDeleteRecoveryReader) GetWithLease(
	_ context.Context,
	subject uuid.UUID,
	category presence.Category,
) (presence.ActivityState, bool, error) {
	if subject != r.subject || category != presence.CategoryServerVoice {
		return presence.ActivityState{}, false, nil
	}
	return r.state, true, nil
}

type serverDeleteRecoveryTerminal struct {
	clears          []activepresence.PlanKey
	disconnects     int
	deleteCalls     int
	deletedSubject  uuid.UUID
	deletedCategory presence.Category
	deletedToken    uuid.UUID
	deletedVersion  int64
}

func (d *serverDeleteRecoveryTerminal) CompareAndDelete(
	_ context.Context,
	subject uuid.UUID,
	category presence.Category,
	token uuid.UUID,
	version int64,
) (bool, error) {
	d.deleteCalls++
	d.deletedSubject = subject
	d.deletedCategory = category
	d.deletedToken = token
	d.deletedVersion = version
	return true, nil
}

func (d *serverDeleteRecoveryTerminal) ClearSenderActiveCategory(
	subject uuid.UUID,
	category presence.Category,
) {
	d.clears = append(d.clears, activepresence.PlanKey{SubjectID: subject, Category: category})
}

func (d *serverDeleteRecoveryTerminal) DisconnectAllRichPresenceClients(context.Context) error {
	d.disconnects++
	return nil
}

// Regression test for #2902. The oracle is deliberately end-to-end: when active Server Voice evidence is
// destroyed by a committed server deletion and plan acknowledgement fails,
// the endpoint returns 503 and an exact durable plan remains for restart
// recovery.
func TestDeleteServerPersistsActivePresencePlanWhenAcknowledgementFails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "server-delete-active-owner")
	control := ts.CreateTestUser(t, "server-delete-active-control")
	member := ts.CreateTestUser(t, "server-delete-active-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Active Presence Delete")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "Voice")

	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	wsURL := "ws" + wsServer.URL[len("http"):]
	wsURL += "/api/v1/ws?activity_rich_presence=1"
	wsHeaders := http.Header{}
	wsHeaders.Set("Authorization", "Bearer "+member.AccessToken)
	memberClient, _, err := websocket.DefaultDialer.Dial(wsURL, wsHeaders)
	require.NoError(t, err, "an affected server member must have an activity-capable client")
	t.Cleanup(func() { _ = memberClient.Close() })
	var bootstrapFrame struct {
		Type string `json:"type"`
	}
	for bootstrapFrame.Type != "presence_snapshot" {
		require.NoError(t, memberClient.ReadJSON(&bootstrapFrame),
			"the affected member client must finish activity bootstrap before deletion")
	}
	eventAt := time.Date(2026, time.January, 15, 10, 30, 0, 0, time.UTC)
	_, err = ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)`, channelID, owner.ID, eventAt)
	require.NoError(t, err)
	var activeRows int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		channelID, owner.ID).Scan(&activeRows))
	require.Equal(t, 1, activeRows, "the fixture must contain active Server Voice evidence")

	// Positive control: a pre-existing valid obligation must survive the same
	// failed acknowledgement transaction, independently of the deleted server.
	controlOperation := uuid.New()
	_, err = ts.DB.Exec(`
		INSERT INTO presence_active_pending_plans
			(user_id, category, operation_id, resolution, scope_lifecycle_id, scope_event_at)
		VALUES ($1, 'server_voice', $2, 'exact', $3, $4)`,
		control.ID, controlOperation, uuid.New(), eventAt)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`
		CREATE OR REPLACE FUNCTION concord_test_fail_active_plan_ack() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'concord test: forced active plan acknowledgement failure';
		END;
		$$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		CREATE TRIGGER concord_test_fail_active_plan_ack_trg
		BEFORE DELETE ON presence_active_pending_plans
		FOR EACH ROW EXECUTE FUNCTION concord_test_fail_active_plan_ack()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec(`DROP TRIGGER IF EXISTS concord_test_fail_active_plan_ack_trg ON presence_active_pending_plans`); err != nil {
			t.Errorf("drop active plan trigger: %v", err)
		}
		if _, err := ts.DB.Exec(`DROP FUNCTION IF EXISTS concord_test_fail_active_plan_ack()`); err != nil {
			t.Errorf("drop active plan trigger function: %v", err)
		}
	})

	_, err = ts.DB.Exec(`DELETE FROM presence_active_pending_plans WHERE user_id = $1 AND operation_id = $2`, control.ID, controlOperation)
	require.ErrorContains(t, err, "forced active plan acknowledgement failure",
		"the positive control must prove the acknowledgement trigger fires")
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM presence_active_pending_plans
		WHERE user_id = $1 AND operation_id = $2`, control.ID, controlOperation).Scan(&activeRows))
	require.Equal(t, 1, activeRows, "the positive-control plan must survive the failed acknowledgement")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"when active Server Voice evidence is destroyed by a committed server deletion and plan acknowledgement fails, the endpoint returns 503 and an exact durable plan remains for restart recovery")
	require.NoError(t, memberClient.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, _, readErr := memberClient.ReadMessage()
		if readErr == nil {
			continue
		}
		var timeoutErr net.Error
		if errors.As(readErr, &timeoutErr) {
			require.False(t, timeoutErr.Timeout(),
				"the committed server deletion must close the affected member's Custom Status client")
		}
		break
	}

	var serverRows, voiceRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM servers WHERE id = $1`, serverID).Scan(&serverRows))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM voice_participants WHERE channel_id = $1`, channelID).Scan(&voiceRows))
	assert.Zero(t, serverRows, "the server deletion must commit before acknowledgement")
	assert.Zero(t, voiceRows, "voice evidence must be destroyed by the committed deletion")

	var category, resolution string
	var lifecycle uuid.UUID
	var planEvent time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, owner.ID).Scan(&activeRows))
	assert.Equal(t, 1, activeRows,
		"the exact durable plan for the active sender must remain after acknowledgement failure")
	if activeRows == 1 {
		err = ts.DB.QueryRow(`
			SELECT category, resolution, scope_lifecycle_id, scope_event_at
			FROM presence_active_pending_plans
			WHERE user_id = $1`, owner.ID).Scan(&category, &resolution, &lifecycle, &planEvent)
		require.NoError(t, err)
		assert.Equal(t, "server_voice", category)
		assert.Equal(t, "exact", resolution)
		assert.Equal(t, channelID, lifecycle.String())
		assert.True(t, planEvent.Equal(eventAt), "the durable plan must retain the seeded lifecycle timestamp")
	}

	var controlRows int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM presence_active_pending_plans
		WHERE user_id = $1 AND operation_id = $2`, control.ID, controlOperation).Scan(&controlRows))
	assert.Equal(t, 1, controlRows,
		"the positive-control plan must be preserved when the acknowledgement trigger fires")

	_, err = ts.DB.Exec(`DROP TRIGGER concord_test_fail_active_plan_ack_trg ON presence_active_pending_plans`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		DELETE FROM presence_active_pending_plans
		WHERE user_id = $1 AND operation_id = $2`, control.ID, controlOperation)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		UPDATE presence_active_pending_plans
		SET created_at = clock_timestamp() - interval '2 seconds',
		    reconcile_after = clock_timestamp() - interval '1 second'
		WHERE user_id = $1`, owner.ID)
	require.NoError(t, err)

	ownerID := uuid.MustParse(owner.ID)
	terminal := &serverDeleteRecoveryTerminal{}
	reconciler := activepresence.NewReconciler(
		ts.DB,
		ts.PresenceHistory,
		serverDeleteRecoveryReader{
			subject: ownerID,
			state: presence.ActivityState{
				SourceToken:   uuid.MustParse(channelID),
				SourceVersion: eventAt.UnixMicro(),
			},
		},
		terminal,
		terminal,
		nil,
	)
	stats, err := reconciler.ReconcilePass(context.Background(), 16)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Cleared, "the fresh reconciler must deliver the retained exact plan")
	assert.Equal(t, 1, terminal.deleteCalls, "the fresh reconciler must remove the stale generation")
	assert.Equal(t, ownerID, terminal.deletedSubject)
	assert.Equal(t, activepresence.CategoryServerVoice, terminal.deletedCategory)
	assert.Equal(t, uuid.MustParse(channelID), terminal.deletedToken)
	assert.Equal(t, eventAt.UnixMicro(), terminal.deletedVersion)
	assert.Equal(t, []activepresence.PlanKey{{
		SubjectID: ownerID,
		Category:  activepresence.CategoryServerVoice,
	}}, terminal.clears)
	assert.Zero(t, terminal.disconnects, "ordinary recovery must remain proportional")
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, owner.ID).Scan(&activeRows))
	assert.Zero(t, activeRows, "the fresh reconciler must acknowledge and remove the retained plan")
}
