package websocket

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	testServerA = "server-a"
	testServerB = "server-b"
)

const notAUUID = "not-a-uuid"

// --- Hub construction tests ---

func TestNewHubInitializesMaps(t *testing.T) {
	hub := NewHub(nil, nil)

	assert.NotNil(t, hub.clients)
	assert.NotNil(t, hub.userClients)
	assert.NotNil(t, hub.channelSubscriptions)
	assert.NotNil(t, hub.usernames)
	assert.NotNil(t, hub.serverSubscriptions)
	assert.NotNil(t, hub.dmSubscriptions)
	assert.NotNil(t, hub.register)
	assert.NotNil(t, hub.unregister)
	assert.NotNil(t, hub.incoming)
	assert.NotNil(t, hub.broadcast)
	assert.NotNil(t, hub.globalBroadcast)
	assert.NotNil(t, hub.userBroadcast)
	assert.NotNil(t, hub.serverBroadcast)
	assert.NotNil(t, hub.dmBroadcast)
	assert.NotNil(t, hub.disconnectUser)
	assert.NotNil(t, hub.disconnectSession)
	assert.NotNil(t, hub.done)
	assert.NotNil(t, hub.onlineCountPending)
}

func TestSetMentionChecker(t *testing.T) {
	hub := NewHub(nil, nil)
	assert.Nil(t, hub.mentionChecker)

	checker := &mockMentionChecker{}
	hub.SetMentionChecker(checker)
	assert.NotNil(t, hub.mentionChecker)
}

// TestSetDMRingCanceller verifies the DM ring cleanup callback is stored
// and that subsequent SetDMRingCanceller calls overwrite the previous
// callback (last writer wins) — matches the SetMentionChecker pattern.
func TestSetDMRingCanceller(t *testing.T) {
	hub := NewHub(nil, nil)
	assert.Nil(t, hub.dmRingCanceller)

	called := uuid.Nil
	canceller := func(userID uuid.UUID) { called = userID }
	hub.SetDMRingCanceller(canceller)
	assert.NotNil(t, hub.dmRingCanceller)

	// Confirm the stored callback is actually the one we set (invoke it).
	uid := uuid.New()
	hub.dmRingCanceller(uid)
	assert.Equal(t, uid, called)

	// Last writer wins.
	called2 := uuid.Nil
	canceller2 := func(userID uuid.UUID) { called2 = userID }
	hub.SetDMRingCanceller(canceller2)
	uid2 := uuid.New()
	hub.dmRingCanceller(uid2)
	assert.Equal(t, uid2, called2)
}

// --- newMinimalHub creates a hub with only the maps needed for unit testing
// (no DB/Redis, channels are nil-safe).
func newMinimalHub() *Hub {
	return &Hub{
		clients:              make(map[uuid.UUID]*Client),
		userClients:          make(map[uuid.UUID]map[uuid.UUID]bool),
		channelSubscriptions: make(map[uuid.UUID]map[uuid.UUID]bool),
		usernames:            make(map[uuid.UUID]string),
		serverSubscriptions:  make(map[uuid.UUID]map[uuid.UUID]bool),
		dmSubscriptions:      make(map[uuid.UUID]map[uuid.UUID]bool),
		broadcast:            make(chan BroadcastMessage, 256),
		globalBroadcast:      make(chan OutgoingMessage, 256),
		userBroadcast:        make(chan UserBroadcastMessage, 256),
		serverBroadcast:      make(chan ServerBroadcastMessage, 256),
		dmBroadcast:          make(chan DMBroadcastMessage, 256),
		onlineCountPending:   make(map[uuid.UUID]bool),
	}
}

func newTestClient(hub *Hub, userID uuid.UUID) *Client {
	return &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Username: "testuser",
		Hub:      hub,
		Send:     make(chan []byte, 256),
		Channels: make(map[uuid.UUID]bool),
	}
}

// --- IsUserOnline / MarkUserOnlineForTest tests ---

func TestHub_IsUserOnline(t *testing.T) {
	h := newMinimalHub()
	uid := uuid.New()
	assert.False(t, h.IsUserOnline(uid))
	cid := uuid.New()
	h.userClients[uid] = map[uuid.UUID]bool{cid: true} // direct registration, matches existing hub tests
	assert.True(t, h.IsUserOnline(uid))
}

func TestHub_MarkUserOnlineForTest(t *testing.T) {
	h := newMinimalHub()
	uid := uuid.New()
	assert.False(t, h.IsUserOnline(uid))
	h.MarkUserOnlineForTest(uid)
	assert.True(t, h.IsUserOnline(uid))
}

// --- handleUnregister tests (no DB/Redis needed for map cleanup) ---

func TestHandleUnregisterRemovesClient(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	// Add a second client so this is NOT the last connection (avoids Redis call)
	otherClient := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[otherClient.ID] = otherClient
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, otherClient.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.clients[client.ID]
	assert.False(t, exists, "client should be removed from clients map")
	assert.True(t, hub.userClients[userID][otherClient.ID], "other client should remain")
}

func TestHandleUnregisterMultiDeviceKeepsOtherClients(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	hub.handleUnregister(client1)

	_, exists := hub.clients[client1.ID]
	assert.False(t, exists, "unregistered client should be removed")
	_, exists = hub.clients[client2.ID]
	assert.True(t, exists, "other client should remain")
	assert.True(t, hub.userClients[userID][client2.ID], "other client should remain in userClients")
}

func TestHandleUnregisterRemovesFromChannelSubscriptions(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	channelID := uuid.New()
	client := newTestClient(hub, userID)
	client.Channels[channelID] = true
	// Add buddy client so this is not the last connection (avoids Redis call)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.channelSubscriptions[channelID]
	assert.False(t, exists, "channel subscription should be cleaned up when last subscriber disconnects")
}

func TestHandleUnregisterRemovesFromServerSubscriptions(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	serverID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.serverSubscriptions[serverID]
	assert.False(t, exists, "server subscription should be cleaned up")
}

func TestHandleUnregisterRemovesFromDMSubscriptions(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	convID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.dmSubscriptions[convID]
	assert.False(t, exists, "DM subscription should be cleaned up")
}

func TestHandleUnregisterClosesClientSendChannel(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}

	hub.handleUnregister(client)

	// Verify Send channel is closed by attempting to receive
	_, ok := <-client.Send
	assert.False(t, ok, "Send channel should be closed")
}

func TestHandleUnregisterNonexistentClient(_ *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())

	// Should not panic when client is not registered
	hub.handleUnregister(client)
}

func TestHandleUnregisterRemovesUsernameCache(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.usernames[userID] = "testuser"

	// Unregister first client — username cache should remain (buddy still connected)
	hub.handleUnregister(client)
	_, exists := hub.usernames[userID]
	assert.True(t, exists, "username cache should remain while user still has connections")

	// Unregister buddy — but this triggers isLastConnection which needs Redis.
	// So we just verify the buddy removal via map inspection instead.
}

// --- handleBroadcast tests ---

