package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliverRichPresence_ClearsBeforeUpdatesOnEveryConnectedDevice(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	clearOnlyID := uuid.New()
	updateOnlyID := uuid.New()
	bothID := uuid.New()
	unrelatedID := uuid.New()

	clearDevices := []*Client{
		activityRichPresenceDeliveryClient(hub, clearOnlyID, 2),
		activityRichPresenceDeliveryClient(hub, clearOnlyID, 2),
	}
	updateDevices := []*Client{
		activityRichPresenceDeliveryClient(hub, updateOnlyID, 2),
		activityRichPresenceDeliveryClient(hub, updateOnlyID, 2),
	}
	bothDevices := []*Client{
		activityRichPresenceDeliveryClient(hub, bothID, 2),
		activityRichPresenceDeliveryClient(hub, bothID, 2),
	}
	unrelated := customTextDeliveryClient(hub, unrelatedID, 1)

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         presence.CategoryServerVoice,
		ClearRecipients:  map[uuid.UUID]bool{clearOnlyID: true, bothID: true, uuid.New(): false},
		UpdateRecipients: map[uuid.UUID]bool{updateOnlyID: true, bothID: true, uuid.New(): false},
		Minimized:        true,
		Payload:          json.RawMessage(`{"channel_id":"11111111-1111-1111-1111-111111111111","server_id":"22222222-2222-2222-2222-222222222222"}`),
		UpdatedAt:        1784088000,
	})

	require.NoError(t, err)
	for _, client := range clearDevices {
		assertRichPresenceClear(t, readClientMsg(t, client), senderID, presence.CategoryServerVoice)
	}
	for _, client := range updateDevices {
		assertServerVoiceUpdate(t, readClientMsg(t, client), senderID)
	}
	for _, client := range bothDevices {
		assertRichPresenceClear(t, readClientMsg(t, client), senderID, presence.CategoryServerVoice)
		assertServerVoiceUpdate(t, readClientMsg(t, client), senderID)
	}
	assertNoMessage(t, unrelated)
}

func TestDeliverRichPresence_RequiresUpgradeActivityCapabilityForLiveFrames(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	recipientID := uuid.New()
	legacy := customTextDeliveryClient(hub, recipientID, 2)
	capable := customTextDeliveryClient(hub, recipientID, 2)
	capable.activityRichPresenceCapable = true

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         presence.CategoryServerVoice,
		ClearRecipients:  map[uuid.UUID]bool{recipientID: true},
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Minimized:        true,
		Payload:          json.RawMessage(`{"channel_id":"11111111-1111-1111-1111-111111111111","server_id":"22222222-2222-2222-2222-222222222222"}`),
		UpdatedAt:        1784088000,
	})

	require.NoError(t, err)
	assertNoMessage(t, legacy)
	assertRichPresenceClear(t, readClientMsg(t, capable), senderID, presence.CategoryServerVoice)
	assertServerVoiceUpdate(t, readClientMsg(t, capable), senderID)
}

func TestDeliverRichPresence_CapableBootstrapOrdersSnapshotBeforeLiveClear(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 2)
	recipient.activityRichPresenceCapable = true
	recipient.beginBootstrap()

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:        uuid.New(),
		Category:        presence.CategoryPrivateCall,
		ClearRecipients: map[uuid.UUID]bool{recipientID: true},
	})
	require.NoError(t, err)
	require.True(t, hub.completeClientBootstrap(
		recipient,
		[]byte(`{"type":"presence_snapshot","data":{"users":[]}}`),
	))

	assert.Equal(t, "presence_snapshot", readClientMsg(t, recipient)["type"])
	assertRichPresenceClear(t, readClientMsg(t, recipient), uuid.Nil, presence.CategoryPrivateCall)
}

func TestDeliverRichPresence_MarshalsEveryFrameBeforeSendingAny(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 2)
	marshalErr := errors.New("marshal sentinel")
	calls := 0
	hub.richPresenceFrameMarshaler = func(
		uuid.UUID,
		presence.Category,
		bool,
		json.RawMessage,
		int64,
	) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, marshalErr
		}
		return []byte(`{"type":"rich_presence_clear"}`), nil
	}

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         uuid.New(),
		Category:         presence.CategoryPrivateCall,
		ClearRecipients:  map[uuid.UUID]bool{recipientID: true},
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          json.RawMessage(`{"call_type":"group","participant_count":3}`),
		UpdatedAt:        1784088000,
	})

	require.ErrorIs(t, err, marshalErr)
	assert.Equal(t, 2, calls)
	assert.Empty(t, recipient.Send)
}

