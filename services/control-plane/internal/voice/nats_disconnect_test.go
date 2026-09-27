package voice_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const natsSubjectEnforceDisconnectForTest = "voice.enforce.disconnect"

// natsTestURL returns the NATS URL for integration tests (dev default: no auth).
func natsTestURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return "nats://localhost:4222"
}

// TestPublishForceDisconnect_PublishesPayload verifies the new voice.enforce.disconnect
// publisher (#487 P3) emits {channelId, userId} on the correct subject. Requires a live
// NATS server (dev env / CI); skips if NATS is unreachable.
func TestPublishForceDisconnect_PublishesPayload(t *testing.T) {
	// Control-side client passed into the subscriber (the publisher).
	pubClient, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live publish test (runs in CI)", err)
	}
	t.Cleanup(func() { _ = pubClient.Close() })

	// Observer client subscribes to capture the published message.
	obsClient, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = obsClient.Close() })

	ts := testhelpers.SetupTestServer(t)
	sub := voice.NewNATSSubscriber(ts.DB, logger.New("test"), ts.Hub, pubClient, ts.Redis, nil, nil)

	received := make(chan []byte, 1)
	natsSub, err := obsClient.Subscribe("voice.enforce.disconnect", func(data []byte) {
		received <- data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = natsSub.Unsubscribe() })

	// Flush the observer connection so the subscription interest has reached the
	// server before we publish — otherwise a fast publish can race ahead of the
	// subscription registration and the message is dropped (intermittent timeout).
	require.NoError(t, obsClient.Flush())

	const channelID = "11111111-1111-1111-1111-111111111111"
	const userID = "22222222-2222-2222-2222-222222222222"
	sub.PublishForceDisconnect(channelID, userID)

	select {
	case data := <-received:
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &payload))
		assert.Equal(t, channelID, payload["channelId"], "channelId should be in the payload")
		assert.Equal(t, userID, payload["userId"], "userId should be in the payload")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for voice.enforce.disconnect message")
	}
}

// TestPublishForceDisconnect_NilNATSNoop verifies the publisher is a safe no-op
// when the NATS client is nil (the test/default construction path), mirroring the
// publishEnforcementFlags nil guard.
func TestPublishForceDisconnect_NilNATSNoop(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := voice.NewNATSSubscriber(ts.DB, logger.New("test"), ts.Hub, nil, ts.Redis, nil, nil)
	// Must not panic when nats is nil.
	sub.PublishForceDisconnect("chan", "user")
}

func TestVoiceJoinedPublishesCompleteRevisionedEnforcementSnapshot(t *testing.T) {
	pubClient, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live publish test", err)
	}
	t.Cleanup(func() { _ = pubClient.Close() })
	obsClient, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = obsClient.Close() })

	t.Run("server", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		user := ts.CreateTestUser(t, "joined_snapshot_server")
		serverID := ts.CreateTestServer(t, user.ID, "Joined Snapshot Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "joined-snapshot")
		received := subscribeForEnforcementSnapshot(t, obsClient, channelID, user.ID)
		sub := newTestSubscriberWithHubAndNATS(ts, ts.Hub, pubClient)
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": channelID, "userId": user.ID, "username": user.Username,
			"timestamp": "2026-03-30T00:00:00Z",
		}))
		assertCompleteEnforcementSnapshot(t, received, channelID, user.ID)
	})

	t.Run("dm", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		user := ts.CreateTestUser(t, "joined_snapshot_dm_user")
		peer := ts.CreateTestUser(t, "joined_snapshot_dm_peer")
		conversationID := ts.CreateDMConversation(t, user.ID, peer.ID)
		callID := uuid.New()
		require.NoError(t, dm.RefreshDMVoiceCallLease(context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: uuid.MustParse(conversationID), CallID: callID, CallerUserID: uuid.MustParse(user.ID),
		}, dm.DMVoiceCallLeaseTTL, true))
		received := subscribeForEnforcementSnapshot(t, obsClient, conversationID, user.ID)
		sub := newTestSubscriberWithHubAndNATS(ts, ts.Hub, pubClient)
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID, "callId": callID.String(), "userId": user.ID,
			"username": user.Username, "timestamp": "2026-03-30T00:00:00Z",
		}))
		assertCompleteEnforcementSnapshot(t, received, conversationID, user.ID)
	})
}

func subscribeForEnforcementSnapshot(t *testing.T, obsClient *natsclient.Client, channelID, userID string) <-chan map[string]interface{} {
	t.Helper()
	received := make(chan map[string]interface{}, 1)
	natsSub, err := obsClient.Subscribe("voice.enforce.mute", func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil && payload["channelId"] == channelID && payload["userId"] == userID {
			received <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = natsSub.Unsubscribe() })
	require.NoError(t, obsClient.Flush())
	return received
}

func assertCompleteEnforcementSnapshot(t *testing.T, received <-chan map[string]interface{}, channelID, userID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case payload := <-received:
			if payload["channelId"] != channelID || payload["userId"] != userID {
				continue
			}
			assert.Equal(t, "unmute", payload["action"])
			assert.Equal(t, false, payload["serverMuted"])
			assert.Equal(t, false, payload["serverDeafened"])
			assert.Equal(t, float64(1), payload["version"])
			revision, ok := payload["authorizationRevision"].(string)
			require.True(t, ok, "revision must be serialized as a string")
			parsedRevision, err := strconv.ParseInt(revision, 10, 64)
			require.NoError(t, err, "revision must be a base-10 integer")
			assert.Positive(t, parsedRevision)
			return
		case <-deadline:
			t.Fatal("timed out waiting for complete voice enforcement snapshot")
		}
	}
}