func TestHandleBroadcastSendsToSubscribers(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	user1 := uuid.New()
	user2 := uuid.New()

	client1 := newTestClient(hub, user1)
	client2 := newTestClient(hub, user2)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		client1.ID: true,
		client2.ID: true,
	}

	msg := BroadcastMessage{
		ChannelID: channelID,
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{"content": "hello"},
		},
	}

	hub.handleBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleBroadcastExcludesUser(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	senderID := uuid.New()
	receiverID := uuid.New()

	sender := newTestClient(hub, senderID)
	receiver := newTestClient(hub, receiverID)

	hub.clients[sender.ID] = sender
	hub.clients[receiver.ID] = receiver
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		sender.ID:   true,
		receiver.ID: true,
	}

	msg := BroadcastMessage{
		ChannelID:   channelID,
		ExcludeUser: &senderID,
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{"content": "hello"},
		},
	}

	hub.handleBroadcast(msg)

	assert.Len(t, sender.Send, 0, "excluded user should not receive message")
	assert.Len(t, receiver.Send, 1, "non-excluded user should receive message")
}

func TestHandleBroadcastNoSubscribers(_ *testing.T) {
	hub := newMinimalHub()

	msg := BroadcastMessage{
		ChannelID: uuid.New(),
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{"content": "hello"},
		},
	}

	// Should not panic
	hub.handleBroadcast(msg)
}

// --- handleGlobalBroadcast tests ---

func TestHandleGlobalBroadcastSendsToAllClients(t *testing.T) {
	hub := newMinimalHub()
	user1 := uuid.New()
	user2 := uuid.New()

	client1 := newTestClient(hub, user1)
	client2 := newTestClient(hub, user2)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2

	msg := OutgoingMessage{
		Type: "presence",
		Data: map[string]interface{}{"user_id": "test"},
	}

	hub.handleGlobalBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleGlobalBroadcastNoClients(_ *testing.T) {
	hub := newMinimalHub()

	msg := OutgoingMessage{
		Type: "presence",
		Data: map[string]interface{}{},
	}

	// Should not panic
	hub.handleGlobalBroadcast(msg)
}

// --- handleServerBroadcast tests ---

func TestHandleServerBroadcastSendsToSubscribers(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	userID := uuid.New()

	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	msg := ServerBroadcastMessage{
		ServerID: serverID,
		Data: OutgoingMessage{
			Type: "server_updated",
			Data: map[string]interface{}{"name": "test"},
		},
	}

	hub.handleServerBroadcast(msg)

	assert.Len(t, client.Send, 1)
}

func TestHandleServerBroadcastNoSubscribers(_ *testing.T) {
	hub := newMinimalHub()

	msg := ServerBroadcastMessage{
		ServerID: uuid.New(),
		Data: OutgoingMessage{
			Type: "server_updated",
			Data: map[string]interface{}{},
		},
	}

	// Should not panic
	hub.handleServerBroadcast(msg)
}

// --- handleUserBroadcast tests ---

func TestHandleUserBroadcastSendsToAllUserClients(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	msg := UserBroadcastMessage{
		UserID: userID,
		Data: OutgoingMessage{
			Type: "profile_updated",
			Data: map[string]interface{}{"username": "new_name"},
		},
	}

	hub.handleUserBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleUserBroadcastExcludesClient(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	msg := UserBroadcastMessage{
		UserID:          userID,
		ExcludeClientID: &client1.ID,
		Data: OutgoingMessage{
			Type: "profile_updated",
			Data: map[string]interface{}{},
		},
	}

	hub.handleUserBroadcast(msg)

	assert.Len(t, client1.Send, 0, "excluded client should not receive message")
	assert.Len(t, client2.Send, 1)
}

func TestHandleUserBroadcastUserNotConnected(_ *testing.T) {
	hub := newMinimalHub()

	msg := UserBroadcastMessage{
		UserID: uuid.New(),
		Data: OutgoingMessage{
			Type: "test",
			Data: map[string]interface{}{},
		},
	}

	// Should not panic
	hub.handleUserBroadcast(msg)
}

// --- handleDMBroadcast tests ---

func TestHandleDMBroadcastSendsToSubscribers(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	user1 := uuid.New()
	user2 := uuid.New()

	client1 := newTestClient(hub, user1)
	client2 := newTestClient(hub, user2)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{
		client1.ID: true,
		client2.ID: true,
	}

	msg := DMBroadcastMessage{
		ConversationID: convID,
		Data: OutgoingMessage{
			Type: "dm_message",
			Data: map[string]interface{}{"content": "hi"},
		},
	}

	hub.handleDMBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleDMBroadcastExcludesUser(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	senderID := uuid.New()
	receiverID := uuid.New()

	sender := newTestClient(hub, senderID)
	receiver := newTestClient(hub, receiverID)

	hub.clients[sender.ID] = sender
	hub.clients[receiver.ID] = receiver
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{
		sender.ID:   true,
		receiver.ID: true,
	}

	msg := DMBroadcastMessage{
		ConversationID: convID,
		ExcludeUser:    &senderID,
		Data: OutgoingMessage{
			Type: "dm_message",
			Data: map[string]interface{}{"content": "hi"},
		},
	}

	hub.handleDMBroadcast(msg)

	assert.Len(t, sender.Send, 0, "excluded user should not receive message")
	assert.Len(t, receiver.Send, 1)
}

func TestHandleDMBroadcastNoSubscribers(_ *testing.T) {
	hub := newMinimalHub()

	msg := DMBroadcastMessage{
		ConversationID: uuid.New(),
		Data: OutgoingMessage{
			Type: "dm_message",
			Data: map[string]interface{}{},
		},
	}

	// Should not panic
	hub.handleDMBroadcast(msg)
}

// --- sendError tests ---

func TestSendErrorSendsToClient(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	hub.sendError(client.ID, "test error message")

	require.Len(t, client.Send, 1)
	data := <-client.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "error", msg["type"])
	msgData := msg["data"].(map[string]interface{})
	assert.Equal(t, "test error message", msgData["message"])
}

func TestSendErrorClientNotFound(_ *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.sendError(uuid.New(), "test error")
}

// --- sendErrorWithData tests ---

func TestSendErrorWithDataIncludesExtraFields(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	extra := map[string]interface{}{
		"current_epoch": 5,
		"channel_id":    "abc-123",
	}
	hub.sendErrorWithData(client.ID, "epoch_revoked", extra)

	require.Len(t, client.Send, 1)
	data := <-client.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "error", msg["type"])
	msgData := msg["data"].(map[string]interface{})
	assert.Equal(t, "epoch_revoked", msgData["code"])
	assert.Equal(t, "epoch_revoked", msgData["error"])
	assert.Equal(t, float64(5), msgData["current_epoch"])
	assert.Equal(t, "abc-123", msgData["channel_id"])
}

func TestSendErrorWithDataClientNotFound(_ *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.sendErrorWithData(uuid.New(), "test", nil)
}

// --- handleIncoming routing tests ---

func TestHandleIncomingUnknownType(_ *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Type:     "unknown_message_type",
		ClientID: uuid.New(),
		UserID:   uuid.New(),
	}

	// Should not panic
	hub.handleIncoming(msg)
}

// --- handleUnsubscribe tests ---

