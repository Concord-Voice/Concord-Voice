package websocket

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type customTextDeliveryResult struct {
	ack presencehistory.DeliveryAck
	err error
}

func newCustomTextDeliveryHub() *Hub {
	return NewHub(nil, nil)
}

func customTextDeliveryClient(hub *Hub, userID uuid.UUID, capacity int) *Client {
	existing := hub.userClients[userID]
	client := connectClient(hub, userID)
	for clientID := range existing {
		hub.userClients[userID][clientID] = true
	}
	client.Send = make(chan []byte, capacity)
	return client
}

func receiveCustomTextDeliveryResult(
	t *testing.T,
	results <-chan customTextDeliveryResult,
) customTextDeliveryResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Custom Status delivery result")
		return customTextDeliveryResult{}
	}
}

func TestDeliverCustomText_ExactAcknowledgesAfterClearThenUpdatePhases(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	operationID := uuid.New()
	clearOnlyID := uuid.New()
	updateOnlyID := uuid.New()
	bothID := uuid.New()
	unrelatedID := uuid.New()
	clearOnly := customTextDeliveryClient(hub, clearOnlyID, 2)
	updateOnly := customTextDeliveryClient(hub, updateOnlyID, 2)
	both := customTextDeliveryClient(hub, bothID, 2)
	unrelated := customTextDeliveryClient(hub, unrelatedID, 2)

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:             presencehistory.DeliveryExactDelta,
		OperationID:      operationID,
		SenderID:         senderID,
		ClearRecipients:  map[uuid.UUID]bool{clearOnlyID: true, bothID: true},
		UpdateRecipients: map[uuid.UUID]bool{updateOnlyID: true, bothID: true},
		Payload: &presencehistory.CustomTextState{
			Text:  "shipping the acknowledged adapter",
			Emoji: "✅",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
	assert.Equal(t, "rich_presence_clear", readClientMsg(t, clearOnly)["type"])
	assert.Equal(t, "rich_presence_update", readClientMsg(t, updateOnly)["type"])
	assert.Equal(t, "rich_presence_clear", readClientMsg(t, both)["type"])
	update := readClientMsg(t, both)
	assert.Equal(t, "rich_presence_update", update["type"])
	data := update["data"].(map[string]interface{})
	assert.Equal(t, senderID.String(), data["user_id"])
	payload := data["payload"].(map[string]interface{})
	assert.Equal(t, "shipping the acknowledged adapter", payload["text"])
	assert.Equal(t, "✅", payload["emoji"])
	assertNoMessage(t, unrelated)
}

func TestDeliverCustomText_ConservativeResetAcknowledgesUnionAndSenderClear(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	operationID := uuid.New()
	priorID := uuid.New()
	attemptedID := uuid.New()
	unrelatedID := uuid.New()
	sender := customTextDeliveryClient(hub, senderID, 2)
	prior := customTextDeliveryClient(hub, priorID, 2)
	attempted := customTextDeliveryClient(hub, attemptedID, 2)
	unrelated := customTextDeliveryClient(hub, unrelatedID, 2)

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:             presencehistory.DeliveryConservativeReset,
		OperationID:      operationID,
		SenderID:         senderID,
		ClearRecipients:  map[uuid.UUID]bool{priorID: true},
		UpdateRecipients: map[uuid.UUID]bool{attemptedID: true},
		Payload:          &presencehistory.CustomTextState{Text: "must never be emitted"},
	})

	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
	for _, client := range []*Client{sender, prior, attempted} {
		message := readClientMsg(t, client)
		assert.Equal(t, "rich_presence_clear", message["type"])
		assert.NotContains(t, message["data"].(map[string]interface{}), "payload")
	}
	assertNoMessage(t, unrelated)
}

func TestDeliverCustomText_OverrideVersionMetadataFollowsDeltaBeforeAcknowledgement(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	sender := customTextDeliveryClient(hub, senderID, 3)
	operationID := uuid.New()
	overrideVersion := 7

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:             presencehistory.DeliveryExactDelta,
		OperationID:      operationID,
		SenderID:         senderID,
		ClearRecipients:  map[uuid.UUID]bool{senderID: true},
		UpdateRecipients: map[uuid.UUID]bool{senderID: true},
		Payload:          &presencehistory.CustomTextState{Text: "current status"},
		OverrideVersion:  &overrideVersion,
	})

	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
	assert.Equal(t, "rich_presence_clear", readClientMsg(t, sender)["type"])
	assert.Equal(t, "rich_presence_update", readClientMsg(t, sender)["type"])
	metadata := readClientMsg(t, sender)
	require.Equal(t, "presence_overrides_updated", metadata["type"])
	data := metadata["data"].(map[string]interface{})
	assert.Equal(t, "custom_text", data["category"])
	assert.Equal(t, float64(overrideVersion), data["version"])
}

