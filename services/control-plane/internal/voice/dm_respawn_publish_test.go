package voice_test

import (
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A completed call's history row respawns a thread the callee hid, and the
// callee's client learns it only from the published visibility frame (#2822).
func TestHandleRoomEmpty_DMSummaryRespawnPublishesToHiddenParticipant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	hub, baseURL := newVoiceReplicaHub(t, ts)
	sub := newTestSubscriberWithHub(ts, hub)

	caller := ts.CreateTestUser(t, "respawn_summary_caller")
	callee := ts.CreateTestUser(t, "respawn_summary_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET hidden_at = clock_timestamp() - interval '1 minute'
		WHERE conversation_id = $1 AND user_id = $2`, convID, callee.ID)
	require.NoError(t, err)

	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, callee)
	synchronizeVoiceWireClient(t, conn)

	endedAt := time.Now().UTC()
	sub.HandleRoomEmptyAt(mustJSON(t, map[string]interface{}{
		"channelId":          convID,
		"callId":             uuid.NewString(),
		"callerUserId":       caller.ID,
		"participantUserIds": []string{caller.ID, callee.ID},
		"startedAt":          endedAt.Add(-time.Minute).Format(time.RFC3339),
		"timestamp":          endedAt.Format(time.RFC3339),
	}), endedAt)

	frame := waitForVoiceWireType(t, conn, "dm_conversation_hidden")
	assert.Equal(t, convID, frame.Data["conversation_id"])
	assert.Contains(t, frame.Data, "hidden_at")
	assert.Nil(t, frame.Data["hidden_at"], "a respawn publishes the visible (null) state")
}