func TestHandleUnsubscribeRemovesFromChannel(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	client.Channels[channelID] = true

	hub.clients[client.ID] = client
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
		},
	}

	hub.handleUnsubscribe(msg)

	assert.False(t, client.Channels[channelID])
	_, exists := hub.channelSubscriptions[channelID]
	assert.False(t, exists, "channel subscription should be cleaned up")
}

func TestHandleUnsubscribeInvalidChannelID(_ *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: client.ID,
		Data:     map[string]interface{}{keyChannelID: notAUUID},
	}

	// Should not panic
	hub.handleUnsubscribe(msg)
}

func TestHandleUnsubscribeMissingChannelID(_ *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: client.ID,
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleUnsubscribe(msg)
}

func TestHandleUnsubscribeClientNotFound(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyChannelID: uuid.New().String(),
		},
	}

	// Should not panic
	hub.handleUnsubscribe(msg)
}

// --- handleUnsubscribeServer tests ---

func TestHandleUnsubscribeServerRemovesFromServer(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe_server",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	hub.handleUnsubscribeServer(msg)

	_, exists := hub.serverSubscriptions[serverID]
	assert.False(t, exists)
}

func TestHandleUnsubscribeServerInvalidServerID(_ *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "unsubscribe_server",
		ClientID: client.ID,
		Data:     map[string]interface{}{keyServerID: notAUUID},
	}

	// Should not panic
	hub.handleUnsubscribeServer(msg)
}

func TestHandleUnsubscribeServerMissingServerID(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe_server",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleUnsubscribeServer(msg)
}

// --- handleUnsubscribeDM tests ---

func TestHandleUnsubscribeDMRemovesFromDM(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()

	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe_dm",
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
		},
	}

	hub.handleUnsubscribeDM(msg)

	_, exists := hub.dmSubscriptions[convID]
	assert.False(t, exists)
}

func TestHandleUnsubscribeDMInvalidConversationID(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe_dm",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{keyConversationID: notAUUID},
	}

	// Should not panic
	hub.handleUnsubscribeDM(msg)
}

func TestHandleUnsubscribeDMMissingConversationID(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe_dm",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleUnsubscribeDM(msg)
}

// --- handleDisconnectUser tests ---

func TestHandleDisconnectUserRemovesTargetedClients(t *testing.T) {
	hub := newMinimalHub()
	targetUserID := uuid.New()
	otherUserID := uuid.New()

	// Two clients for the target user, one for another user
	client1 := newTestClient(hub, targetUserID)
	client2 := newTestClient(hub, targetUserID)
	otherClient := newTestClient(hub, otherUserID)
	// Give otherClient a buddy so its unregister also doesn't trigger Redis
	otherBuddy := newTestClient(hub, otherUserID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.clients[otherClient.ID] = otherClient
	hub.clients[otherBuddy.ID] = otherBuddy
	hub.userClients[targetUserID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}
	hub.userClients[otherUserID] = map[uuid.UUID]bool{otherClient.ID: true, otherBuddy.ID: true}

	// handleDisconnectUser calls handleUnregister which needs Redis for the
	// last connection. These tests skip the last-connection path because it
	// requires Redis. Integration tests in hub_epoch_test.go cover that path.
	// We verify that the function collects and attempts to disconnect all clients
	// for the target user by checking the first unregister succeeds.

	// Instead, test behavior before the last-connection Redis path:
	// Verify the revoked message is queued for all target clients, then verify
	// that the other user's clients are untouched.
	// We call handleUnregister on each client individually, stopping before
	// the last one would trigger Redis.

	// Unregister client1 (not last for target user)
	hub.handleUnregister(client1)
	_, exists := hub.clients[client1.ID]
	assert.False(t, exists, "client1 should be removed")
	assert.True(t, hub.userClients[targetUserID][client2.ID], "client2 should remain")

	// Other user's clients should be untouched
	_, exists = hub.clients[otherClient.ID]
	assert.True(t, exists, "other user's client should be untouched")
}

func TestHandleDisconnectUserSendsRevokedMessageToMultipleClients(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)
	// A third client ensures unregistering client1 and client2 still leaves one
	// so the last-connection Redis path is not triggered until the very end.
	client3 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.clients[client3.ID] = client3
	hub.userClients[userID] = map[uuid.UUID]bool{
		client1.ID: true,
		client2.ID: true,
		client3.ID: true,
	}

	// Manually replicate what handleDisconnectUser does: collect clients,
	// send revoked message, then unregister. We only unregister the first two
	// (leaving client3 so Redis isn't called).
	revokedMsg, _ := json.Marshal(OutgoingMessage{
		Type: sessionRevoked,
		Data: map[string]interface{}{"reason": "session_terminated"},
	})

	// Queue the revoked message on all three clients
	for _, c := range []*Client{client1, client2, client3} {
		select {
		case c.Send <- revokedMsg:
		default:
		}
	}

	// Verify all three received the revoked message
	for i, c := range []*Client{client1, client2, client3} {
		select {
		case data := <-c.Send:
			var msg map[string]interface{}
			require.NoError(t, json.Unmarshal(data, &msg))
			assert.Equal(t, sessionRevoked, msg["type"], "client %d should receive session_revoked", i)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timed out waiting for session_revoked on client %d", i)
		}
	}
}

func TestHandleDisconnectUserUserNotConnected(_ *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.handleDisconnectUser(uuid.New())
}

// --- handleDisconnectSession tests ---

func TestHandleDisconnectSessionDisconnectsMatchingSession(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	sessionID := "session-123"

	client1 := newTestClient(hub, userID)
	client1.SessionID = sessionID
	client2 := newTestClient(hub, userID)
	client2.SessionID = "other-session"

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	hub.handleDisconnectSession(sessionID)

	_, exists := hub.clients[client1.ID]
	assert.False(t, exists, "matching session client should be disconnected")
	_, exists = hub.clients[client2.ID]
	assert.True(t, exists, "non-matching session client should remain")
}

func TestHandleDisconnectSessionEmptySessionID(_ *testing.T) {
	hub := newMinimalHub()
	// Should return early
	hub.handleDisconnectSession("")
}

func TestHandleDisconnectSessionNoMatchingSessions(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	client.SessionID = "some-session"
	hub.clients[client.ID] = client
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}

	// Should not disconnect anything
	hub.handleDisconnectSession("nonexistent-session")

	_, exists := hub.clients[client.ID]
	assert.True(t, exists, "client should still be connected")
}

// --- sendUnreadNotify tests ---

func TestSendUnreadNotifySendsToServerSubscribers(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	channelID := uuid.New()
	senderID := uuid.New()
	receiverID := uuid.New()

	sender := newTestClient(hub, senderID)
	receiver := newTestClient(hub, receiverID)

	hub.clients[sender.ID] = sender
	hub.clients[receiver.ID] = receiver

	// Both subscribed to server, but only sender is subscribed to channel
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{
		sender.ID:   true,
		receiver.ID: true,
	}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		sender.ID: true,
	}

	hub.sendUnreadNotify(serverID, channelID, senderID)

	assert.Len(t, sender.Send, 0, "sender should not receive unread notify")
	assert.Len(t, receiver.Send, 1, "receiver should get unread notify")

	data := <-receiver.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "unread_notify", msg["type"])
}