func TestDeliverRichPresence_CancellationImmediatelyBeforeEnqueueSendsNothing(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)
	ctx, cancel := context.WithCancel(context.Background())
	hub.customTextDeliveryBeforeEnqueue = cancel

	err := hub.DeliverRichPresence(ctx, presence.DeliveryPlan{
		SenderID:         uuid.New(),
		Category:         presence.CategoryPrivateCall,
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Minimized:        true,
		Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		UpdatedAt:        1784088000,
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, recipient.Send)
}

func TestDeliverRichPresence_FullQueueDisconnectsWithoutDroppingQueuedFrame(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)
	queued := []byte(`{"type":"already_queued"}`)
	recipient.Send <- queued
	disconnected := false
	hub.customTextClientDisconnect = func(client *Client) error {
		assert.Same(t, recipient, client)
		disconnected = true
		return nil
	}

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:        uuid.New(),
		Category:        presence.CategoryServerVoice,
		ClearRecipients: map[uuid.UUID]bool{recipientID: true},
	})

	require.NoError(t, err)
	assert.True(t, disconnected)
	assert.Equal(t, queued, <-recipient.Send)
}

func TestDeliverRichPresence_DisconnectedClearRecipientIsNotRetargetedByUpdate(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)
	disconnectCalls := 0
	hub.customTextClientDisconnect = func(*Client) error {
		disconnectCalls++
		if disconnectCalls > 1 {
			return errors.New("client disconnected twice")
		}
		return nil
	}

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         uuid.New(),
		Category:         presence.CategoryServerVoice,
		ClearRecipients:  map[uuid.UUID]bool{recipientID: true},
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          json.RawMessage(`{"channel_id":"11111111-1111-1111-1111-111111111111","server_id":"22222222-2222-2222-2222-222222222222"}`),
		UpdatedAt:        1784088000,
	})

	require.NoError(t, err)
	assert.Equal(t, 1, disconnectCalls)
	assertRichPresenceClear(t, readClientMsg(t, recipient), uuid.Nil, "")
}

func TestDisconnectRichPresenceClients_DisconnectsEveryTargetDeviceOnly(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	targetID := uuid.New()
	unrelatedID := uuid.New()
	targets := []*Client{
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
	}
	unrelated := customTextDeliveryClient(hub, unrelatedID, 1)
	disconnected := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		disconnected[client.ID] = true
		return nil
	}

	err := hub.DisconnectRichPresenceClients(
		context.Background(),
		map[uuid.UUID]bool{targetID: true, unrelatedID: false},
	)

	require.NoError(t, err)
	for _, client := range targets {
		assert.True(t, disconnected[client.ID])
	}
	assert.False(t, disconnected[unrelated.ID])
}

func TestDisconnectRichPresenceClients_AttemptsEveryTargetAfterCloseError(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	targetID := uuid.New()
	targets := []*Client{
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
	}
	closeErr := errors.New("close sentinel")
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		if len(attempted) == 1 {
			return closeErr
		}
		return nil
	}

	err := hub.DisconnectRichPresenceClients(
		context.Background(),
		map[uuid.UUID]bool{targetID: true},
	)

	require.ErrorIs(t, err, closeErr)
	for _, client := range targets {
		assert.True(t, attempted[client.ID])
	}
}

func TestDisconnectRichPresenceClients_AttemptsEveryTargetAfterCancellation(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	targetID := uuid.New()
	targets := []*Client{
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		return nil
	}

	err := hub.DisconnectRichPresenceClients(
		ctx,
		map[uuid.UUID]bool{targetID: true},
	)

	require.ErrorIs(t, err, context.Canceled)
	for _, client := range targets {
		assert.True(t, attempted[client.ID])
	}
}

func TestDisconnectAllRichPresenceClients_DisconnectsEveryLocalClient(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	clients := []*Client{
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
	}
	disconnected := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		disconnected[client.ID] = true
		return nil
	}

	require.NoError(t, hub.DisconnectAllRichPresenceClients(context.Background()))
	for _, client := range clients {
		assert.True(t, disconnected[client.ID])
	}
}

func TestDisconnectAllRichPresenceClients_AttemptsEveryClientAfterCloseError(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	clients := []*Client{
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
	}
	closeErr := errors.New("close sentinel")
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		if len(attempted) == 1 {
			return closeErr
		}
		return nil
	}

	err := hub.DisconnectAllRichPresenceClients(context.Background())

	require.ErrorIs(t, err, closeErr)
	for _, client := range clients {
		assert.True(t, attempted[client.ID])
	}
}

func TestDisconnectAllRichPresenceClients_AttemptsEveryClientAfterCancellation(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	clients := []*Client{
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		return nil
	}

	err := hub.DisconnectAllRichPresenceClients(ctx)

	require.ErrorIs(t, err, context.Canceled)
	for _, client := range clients {
		assert.True(t, attempted[client.ID])
	}
}

