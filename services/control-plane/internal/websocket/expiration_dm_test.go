package websocket

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersistDMMessage_UsesSharedExpirationPolicy(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	for _, tc := range []struct {
		name   string
		window any
		want   bool
	}{
		{name: "disabled policy leaves expiry null", window: nil, want: false},
		{name: "one hour policy stamps expiry", window: 3600, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := setup.db.Exec(`UPDATE dm_conversations SET expiration_window_seconds = $1 WHERE id = $2`, tc.window, setup.convID)
			require.NoError(t, err)
			_, createdAt, _, expiresAt, _, err := setup.hub.persistDMMessageWithExpiry(
				mustUUID(t, setup.convID), setup.user1, "", &dmMessageInput{content: "ciphertext", keyVersion: 1, msgType: "user"},
			)
			require.NoError(t, err)
			if tc.want {
				require.NotNil(t, expiresAt)
				assert.WithinDuration(t, createdAt.Add(time.Hour), *expiresAt, time.Microsecond)
			} else {
				assert.Nil(t, expiresAt)
			}
		})
	}
}

func TestPersistMessage_UsesSharedExpirationPolicy(t *testing.T) {
	setup := setupMessageTest(t)
	channelID := mustUUID(t, setup.convID)
	incarnation := membershipIncarnation(t, setup.db, setup.user2, setup.user1)

	for _, tc := range []struct {
		name   string
		window any
		want   bool
	}{
		{name: "disabled policy leaves expiry null", window: nil, want: false},
		{name: "one hour policy stamps expiry", window: 3600, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := setup.db.Exec(`UPDATE channels SET expiration_window_seconds = $1 WHERE id = $2`, tc.window, setup.convID)
			require.NoError(t, err)
			_, createdAt, _, expiresAt, _, persistErr, _ := setup.hub.persistMessageWithExpiry(persistMessageParams{
				channelUUID: channelID, userID: setup.user1, membershipIncarnation: incarnation,
				content: "ciphertext", keyVersion: 1,
			})
			require.Empty(t, persistErr)
			if tc.want {
				require.NotNil(t, expiresAt)
				assert.WithinDuration(t, createdAt.Add(time.Hour), *expiresAt, time.Microsecond)
			} else {
				assert.Nil(t, expiresAt)
			}
		})
	}
}

func TestSendDMMessageAckIncludesExpiry(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	expiresAt := time.Now().UTC().Add(time.Hour)
	setup.hub.sendDMMessageAck(dmMessageAckParams{
		client: setup.client, nonce: "n", messageID: mustUUID(t, setup.convID), convUUID: mustUUID(t, setup.convID),
		createdAt: time.Now().UTC(), updatedAt: time.Now().UTC(), expiresAt: &expiresAt,
	})
	select {
	case data := <-setup.client.Send:
		var msg map[string]any
		require.NoError(t, json.Unmarshal(data, &msg))
		payload, ok := msg["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, expiresAt.Format(time.RFC3339Nano), payload["expires_at"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DM acknowledgement")
	}
}

func TestSendMessageAckAndDMBroadcastIncludeExactExpiry(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	setup.hub.sendMessageAck(messageAck{
		Client: setup.client, Nonce: "n", MessageID: mustUUID(t, setup.convID), ChannelUUID: mustUUID(t, setup.convID),
		CreatedAt: expiresAt.Add(-time.Hour), UpdatedAt: expiresAt.Add(-time.Hour), ExpiresAt: &expiresAt,
	})
	select {
	case data := <-setup.client.Send:
		var msg map[string]any
		require.NoError(t, json.Unmarshal(data, &msg))
		payload, ok := msg["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, expiresAt.Format(time.RFC3339Nano), payload["expires_at"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel acknowledgement")
	}

	setup.hub.broadcastDMMessage(dmBroadcastCtx{
		messageID: mustUUID(t, setup.convID), convUUID: mustUUID(t, setup.convID), senderUserID: setup.user1,
		client: setup.client, input: &dmMessageInput{content: "ciphertext", keyVersion: 1, msgType: "user"},
		createdAt: expiresAt.Add(-time.Hour), updatedAt: expiresAt.Add(-time.Hour), expiresAt: &expiresAt,
	})
	select {
	case broadcast := <-setup.hub.dmBroadcast:
		payload := broadcast.Data.Data
		got, ok := payload["expires_at"].(*time.Time)
		require.True(t, ok)
		assert.Equal(t, expiresAt, *got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DM broadcast")
	}
}

func TestChannelBroadcastIncludesExactExpiry(t *testing.T) {
	setup := setupMessageTest(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	data := setup.hub.buildMessageBroadcast(messageBroadcastCtx{
		messageID: mustUUID(t, setup.convID), channelUUID: mustUUID(t, setup.convID), userID: setup.user1,
		client: setup.client, input: &messageInput{content: "ciphertext", keyVersion: 1},
		createdAt: expiresAt.Add(-time.Hour), updatedAt: expiresAt.Add(-time.Hour), expiresAt: &expiresAt,
	})
	got, ok := data["expires_at"].(*time.Time)
	require.True(t, ok)
	assert.Equal(t, expiresAt, *got)
}

func TestPersistDMMessage_RemovedParticipantCannotWriteAfterParentLock(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	_, err := setup.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, setup.convID, setup.user1)
	require.NoError(t, err)

	_, _, _, _, _, err = setup.hub.persistDMMessageWithExpiry(
		mustUUID(t, setup.convID), setup.user1, "", &dmMessageInput{content: "ciphertext", keyVersion: 1, msgType: "user"},
	)
	assert.Error(t, err, "a removed participant must not write after the conversation parent lock")
	var count int
	require.NoError(t, setup.db.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, setup.convID).Scan(&count))
	assert.Zero(t, count)
}

func mustUUID(t *testing.T, value string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(value)
	require.NoError(t, err)
	return id
}