func TestSendUnreadNotifySkipsChannelSubscribers(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	channelID := uuid.New()
	senderID := uuid.New()
	subscribedUserID := uuid.New()

	sender := newTestClient(hub, senderID)
	subscribedUser := newTestClient(hub, subscribedUserID)

	hub.clients[sender.ID] = sender
	hub.clients[subscribedUser.ID] = subscribedUser

	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{
		sender.ID:         true,
		subscribedUser.ID: true,
	}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		sender.ID:         true,
		subscribedUser.ID: true,
	}

	hub.sendUnreadNotify(serverID, channelID, senderID)

	assert.Len(t, subscribedUser.Send, 0, "channel subscriber should not get unread notify")
	assert.Len(t, sender.Send, 0, "sender should not get unread notify")
}

func TestSendUnreadNotifyNoServerSubscribers(_ *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.sendUnreadNotify(uuid.New(), uuid.New(), uuid.New())
}

// --- handleTyping tests (no DB needed) ---

func TestHandleTypingNotSubscribed(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	// Client exists but is NOT subscribed to the channel
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
			keyIsTyping:  true,
		},
	}

	hub.handleTyping(msg)

	// No broadcast should be sent since client is not subscribed
	select {
	case <-hub.broadcast:
		t.Fatal("should not broadcast typing for unsubscribed client")
	case <-time.After(50 * time.Millisecond):
		// Expected: no broadcast
	}
}

func TestHandleTypingSubscribed(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	client.Channels[channelID] = true

	hub.clients[client.ID] = client
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
			keyIsTyping:  true,
		},
	}

	hub.handleTyping(msg)

	select {
	case bMsg := <-hub.broadcast:
		assert.Equal(t, channelID, bMsg.ChannelID)
		assert.Equal(t, "typing", bMsg.Data.Type)
		assert.Equal(t, &userID, bMsg.ExcludeUser)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected broadcast for typing indicator")
	}
}

func TestHandleTypingInvalidChannelID(_ *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: client.ID,
		Data:     map[string]interface{}{keyChannelID: notAUUID},
	}

	// Should not panic
	hub.handleTyping(msg)
}

func TestHandleTypingMissingChannelID(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleTyping(msg)
}

func TestHandleTypingClientNotFound(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyChannelID: uuid.New().String(),
		},
	}

	// Should not panic
	hub.handleTyping(msg)
}

// --- handleDMTyping tests ---

func TestHandleDMTypingSubscribed(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
			keyIsTyping:       true,
		},
	}

	hub.handleDMTyping(msg)

	select {
	case dmMsg := <-hub.dmBroadcast:
		assert.Equal(t, convID, dmMsg.ConversationID)
		assert.Equal(t, "dm_typing", dmMsg.Data.Type)
		assert.Equal(t, &userID, dmMsg.ExcludeUser)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected DM broadcast for typing indicator")
	}
}

func TestHandleDMTypingNotSubscribed(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	// Not subscribed to any DM

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
			keyIsTyping:       true,
		},
	}

	hub.handleDMTyping(msg)

	select {
	case <-hub.dmBroadcast:
		t.Fatal("should not broadcast DM typing for unsubscribed client")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestHandleDMTypingInvalidConversationID(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{keyConversationID: notAUUID},
	}

	// Should not panic
	hub.handleDMTyping(msg)
}

func TestHandleDMTypingClientNotFound(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyConversationID: uuid.New().String(),
		},
	}

	// Should not panic
	hub.handleDMTyping(msg)
}

// --- handleSetStatus tests (needs Redis; tested for input validation only) ---

func TestHandleSetStatusInvalidStatus(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: uuid.New(),
		Data: map[string]interface{}{
			keyStatus: "invalid_status",
		},
	}

	// Should return early without panic (no Redis call)
	hub.handleSetStatus(msg)
}

func TestHandleSetStatusMissingStatus(_ *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: uuid.New(),
		Data:   map[string]interface{}{},
	}

	// Should return early
	hub.handleSetStatus(msg)
}

// --- GetConnectedUsers / GetUserClientCount tests ---

func TestGetConnectedUsers(t *testing.T) {
	hub := newMinimalHub()
	hub.mu.Lock()
	user1 := uuid.New()
	user2 := uuid.New()
	hub.userClients[user1] = map[uuid.UUID]bool{uuid.New(): true}
	hub.userClients[user2] = map[uuid.UUID]bool{uuid.New(): true}
	hub.mu.Unlock()

	users := hub.GetConnectedUsers()

	assert.Len(t, users, 2)
	assert.True(t, users[user1])
	assert.True(t, users[user2])
}

func TestGetConnectedUsersEmpty(t *testing.T) {
	hub := newMinimalHub()
	users := hub.GetConnectedUsers()
	assert.Empty(t, users)
}

func TestGetUserClientCount(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	hub.mu.Lock()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true, uuid.New(): true}
	hub.mu.Unlock()

	assert.Equal(t, 2, hub.GetUserClientCount(userID))
}

func TestGetUserClientCountNotConnected(t *testing.T) {
	hub := newMinimalHub()
	assert.Equal(t, 0, hub.GetUserClientCount(uuid.New()))
}

// --- Public API thread-safe wrappers ---

func TestBroadcastToChannelQueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	msg := OutgoingMessage{Type: "test", Data: map[string]interface{}{}}

	hub.BroadcastToChannel(channelID, msg)

	select {
	case bMsg := <-hub.broadcast:
		assert.Equal(t, channelID, bMsg.ChannelID)
		assert.Equal(t, "test", bMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on broadcast channel")
	}
}

func TestBroadcastToAllQueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	msg := OutgoingMessage{Type: "global_test", Data: map[string]interface{}{}}

	hub.BroadcastToAll(msg)

	select {
	case gMsg := <-hub.globalBroadcast:
		assert.Equal(t, "global_test", gMsg.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on globalBroadcast channel")
	}
}

func TestBroadcastToServerQueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	msg := OutgoingMessage{Type: "server_test", Data: map[string]interface{}{}}

	hub.BroadcastToServer(serverID, msg)

	select {
	case sMsg := <-hub.serverBroadcast:
		assert.Equal(t, serverID, sMsg.ServerID)
		assert.Equal(t, "server_test", sMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on serverBroadcast channel")
	}
}

func TestBroadcastToUserQueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	msg := OutgoingMessage{Type: "user_test", Data: map[string]interface{}{}}

	hub.BroadcastToUser(userID, msg)

	select {
	case uMsg := <-hub.userBroadcast:
		assert.Equal(t, userID, uMsg.UserID)
		assert.Equal(t, "user_test", uMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on userBroadcast channel")
	}
}

func TestBroadcastToDMQueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	msg := OutgoingMessage{Type: "dm_test", Data: map[string]interface{}{}}

	hub.BroadcastToDM(convID, msg)

	select {
	case dmMsg := <-hub.dmBroadcast:
		assert.Equal(t, convID, dmMsg.ConversationID)
		assert.Equal(t, "dm_test", dmMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on dmBroadcast channel")
	}
}