func TestDeliverCustomText_ConservativeResetWithoutSafeMapsDisconnectsAllLocal(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	operationID := uuid.New()
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

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:        presencehistory.DeliveryConservativeReset,
		OperationID: operationID,
		SenderID:    uuid.New(),
	})

	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
	for _, client := range clients {
		assert.True(t, disconnected[client.ID], "every locally connected client must be disconnected")
		assert.Empty(t, client.Send, "crash recovery must not emit a sender-revealing clear")
	}
}

func TestDeliverCustomText_MarshalFailureReturnsNoAcknowledgementOrFrame(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 1)
	marshalErr := errors.New("marshal sentinel")
	hub.customTextFrameMarshaler = func(uuid.UUID, *CustomTextPayload) ([]byte, error) {
		return nil, marshalErr
	}

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:            presencehistory.DeliveryExactDelta,
		OperationID:     uuid.New(),
		SenderID:        uuid.New(),
		ClearRecipients: map[uuid.UUID]bool{recipientID: true},
	})

	require.ErrorIs(t, err, marshalErr)
	assert.Equal(t, presencehistory.DeliveryAck{}, ack)
	assert.Empty(t, recipient.Send)
}

func TestDeliverCustomText_RejectsNilOperationID(t *testing.T) {
	hub := newCustomTextDeliveryHub()

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode: presencehistory.DeliveryExactDelta,
	})

	require.ErrorIs(t, err, ErrCustomTextDeliveryPlan)
	assert.Equal(t, presencehistory.DeliveryAck{}, ack)
}

func TestDeliverCustomText_RejectsNilSenderForPreparedRecipients(t *testing.T) {
	tests := []struct {
		name string
		plan presencehistory.DeliveryPlan
	}{
		{
			name: "exact empty recipients",
			plan: presencehistory.DeliveryPlan{
				Mode:        presencehistory.DeliveryExactDelta,
				OperationID: uuid.New(),
			},
		},
		{
			name: "exact clear",
			plan: presencehistory.DeliveryPlan{
				Mode:            presencehistory.DeliveryExactDelta,
				OperationID:     uuid.New(),
				ClearRecipients: map[uuid.UUID]bool{uuid.New(): true},
			},
		},
		{
			name: "conservative clear",
			plan: presencehistory.DeliveryPlan{
				Mode:            presencehistory.DeliveryConservativeReset,
				OperationID:     uuid.New(),
				ClearRecipients: map[uuid.UUID]bool{uuid.New(): true},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hub := newCustomTextDeliveryHub()
			ack, err := hub.DeliverCustomText(context.Background(), test.plan)
			require.ErrorIs(t, err, ErrCustomTextDeliveryPlan)
			assert.Equal(t, presencehistory.DeliveryAck{}, ack)
		})
	}
}

func TestDeliverCustomText_BroadcasterFailureReturnsNoAcknowledgement(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 1)
	broadcastErr := errors.New("broadcaster sentinel")
	hub.customTextDeliveryBroadcaster = func(context.Context, *Client, []byte) error {
		return broadcastErr
	}

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:             presencehistory.DeliveryExactDelta,
		OperationID:      uuid.New(),
		SenderID:         uuid.New(),
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          &presencehistory.CustomTextState{Text: "not delivered"},
	})

	require.ErrorIs(t, err, broadcastErr)
	assert.Equal(t, presencehistory.DeliveryAck{}, ack)
	assert.Empty(t, recipient.Send)
}

func TestDeliverCustomText_FullQueueDisconnectsAndPreservesQueuedFrame(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 1)
	queued := []byte(`{"type":"already_queued"}`)
	recipient.Send <- queued
	disconnected := false
	hub.customTextClientDisconnect = func(client *Client) error {
		assert.Same(t, recipient, client)
		disconnected = true
		return nil
	}
	operationID := uuid.New()

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:            presencehistory.DeliveryExactDelta,
		OperationID:     operationID,
		SenderID:        uuid.New(),
		ClearRecipients: map[uuid.UUID]bool{recipientID: true},
	})

	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
	assert.True(t, disconnected)
	assert.Equal(t, queued, <-recipient.Send)
}