func TestDeliverRichPresence_PrivateCallFrameUsesExactWireShape(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         presence.CategoryPrivateCall,
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		UpdatedAt:        1784088001,
	})

	require.NoError(t, err)
	message := readClientMsg(t, recipient)
	assert.Equal(t, "rich_presence_update", message["type"])
	data, ok := message["data"].(map[string]interface{})
	require.True(t, ok)
	assert.ElementsMatch(
		t,
		[]string{"user_id", "category", "minimized", "payload", "updated_at"},
		interfaceMapKeys(data),
	)
	assert.Equal(t, senderID.String(), data["user_id"])
	assert.Equal(t, string(presence.CategoryPrivateCall), data["category"])
	assert.Equal(t, false, data["minimized"])
	assert.Equal(t, float64(1784088001), data["updated_at"])
	payload, ok := data["payload"].(map[string]interface{})
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"call_type", "participant_count"}, interfaceMapKeys(payload))
	assert.Equal(t, "dm", payload["call_type"])
	assert.Equal(t, float64(2), payload["participant_count"])
}

func TestDeliverRichPresence_RejectsInvalidPlansBeforeSending(t *testing.T) {
	recipientID := uuid.New()
	tests := []struct {
		name string
		plan presence.DeliveryPlan
	}{
		{
			name: "missing sender",
			plan: presence.DeliveryPlan{
				Category:        presence.CategoryServerVoice,
				ClearRecipients: map[uuid.UUID]bool{recipientID: true},
			},
		},
		{
			name: "unsupported category",
			plan: presence.DeliveryPlan{
				SenderID:        uuid.New(),
				Category:        presence.Category("custom_text"),
				ClearRecipients: map[uuid.UUID]bool{recipientID: true},
			},
		},
		{
			name: "update missing payload",
			plan: presence.DeliveryPlan{
				SenderID:         uuid.New(),
				Category:         presence.CategoryPrivateCall,
				UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
				UpdatedAt:        1784088000,
			},
		},
		{
			name: "update missing timestamp",
			plan: presence.DeliveryPlan{
				SenderID:         uuid.New(),
				Category:         presence.CategoryPrivateCall,
				UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
				Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
			},
		},
		{
			name: "update timestamp exceeds exact JSON ceiling",
			plan: presence.DeliveryPlan{
				SenderID:         uuid.New(),
				Category:         presence.CategoryPrivateCall,
				UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
				Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
				UpdatedAt:        presence.MaxActivityUnixSeconds + 1,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hub := newCustomTextDeliveryHub()
			recipient := customTextDeliveryClient(hub, recipientID, 1)
			err := hub.DeliverRichPresence(context.Background(), test.plan)
			require.ErrorIs(t, err, ErrRichPresenceDeliveryPlan)
			assert.Empty(t, recipient.Send)
		})
	}
}

func assertRichPresenceClear(
	t *testing.T,
	message map[string]interface{},
	senderID uuid.UUID,
	category presence.Category,
) {
	t.Helper()
	assert.Equal(t, "rich_presence_clear", message["type"])
	data, ok := message["data"].(map[string]interface{})
	require.True(t, ok)
	if senderID != uuid.Nil {
		assert.Equal(t, senderID.String(), data["user_id"])
	}
	if category != "" {
		assert.Equal(t, string(category), data["category"])
	}
	assert.ElementsMatch(t, []string{"user_id", "category"}, interfaceMapKeys(data))
}

func assertServerVoiceUpdate(t *testing.T, message map[string]interface{}, senderID uuid.UUID) {
	t.Helper()
	assert.Equal(t, "rich_presence_update", message["type"])
	data, ok := message["data"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, senderID.String(), data["user_id"])
	assert.Equal(t, string(presence.CategoryServerVoice), data["category"])
	assert.Equal(t, true, data["minimized"])
	assert.Equal(t, float64(1784088000), data["updated_at"])
	assert.ElementsMatch(
		t,
		[]string{"user_id", "category", "minimized", "payload", "updated_at"},
		interfaceMapKeys(data),
	)
	payload, ok := data["payload"].(map[string]interface{})
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"channel_id", "server_id"}, interfaceMapKeys(payload))
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", payload["channel_id"])
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", payload["server_id"])
}

func interfaceMapKeys(values map[string]interface{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func activityRichPresenceDeliveryClient(hub *Hub, userID uuid.UUID, capacity int) *Client {
	client := customTextDeliveryClient(hub, userID, capacity)
	client.activityRichPresenceCapable = true
	return client
}