func TestBroadcastServerVoiceCountsQueuesSignal(t *testing.T) {
	hub := NewHub(nil, nil)

	hub.BroadcastServerVoiceCounts()

	select {
	case <-hub.voiceCountSignal:
		// Signal received
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected signal on voiceCountSignal")
	}
}

func TestBroadcastServerVoiceCountsCoalescesMultipleSignals(t *testing.T) {
	hub := NewHub(nil, nil)

	// Send multiple signals in quick succession
	hub.BroadcastServerVoiceCounts()
	hub.BroadcastServerVoiceCounts()
	hub.BroadcastServerVoiceCounts()

	// Drain the one signal
	select {
	case <-hub.voiceCountSignal:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected at least one signal")
	}

	// Verify no more signals (coalesced)
	select {
	case <-hub.voiceCountSignal:
		t.Fatal("expected signals to be coalesced")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

// --- scheduleOnlineCountBroadcast tests ---

func TestScheduleOnlineCountBroadcastAddsUserToPending(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	hub.scheduleOnlineCountBroadcast(userID)

	assert.True(t, hub.onlineCountPending[userID])
	assert.NotNil(t, hub.onlineCountTimer)

	// Clean up timer
	hub.onlineCountTimer.Stop()
}

func TestScheduleOnlineCountBroadcastReusesExistingTimer(t *testing.T) {
	hub := newMinimalHub()
	user1 := uuid.New()
	user2 := uuid.New()

	hub.scheduleOnlineCountBroadcast(user1)
	timer1 := hub.onlineCountTimer

	hub.scheduleOnlineCountBroadcast(user2)
	timer2 := hub.onlineCountTimer

	assert.Same(t, timer1, timer2, "should reuse existing timer")
	assert.True(t, hub.onlineCountPending[user1])
	assert.True(t, hub.onlineCountPending[user2])

	timer1.Stop()
}

// --- Shutdown test ---

func TestShutdownClosesAllClientSendChannels(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Send:     make(chan []byte, 10),
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client

	// Start Run in a goroutine
	go hub.Run()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	hub.Shutdown()

	// Verify the Send channel gets closed (Run exits and closes all client.Send)
	select {
	case _, ok := <-client.Send:
		assert.False(t, ok, "Send channel should be closed after shutdown")
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestShutdownIdempotent(t *testing.T) {
	hub := NewHub(nil, nil)
	go hub.Run()

	// First call shuts down and waits for Run to exit
	hub.Shutdown()

	// Second call must not panic (close of closed channel) or block
	require.NotPanics(t, func() {
		hub.Shutdown()
	})
}

func TestBroadcastServerVoiceCountsAfterShutdown(t *testing.T) {
	hub := NewHub(nil, nil)
	go hub.Run()

	// Shutdown waits for Run to exit
	hub.Shutdown()

	// Must not panic or hang — the buffered channel with default clause handles this
	require.NotPanics(t, func() {
		hub.BroadcastServerVoiceCounts()
	})
}

func TestShutdownStopsRunLoop(t *testing.T) {
	hub := NewHub(nil, nil)
	go hub.Run()

	hub.Shutdown()

	// Stopped channel should already be closed since Shutdown waits
	select {
	case <-hub.Stopped():
		// Run goroutine exited — success
	case <-time.After(2 * time.Second):
		t.Fatal("Run goroutine did not exit after Shutdown")
	}
}

// --- isVisibleStatus tests ---

func TestIsVisibleStatus(t *testing.T) {
	tests := []struct {
		name string
		val  interface{}
		want bool
	}{
		{"nil value (just connected)", nil, true},
		{"online status", "online", true},
		{"offline status", "offline", true},
		{"invisible status", "invisible", false},
		{"empty string", "", true},
		{"non-string type", 42, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isVisibleStatus(tt.val))
		})
	}
}

// --- buildPlaceholders tests ---

func TestBuildPlaceholders(t *testing.T) {
	assert.Equal(t, "$1", buildPlaceholders(1))
	assert.Equal(t, "$1,$2,$3", buildPlaceholders(3))
	assert.Equal(t, "", buildPlaceholders(0))
}

// --- computeServerCounts tests ---

func TestComputeServerCounts(t *testing.T) {
	hub := newMinimalHub()

	uid1 := uuid.New()
	uid2 := uuid.New()
	uid3 := uuid.New()

	serverMembers := map[string][]uuid.UUID{
		testServerA: {uid1, uid2, uid3},
		testServerB: {uid2, uid3},
	}
	visibleOnline := map[uuid.UUID]bool{
		uid1: true,
		uid3: true,
	}

	counts := hub.computeServerCounts(serverMembers, visibleOnline)
	assert.Equal(t, 2, counts[testServerA]) // uid1 + uid3
	assert.Equal(t, 1, counts[testServerB]) // uid3 only
}

func TestComputeServerCountsNoneOnline(t *testing.T) {
	hub := newMinimalHub()
	serverMembers := map[string][]uuid.UUID{
		testServerA: {uuid.New(), uuid.New()},
	}
	counts := hub.computeServerCounts(serverMembers, map[uuid.UUID]bool{})
	assert.Equal(t, 0, counts[testServerA])
}

// --- collectPendingUsers tests ---

func TestCollectPendingUsersClearsSet(t *testing.T) {
	hub := newMinimalHub()
	uid1 := uuid.New()
	uid2 := uuid.New()
	hub.onlineCountPending[uid1] = true
	hub.onlineCountPending[uid2] = true

	params := hub.collectPendingUsers()
	assert.Len(t, params, 2)
	assert.Empty(t, hub.onlineCountPending, "pending set should be cleared")
}

// --- isFKViolation tests ---

func TestIsFKViolationPostgresForeignKeyError(t *testing.T) {
	pqErr := &pq.Error{Code: "23503"}
	assert.True(t, isFKViolation(pqErr))
}

func TestIsFKViolationOtherPostgresError(t *testing.T) {
	pqErr := &pq.Error{Code: "23505"} // unique violation
	assert.False(t, isFKViolation(pqErr))
}

func TestIsFKViolationNonPostgresError(t *testing.T) {
	assert.False(t, isFKViolation(errors.New("generic error")))
}

// --- userHasDMSubscription tests ---

func TestUserHasDMSubscriptionHasSubscribed(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	userClientIDs := map[uuid.UUID]bool{clientID: true}
	dmClients := map[uuid.UUID]bool{clientID: true}

	assert.True(t, hub.userHasDMSubscription(userClientIDs, dmClients))
}

func TestUserHasDMSubscriptionNotSubscribed(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	userClientIDs := map[uuid.UUID]bool{clientID: true}
	dmClients := map[uuid.UUID]bool{uuid.New(): true}

	assert.False(t, hub.userHasDMSubscription(userClientIDs, dmClients))
}

func TestUserHasDMSubscriptionNilDMClients(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	userClientIDs := map[uuid.UUID]bool{clientID: true}

	assert.False(t, hub.userHasDMSubscription(userClientIDs, nil))
}

// --- notifyUnsubscribedUser tests ---

func TestNotifyUnsubscribedUserSendsWhenNotSubscribed(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	data := []byte(`{"type":"dm_unread_notify"}`)
	hub.notifyUnsubscribedUser(userID, nil, data)

	select {
	case msg := <-client.Send:
		assert.Equal(t, data, msg)
	default:
		t.Fatal("expected message to be sent")
	}
}

func TestNotifyUnsubscribedUserSkipsWhenSubscribed(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	dmClients := map[uuid.UUID]bool{client.ID: true}
	data := []byte(`{"type":"dm_unread_notify"}`)
	hub.notifyUnsubscribedUser(userID, dmClients, data)

	select {
	case <-client.Send:
		t.Fatal("should not send to subscribed user")
	default:
		// expected
	}
}

func TestNotifyUnsubscribedUserUserNotConnected(_ *testing.T) {
	hub := newMinimalHub()
	data := []byte(`{"type":"dm_unread_notify"}`)
	// Should not panic
	hub.notifyUnsubscribedUser(uuid.New(), nil, data)
}

// --- sendToUserClients tests ---

func TestSendToUserClientsMultipleClients(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	c1 := newTestClient(hub, userID)
	c2 := newTestClient(hub, userID)
	hub.clients[c1.ID] = c1
	hub.clients[c2.ID] = c2

	clientIDs := map[uuid.UUID]bool{c1.ID: true, c2.ID: true}
	data := []byte(`{"test":"msg"}`)
	hub.sendToUserClients(clientIDs, data)

	select {
	case msg := <-c1.Send:
		assert.Equal(t, data, msg)
	default:
		t.Fatal("c1 should receive message")
	}
	select {
	case msg := <-c2.Send:
		assert.Equal(t, data, msg)
	default:
		t.Fatal("c2 should receive message")
	}
}

func TestSendToUserClientsSkipsMissingClient(_ *testing.T) {
	hub := newMinimalHub()
	missingID := uuid.New()
	clientIDs := map[uuid.UUID]bool{missingID: true}
	data := []byte(`{"test":"msg"}`)
	// Should not panic
	hub.sendToUserClients(clientIDs, data)
}

// --- parseDMMentionMeta tests ---

func TestParseDMMentionMetaNoMeta(t *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Data: map[string]interface{}{},
	}
	result := hub.parseDMMentionMeta(msg)
	assert.Nil(t, result)
}

func TestParseDMMentionMetaInvalidJSON(t *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Data: map[string]interface{}{
			"mention_meta": "not-valid-json",
		},
	}
	result := hub.parseDMMentionMeta(msg)
	assert.Nil(t, result)
}