func TestDeliverCustomText_AfterShutdownDoesNotSendToClosedClientQueue(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 1)
	go hub.Run()
	hub.Shutdown()

	operationID := uuid.New()
	var ack presencehistory.DeliveryAck
	var err error
	require.NotPanics(t, func() {
		ack, err = hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
			Mode:            presencehistory.DeliveryExactDelta,
			OperationID:     operationID,
			SenderID:        uuid.New(),
			ClearRecipients: map[uuid.UUID]bool{recipientID: true},
		})
	})
	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
	_, open := <-recipient.Send
	assert.False(t, open)
}

func TestDeliverCustomText_DisconnectedClientIsNotRetargetedByLaterPhase(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	sender := customTextDeliveryClient(hub, senderID, 1)
	disconnectCalls := 0
	hub.customTextClientDisconnect = func(*Client) error {
		disconnectCalls++
		if disconnectCalls > 1 {
			return errors.New("client disconnected twice")
		}
		return nil
	}
	operationID := uuid.New()
	overrideVersion := 9

	ack, err := hub.DeliverCustomText(context.Background(), presencehistory.DeliveryPlan{
		Mode:             presencehistory.DeliveryExactDelta,
		OperationID:      operationID,
		SenderID:         senderID,
		ClearRecipients:  map[uuid.UUID]bool{senderID: true},
		UpdateRecipients: map[uuid.UUID]bool{senderID: true},
		Payload:          &presencehistory.CustomTextState{Text: "new state"},
		OverrideVersion:  &overrideVersion,
	})

	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
	assert.Equal(t, 1, disconnectCalls)
	assert.Equal(t, "rich_presence_clear", readClientMsg(t, sender)["type"])
}

func TestDeliverCustomText_TimeoutBeforeSharedGateCannotEnqueueLater(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	service := presencehistory.NewService(nil, presencehistory.DisclosureState{}, false)
	senderID := uuid.New()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 1)
	gateHeld := make(chan struct{})
	releaseGate := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- service.WithSender(context.Background(), senderID, func() error {
			close(gateHeld)
			<-releaseGate
			return nil
		})
	}()
	awaitCustomTextSignal(t, gateHeld)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	workRan := false
	err := service.WithSender(ctx, senderID, func() error {
		workRan = true
		_, deliverErr := hub.DeliverCustomText(ctx, presencehistory.DeliveryPlan{
			Mode:             presencehistory.DeliveryExactDelta,
			OperationID:      uuid.New(),
			SenderID:         senderID,
			UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
			Payload:          &presencehistory.CustomTextState{Text: "too late"},
		})
		return deliverErr
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, workRan)
	close(releaseGate)
	require.NoError(t, awaitCustomTextSignal(t, holderDone))
	assertNoMessage(t, recipient)
}

func TestDeliverCustomText_CancellationImmediatelyBeforeEnqueueSuppressesFrame(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 1)
	ctx, cancel := context.WithCancel(context.Background())
	hub.customTextDeliveryBeforeEnqueue = cancel

	ack, err := hub.DeliverCustomText(ctx, presencehistory.DeliveryPlan{
		Mode:             presencehistory.DeliveryExactDelta,
		OperationID:      uuid.New(),
		SenderID:         uuid.New(),
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          &presencehistory.CustomTextState{Text: "canceled"},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, presencehistory.DeliveryAck{}, ack)
	assert.Empty(t, recipient.Send)
}

func TestDeliverCustomText_TimedOutCallCannotEnqueueAfterReturn(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 1)
	beforeEnqueue := make(chan struct{})
	releaseEnqueue := make(chan struct{})
	hub.customTextDeliveryBeforeEnqueue = func() {
		close(beforeEnqueue)
		<-releaseEnqueue
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	results := make(chan customTextDeliveryResult, 1)
	go func() {
		ack, err := hub.DeliverCustomText(ctx, presencehistory.DeliveryPlan{
			Mode:             presencehistory.DeliveryExactDelta,
			OperationID:      uuid.New(),
			SenderID:         uuid.New(),
			UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
			Payload:          &presencehistory.CustomTextState{Text: "must stay absent"},
		})
		results <- customTextDeliveryResult{ack: ack, err: err}
	}()
	awaitCustomTextSignal(t, beforeEnqueue)
	<-ctx.Done()
	close(releaseEnqueue)

	result := receiveCustomTextDeliveryResult(t, results)
	require.ErrorIs(t, result.err, context.DeadlineExceeded)
	assert.Equal(t, presencehistory.DeliveryAck{}, result.ack)
	assertNoMessage(t, recipient)
}