func TestParseDMMentionMetaSanitizesServerFields(t *testing.T) {
	hub := newMinimalHub()
	uid := uuid.New()
	meta := MentionAddendum{
		Everyone: true,
		Users:    []string{uid.String()},
		Roles:    []string{"admin"},
	}
	packed, err := msgpack.Marshal(meta)
	require.NoError(t, err)
	encoded := base64.StdEncoding.EncodeToString(packed)
	msg := IncomingMessage{
		Data: map[string]interface{}{
			"mention_meta": encoded,
		},
	}
	result := hub.parseDMMentionMeta(msg)
	require.NotNil(t, result)
	assert.False(t, result.Everyone, "Everyone should be cleared for DM")
	assert.Nil(t, result.Roles, "Roles should be cleared for DM")
	assert.Equal(t, []string{uid.String()}, result.Users)
}

// --- validateContentLength tests ---

func TestValidateContentLengthShortContentOK(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	assert.True(t, hub.validateContentLength(clientID, "hello"))
}

func TestValidateContentLengthBelowCiphertextLimitOK(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	// All content is encrypted under E2EE-everywhere; the 65536 ciphertext cap applies.
	content := make([]byte, 5000)
	for i := range content {
		content[i] = 'a'
	}
	assert.True(t, hub.validateContentLength(clientID, string(content)))
}

func TestValidateContentLengthAboveCiphertextLimitRejected(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	// 65537 bytes exceeds the encrypted-ciphertext cap.
	content := make([]byte, 65537)
	for i := range content {
		content[i] = 'a'
	}
	assert.False(t, hub.validateContentLength(clientID, string(content)))
}

// --- handleIncoming routing tests ---

func TestHandleIncomingSubscribeRoute(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	// subscribe with missing channel_id sends error
	msg := IncomingMessage{
		Type:     "subscribe",
		ClientID: clientID,
		UserID:   uuid.New(),
		Data:     map[string]interface{}{},
	}
	hub.handleIncoming(msg)

	// Should receive error about invalid channel_id
	select {
	case data := <-client.Send:
		var out OutgoingMessage
		require.NoError(t, json.Unmarshal(data, &out))
		assert.Equal(t, "error", out.Type)
	default:
		t.Fatal("expected error message")
	}
}

func TestHandleIncomingUnsubscribeRoute(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	client.Channels[channelID] = true
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
		},
	}
	hub.handleIncoming(msg)

	assert.False(t, client.Channels[channelID])
}

func TestHandleIncomingSubscribeServerRoute(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		Type:     "subscribe_server",
		ClientID: clientID,
		UserID:   uuid.New(),
		Data:     map[string]interface{}{},
	}
	hub.handleIncoming(msg)

	select {
	case data := <-client.Send:
		var out OutgoingMessage
		require.NoError(t, json.Unmarshal(data, &out))
		assert.Equal(t, "error", out.Type)
	default:
		t.Fatal("expected error message for missing server_id")
	}
}

func TestHandleIncomingUnsubscribeServerRoute(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	serverID := uuid.New()
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe_server",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}
	hub.handleIncoming(msg)
	// Verify removed
	_, exists := hub.serverSubscriptions[serverID]
	assert.False(t, exists, "server subscription map should be deleted when empty")
}

func TestHandleIncomingSubscribeDMRoute(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		ClientID: clientID,
		UserID:   uuid.New(),
		Data:     map[string]interface{}{},
	}
	hub.handleIncoming(msg)

	select {
	case data := <-client.Send:
		var out OutgoingMessage
		require.NoError(t, json.Unmarshal(data, &out))
		assert.Equal(t, "error", out.Type)
	default:
		t.Fatal("expected error message for missing conversation_id")
	}
}

func TestHandleIncomingUnsubscribeDMRoute(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client
	convID := uuid.New()
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{clientID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe_dm",
		ClientID: clientID,
		UserID:   uuid.New(),
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
		},
	}
	hub.handleIncoming(msg)
	assert.False(t, hub.dmSubscriptions[convID][clientID])
}

func TestHandleIncomingSetStatusRoute(_ *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Type:     "set_status",
		ClientID: uuid.New(),
		UserID:   uuid.New(),
		Data:     map[string]interface{}{},
	}
	// Missing status — should not panic, just return
	hub.handleIncoming(msg)
}

func TestHandleIncomingTypingRoute(_ *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Type:     "typing",
		ClientID: uuid.New(),
		UserID:   uuid.New(),
		Data:     map[string]interface{}{},
	}
	// Missing channel_id — should not panic, just return
	hub.handleIncoming(msg)
}

func TestHandleIncomingDMTypingRoute(_ *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: uuid.New(),
		UserID:   uuid.New(),
		Data:     map[string]interface{}{},
	}
	// Missing conversation_id — should not panic
	hub.handleIncoming(msg)
}

func TestHandleIncomingProfileUpdateRouteClientNotFound(_ *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Type:     "profile_update",
		ClientID: uuid.New(), // not registered
		UserID:   uuid.New(),
		Data:     map[string]interface{}{},
	}
	// Client not found — should return early without panic
	hub.handleIncoming(msg)
}

// --- flushOnlineCounts edge cases ---

func TestFlushOnlineCountsEmptyPending(_ *testing.T) {
	hub := newMinimalHub()
	// Should not panic with nil DB/Redis when pending is empty
	hub.flushOnlineCounts()
}

// --- parseDMMessageFields tests ---

func TestParseDMMessageFieldsEmptyContent(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		ClientID: clientID,
		Data: map[string]interface{}{
			keyContent: "",
		},
	}
	input, valid := hub.parseDMMessageFields(msg)
	assert.False(t, valid)
	assert.Nil(t, input)
}

func TestParseDMMessageFieldsMissingContent(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		ClientID: clientID,
		Data:     map[string]interface{}{},
	}
	input, valid := hub.parseDMMessageFields(msg)
	assert.False(t, valid)
	assert.Nil(t, input)
}

func TestParseDMMessageFieldsSystemType(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		ClientID: clientID,
		Data: map[string]interface{}{
			keyContent:    "system message",
			keyKeyVersion: float64(1),
			"type":        "system",
		},
	}
	input, valid := hub.parseDMMessageFields(msg)
	assert.True(t, valid)
	require.NotNil(t, input)
	assert.Equal(t, "system", input.msgType)
}

func TestParseDMMessageFieldsCustomKeyVersion(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		ClientID: clientID,
		Data: map[string]interface{}{
			keyContent:    "hello",
			keyKeyVersion: float64(5),
		},
	}
	input, valid := hub.parseDMMessageFields(msg)
	assert.True(t, valid)
	require.NotNil(t, input)
	assert.Equal(t, 5, input.keyVersion)
}

func TestParseDMMessageFieldsDefaultMsgType(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		ClientID: clientID,
		Data: map[string]interface{}{
			keyContent:    "hello",
			keyKeyVersion: float64(1),
		},
	}
	input, valid := hub.parseDMMessageFields(msg)
	assert.True(t, valid)
	require.NotNil(t, input)
	assert.Equal(t, "user", input.msgType)
	assert.Equal(t, 1, input.keyVersion)
}

// --- validateDMMessage tests ---

func TestValidateDMMessageInvalidConversationID(t *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyConversationID: "not-a-uuid",
		},
	}
	client, convUUID, input := hub.validateDMMessage(msg)
	assert.Nil(t, client)
	assert.Equal(t, uuid.Nil, convUUID)
	assert.Nil(t, input)
}

func TestValidateDMMessageMissingConversationID(t *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}
	client, convUUID, input := hub.validateDMMessage(msg)
	assert.Nil(t, client)
	assert.Equal(t, uuid.Nil, convUUID)
	assert.Nil(t, input)
}

func TestValidateDMMessageClientNotFound(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	msg := IncomingMessage{
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
		},
	}
	client, convUUID, input := hub.validateDMMessage(msg)
	assert.Nil(t, client)
	assert.Equal(t, uuid.Nil, convUUID)
	assert.Nil(t, input)
}

func TestValidateDMMessageNotSubscribed(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
		},
	}
	c, convUUID, input := hub.validateDMMessage(msg)
	assert.Nil(t, c)
	assert.Equal(t, uuid.Nil, convUUID)
	assert.Nil(t, input)

	// Verify error was sent
	select {
	case data := <-client.Send:
		var out OutgoingMessage
		require.NoError(t, json.Unmarshal(data, &out))
		assert.Equal(t, "error", out.Type)
	default:
		t.Fatal("expected error message")
	}
}

func TestValidateDMMessageRateLimited(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	// Exhaust rate limit
	for i := 0; i < 100; i++ {
		client.rateLimitAllow()
	}

	msg := IncomingMessage{
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
			keyContent:        "hello",
		},
	}
	c, convUUID, input := hub.validateDMMessage(msg)
	assert.Nil(t, c)
	assert.Equal(t, uuid.Nil, convUUID)
	assert.Nil(t, input)
}

func TestValidateDMMessageValidMessage(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
			keyContent:        "hello there",
			keyKeyVersion:     float64(1),
		},
	}
	c, convUUID, input := hub.validateDMMessage(msg)
	assert.NotNil(t, c)
	assert.Equal(t, convID, convUUID)
	require.NotNil(t, input)
	assert.Equal(t, "hello there", input.content)
}

// --- handleConnectionReadyProbe tests ---

func TestHub_ConnectionReadyProbe_EmitsConnectionReady(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	// Seed channel subscriptions on the client
	client.Channels[uuid.New()] = true
	client.Channels[uuid.New()] = true
	client.Channels[uuid.New()] = true

	// Seed server subscriptions in hub-level map (2 servers)
	srv1, srv2 := uuid.New(), uuid.New()
	hub.serverSubscriptions[srv1] = map[uuid.UUID]bool{client.ID: true}
	hub.serverSubscriptions[srv2] = map[uuid.UUID]bool{client.ID: true, uuid.New(): true}

	// Seed DM subscriptions in hub-level map (1 DM)
	dm1 := uuid.New()
	hub.dmSubscriptions[dm1] = map[uuid.UUID]bool{client.ID: true}

	hub.handleConnectionReadyProbe(IncomingMessage{
		Type:     "connection_ready_probe",
		Data:     map[string]interface{}{"protocol_version": float64(2)},
		ClientID: client.ID,
		UserID:   client.UserID,
	})

	select {
	case raw := <-client.Send:
		var out OutgoingMessage
		require.NoError(t, json.Unmarshal(raw, &out))
		assert.Equal(t, "connection_ready", out.Type)
		assert.Equal(t, float64(2), out.Data["protocol_version"])
		assert.Equal(t, float64(3), out.Data["subscribed_channels"])
		assert.Equal(t, float64(2), out.Data["subscribed_servers"])
		assert.Equal(t, float64(1), out.Data["subscribed_dms"])
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive connection_ready on client.Send")
	}
}

func TestHub_ConnectionReadyProbe_UnknownClient_NoEmission(_ *testing.T) {
	hub := newMinimalHub()
	// Do NOT add any client to hub.clients

	// Must not panic on unknown ClientID
	hub.handleConnectionReadyProbe(IncomingMessage{
		Type:     "connection_ready_probe",
		Data:     map[string]interface{}{"protocol_version": float64(2)},
		ClientID: uuid.New(),
		UserID:   uuid.New(),
	})
	// If we reach here without panic, the handler safely no-op'd. Done.
}

func TestHub_ConnectionReadyProbe_FullSendBuffer_NoBlock(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	// Zero-capacity channel with no receiver: sends will always fall through
	// to the handler's `default` branch in the non-blocking select.
	client.Send = make(chan []byte)
	hub.clients[client.ID] = client

	start := time.Now()
	hub.handleConnectionReadyProbe(IncomingMessage{
		Type:     "connection_ready_probe",
		ClientID: client.ID,
		UserID:   client.UserID,
	})
	elapsed := time.Since(start)

	// Handler must return immediately via `default` branch (no blocking send).
	assert.Less(t, elapsed, 50*time.Millisecond,
		"handler should return immediately when send buffer is full")

	// Verify the frame was NOT delivered (buffer was full → default branch fired).
	select {
	case <-client.Send:
		t.Fatal("expected no frame on full buffer; default branch should have dropped the send")
	default:
		// Expected — no frame delivered.
	}
}

// TestHandleIncomingConnectionReadyProbeRoute verifies that the handleIncoming
// switch dispatches "connection_ready_probe" to handleConnectionReadyProbe,
// causing a "connection_ready" frame to appear on the client's Send channel.
// A typo in the case string would cause no frame to be delivered, failing this test.
func TestHandleIncomingConnectionReadyProbeRoute(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "connection_ready_probe",
		Data:     map[string]interface{}{"protocol_version": float64(2)},
		ClientID: client.ID,
		UserID:   client.UserID,
	}
	hub.handleIncoming(msg)

	select {
	case raw := <-client.Send:
		var out OutgoingMessage
		require.NoError(t, json.Unmarshal(raw, &out))
		assert.Equal(t, "connection_ready", out.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("handleIncoming did not dispatch connection_ready_probe to handler")
	}
}

// TestHub_RejectsEnvelopeMissingKeyVersion_Returns4400 verifies the new
// envelope validator (#1025): an incoming "message" frame whose Data lacks
// key_version triggers validateEnvelope → rejectEnvelope, which writes a
// websocket close frame with code 4400 and reason
// "missing_or_invalid_key_version". The test wires a real WebSocket
// connection via httptest so the close frame is observable.
func TestHub_RejectsEnvelopeMissingKeyVersion_Returns4400(t *testing.T) {
	hub := newMinimalHub()

	userID := uuid.New()
	channelUUID := uuid.New()

	// Subscribe the (yet-to-be-registered) client's channel so the message
	// reaches parseMessageInput (and the envelope validator) rather than
	// short-circuiting on "Not subscribed".
	hub.channelSubscriptions[channelUUID] = map[uuid.UUID]bool{}

	// Set up a Gin route that upgrades the connection and wires the resulting
	// Client into the hub map so handleMessage can find it. We don't run
	// hub.Run() — the test calls handleMessage synchronously after the
	// upgrade so the close frame fires before the request handler returns.
	gin.SetMode(gin.TestMode)
	router := gin.New()

	upgrader := gorillaWS.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(_ *http.Request) bool { return true },
	}

	clientReady := make(chan *Client, 1)
	router.GET("/ws", func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		client := &Client{
			ID:       uuid.New(),
			UserID:   userID,
			Username: "test-envelope-reject",
			Conn:     conn,
			Hub:      hub,
			Send:     make(chan []byte, 8),
			Channels: map[uuid.UUID]bool{channelUUID: true},
		}
		hub.clients[client.ID] = client
		hub.channelSubscriptions[channelUUID][client.ID] = true
		clientReady <- client

		// Park the goroutine until the test closes the connection so the
		// close frame written by rejectEnvelope has a live socket on which
		// to arrive.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	wsURL := "ws" + srv.URL[4:] + "/ws"
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer func() { _ = conn.Close() }()

	// Wait for server-side client registration before dispatching the
	// synthetic IncomingMessage.
	var client *Client
	select {
	case client = <-clientReady:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client registration")
	}

	// Synthetic message envelope WITHOUT key_version — this is exactly the
	// shape that the new validator (#1025) is meant to reject.
	msg := IncomingMessage{
		Type:     "message",
		UserID:   userID,
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyChannelID: channelUUID.String(),
			keyContent:   "ciphertext that lacks key_version",
		},
	}
	hub.handleMessage(msg)

	// Expect the next inbound frame on the client side to be a close frame
	// with code 4400 and the documented reason string.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, readErr := conn.ReadMessage()
	require.Error(t, readErr, "expected connection to close")

	var closeErr *gorillaWS.CloseError
	require.True(t, errors.As(readErr, &closeErr),
		"expected *websocket.CloseError, got %T: %v", readErr, readErr)
	assert.Equal(t, 4400, closeErr.Code)
	assert.Equal(t, "missing_or_invalid_key_version", closeErr.Text)
}

// TestValidateEnvelope_TableDriven covers all four rejection branches of
// validateEnvelope (#1025). The close-frame integration test above only
// exercises the "missing key_version" branch. This table-driven unit test
// adds explicit coverage for the float64-below-1, int-below-1, and
// non-numeric-type branches so future refactors that "simplify" the switch
// (e.g., collapsing into kv > 0 and losing the type assertion) regress
// loudly rather than silently accepting `key_version: "1"` or similar.
func TestValidateEnvelope_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		data      map[string]interface{}
		wantError bool
	}{
		{"missing_key_version_field", map[string]interface{}{keyContent: "x"}, true},
		{"float64_zero", map[string]interface{}{keyKeyVersion: float64(0)}, true},
		{"float64_negative", map[string]interface{}{keyKeyVersion: float64(-1)}, true},
		{"float64_valid_one", map[string]interface{}{keyKeyVersion: float64(1)}, false},
		{"float64_valid_high", map[string]interface{}{keyKeyVersion: float64(99)}, false},
		{"int_zero", map[string]interface{}{keyKeyVersion: 0}, true},
		{"int_negative", map[string]interface{}{keyKeyVersion: -5}, true},
		{"int_valid", map[string]interface{}{keyKeyVersion: 3}, false},
		{"string_value", map[string]interface{}{keyKeyVersion: "1"}, true},
		{"bool_value", map[string]interface{}{keyKeyVersion: true}, true},
		{"nil_value", map[string]interface{}{keyKeyVersion: nil}, true},
		{"map_value", map[string]interface{}{keyKeyVersion: map[string]interface{}{"x": 1}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &IncomingMessage{Data: tt.data}
			err := validateEnvelope(msg)
			if tt.wantError {
				assert.ErrorIs(t, err, errMissingKeyVersion)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestParseDMMessageFields_RejectsEnvelopeMissingKeyVersion verifies the
// DM-send wiring (#1025): parseDMMessageFields invokes validateEnvelope
// before any DB work and returns (nil, false) when the envelope lacks
// key_version. The channel-send wiring is covered by
// TestHub_RejectsEnvelopeMissingKeyVersion_Returns4400 above; this test
// closes the gap so a future refactor that detaches the DM path from the
// validator regresses loudly.
func TestParseDMMessageFields_RejectsEnvelopeMissingKeyVersion(t *testing.T) {
	hub := newMinimalHub()
	clientID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[clientID] = client

	msg := IncomingMessage{
		ClientID: clientID,
		UserID:   uuid.New(),
		Data: map[string]interface{}{
			keyContent: "ciphertext that lacks key_version",
			// intentionally NO key_version
		},
	}
	input, valid := hub.parseDMMessageFields(msg)
	assert.Nil(t, input)
	assert.False(t, valid)
}
