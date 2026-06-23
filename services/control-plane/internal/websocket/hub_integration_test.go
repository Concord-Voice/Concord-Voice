package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPermViewVoiceChannels int64 = 1 << 9
	testPermViewTextChannels  int64 = 1 << 10
	testPermSendMessages      int64 = 1 << 11
	testBaseChannelPerms            = testPermViewVoiceChannels | testPermViewTextChannels | testPermSendMessages
)

// --- handleMessage integration tests (require DB) ---

func setupMessageTest(t *testing.T) *hubTestSetup {
	t.Helper()

	db := setupHubTestDB(t)
	hub := NewHub(db, nil)
	hub.SetChannelPermissionChecker(&testChannelPermissionChecker{db: db})

	userID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec

	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		userID.String(), "hubmsguser@test.concord.chat", "hubmsguser", hash)
	require.NoError(t, err)

	// Create server
	serverID := uuid.New()
	_, err = db.Exec(`INSERT INTO servers (id, name, owner_id, allow_embedded_content) VALUES ($1, $2, $3, true)`,
		serverID.String(), "Test Server", userID.String())
	require.NoError(t, err)

	roleID := uuid.New()
	_, err = db.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		 VALUES ($1, $2, '@all', 0, $3, TRUE, TRUE)`,
		roleID.String(), serverID.String(), testBaseChannelPerms)
	require.NoError(t, err)

	// Add user as member
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		serverID.String(), userID.String())
	require.NoError(t, err)

	// Create channel
	channelID := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`,
		channelID.String(), serverID.String(), "general")
	require.NoError(t, err)

	clientID := uuid.New()
	client := &Client{
		ID:       clientID,
		UserID:   userID,
		Username: "hubmsguser",
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: map[uuid.UUID]bool{channelID: true},
	}

	hub.clients[clientID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{clientID: true}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{clientID: true}
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{clientID: true}

	t.Cleanup(func() {
		if _, err := db.Exec(`TRUNCATE users, servers, server_members, channels, messages CASCADE`); err != nil {
			t.Errorf("failed to truncate tables: %v", err)
		}
	})

	return &hubTestSetup{
		hub:    hub,
		db:     db,
		client: client,
		convID: channelID.String(),
		user1:  userID,
		user2:  serverID, // reusing user2 field for serverID
	}
}

func addHubMemberClient(t *testing.T, setup *hubTestSetup, username string) *Client {
	t.Helper()

	userID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec
	_, err := setup.db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		userID.String(), username+"@test.concord.chat", username, hash)
	require.NoError(t, err)

	_, err = setup.db.Exec(
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
		setup.user2.String(), userID.String())
	require.NoError(t, err)

	_, err = setup.db.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id)
		 SELECT $1, $2, id FROM roles WHERE server_id = $1 AND is_default = TRUE`,
		setup.user2.String(), userID.String())
	require.NoError(t, err)

	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Username: username,
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[client.ID] = client
	setup.hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
	return client
}

func denyDefaultRolePermission(t *testing.T, setup *hubTestSetup, perm int64) {
	t.Helper()

	var roleID string
	err := setup.db.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`,
		setup.user2.String(),
	).Scan(&roleID)
	require.NoError(t, err)

	_, err = setup.db.Exec(
		`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		 VALUES ($1, $2, 'role', $3, 0, $4)`,
		uuid.New().String(), setup.convID, roleID, perm)
	require.NoError(t, err)
}

type testChannelPermissionChecker struct {
	db *sql.DB
}

func (c *testChannelPermissionChecker) HasChannelPermission(
	ctx context.Context,
	serverID, userID, channelID string,
	permBit int64,
) (bool, error) {
	var isMember bool
	if err := c.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID).Scan(&isMember); err != nil {
		return false, err
	}
	if !isMember {
		return false, nil
	}

	var ownerID string
	if err := c.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		return false, err
	}
	if ownerID == userID {
		return true, nil
	}

	var basePerms int64
	if err := c.db.QueryRowContext(ctx, `
		SELECT COALESCE(BIT_OR(r.permissions), 0)
		FROM member_roles mr
		INNER JOIN roles r ON mr.role_id = r.id
		WHERE mr.server_id = $1 AND mr.user_id = $2
	`, serverID, userID).Scan(&basePerms); err != nil {
		return false, err
	}

	rows, err := c.db.QueryContext(ctx, `
		SELECT target_type, allow, deny
		FROM channel_permission_overrides
		WHERE channel_id = $1
		  AND (
		      (target_type = 'user' AND target_id = $2)
		      OR (target_type = 'role' AND target_id IN (
		          SELECT role_id FROM member_roles
		          WHERE server_id = $3 AND user_id = $2
		      ))
		  )`, channelID, userID, serverID)
	if err != nil {
		return false, err
	}
	defer rows.Close() //nolint:errcheck

	var userAllow, userDeny, roleAllow, roleDeny int64
	for rows.Next() {
		var targetType string
		var allow, deny int64
		if err := rows.Scan(&targetType, &allow, &deny); err != nil {
			return false, err
		}
		if targetType == "user" {
			userAllow |= allow
			userDeny |= deny
		} else {
			roleAllow |= allow
			roleDeny |= deny
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	finalPerms := basePerms
	finalPerms |= roleAllow
	finalPerms &^= roleDeny
	finalPerms |= userAllow
	finalPerms &^= userDeny
	return finalPerms&permBit != 0, nil
}

func TestHandleMessagePlaintextSuccess(t *testing.T) {
	setup := setupMessageTest(t)
	channelID := setup.convID

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  channelID,
			keyContent:    "Hello, world!",
			keyKeyVersion: float64(1),
			keyNonce:      "nonce-123",
		},
	}

	setup.hub.handleMessage(msg)

	// Should receive message_ack
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, "nonce-123", data[keyNonce])
	assert.NotEmpty(t, data["id"])
}

func TestHandleMessageNotSubscribed(t *testing.T) {
	setup := setupMessageTest(t)

	// Use a channel the client is NOT subscribed to
	otherChannelID := uuid.New()

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: otherChannelID.String(),
			keyContent:   "should fail",
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Not subscribed")
}

func TestHandleMessageDeniedByChannelSendOverride(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, err := uuid.Parse(setup.convID)
	require.NoError(t, err)

	client := addHubMemberClient(t, setup, "hubsenddeny")
	client.Channels[channelUUID] = true
	setup.hub.channelSubscriptions[channelUUID][client.ID] = true
	denyDefaultRolePermission(t, setup, testPermSendMessages)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   client.UserID,
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    "blocked by channel override",
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Not authorized")
}

func TestHandleMessageDeniedByChannelVisibilityOverride(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, err := uuid.Parse(setup.convID)
	require.NoError(t, err)

	client := addHubMemberClient(t, setup, "hubviewdeny")
	client.Channels[channelUUID] = true
	setup.hub.channelSubscriptions[channelUUID][client.ID] = true
	denyDefaultRolePermission(t, setup, testPermViewTextChannels)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   client.UserID,
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    "blocked by channel view override",
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Not authorized")
}

func TestDeliveryAuthForChannelLoadsViewPermission(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, err := uuid.Parse(setup.convID)
	require.NoError(t, err)

	serverID, viewPerm, ok := setup.hub.deliveryAuthForChannel(channelUUID)

	assert.True(t, ok)
	assert.Equal(t, setup.user2, serverID)
	assert.Equal(t, testPermViewTextChannels, viewPerm)
}

func TestHandleChannelRevalidationPrunesViewerDeniedSubscriber(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, err := uuid.Parse(setup.convID)
	require.NoError(t, err)

	member := addHubMemberClient(t, setup, "hubchannelrevaldeny")
	member.Channels[channelUUID] = true
	setup.hub.channelSubscriptions[channelUUID][member.ID] = true
	denyDefaultRolePermission(t, setup, testPermViewTextChannels)

	setup.hub.handleChannelRevalidation(channelRevalidation{
		serverID:  setup.user2,
		channelID: channelUUID,
	})
	applyAsyncChannelDelivery(t, setup.hub)

	assert.True(t, setup.hub.channelSubscriptions[channelUUID][setup.client.ID], "owner subscriber should remain")
	assert.False(t, setup.hub.channelSubscriptions[channelUUID][member.ID], "viewer-denied member should be pruned")
	assert.False(t, member.Channels[channelUUID], "viewer-denied member should lose local channel subscription")
}

func TestHandleServerRevalidationPrunesViewerDeniedSubscriber(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, err := uuid.Parse(setup.convID)
	require.NoError(t, err)

	member := addHubMemberClient(t, setup, "hubserverrevaldeny")
	member.Channels[channelUUID] = true
	setup.hub.channelSubscriptions[channelUUID][member.ID] = true
	denyDefaultRolePermission(t, setup, testPermViewTextChannels)

	setup.hub.handleServerRevalidation(setup.user2)
	applyAsyncChannelDelivery(t, setup.hub)

	assert.True(t, setup.hub.channelSubscriptions[channelUUID][setup.client.ID], "owner subscriber should remain")
	assert.False(t, setup.hub.channelSubscriptions[channelUUID][member.ID], "viewer-denied member should be pruned")
	assert.False(t, member.Channels[channelUUID], "viewer-denied member should lose local channel subscription")
}

func TestHandleMessageEmptyContent(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   "",
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "content is required")
}

func TestHandleMessageMissingContent(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleMessageContentTooLong(t *testing.T) {
	setup := setupMessageTest(t)

	// Content > 65536 ciphertext cap under E2EE-everywhere (#201).
	longContent := make([]byte, 65537)
	for i := range longContent {
		longContent[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    string(longContent),
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "maximum length")
}

// Under E2EE-everywhere (#201) all message content is ciphertext; the
// "_Encrypted_" qualifier in the name is a pre-#201 vestige. The test
// remains a length-cap regression check at 5000 bytes.
func TestHandleMessageEncryptedContentLongerLimit(t *testing.T) {
	setup := setupMessageTest(t)

	// 5000 chars is well below the 65536 ciphertext cap under E2EE-everywhere.
	longContent := make([]byte, 5000)
	for i := range longContent {
		longContent[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    string(longContent),
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	// Should succeed (5000 < 65536)
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", resp["type"])
}

func TestHandleMessageInvalidChannelID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: "not-a-uuid",
			keyContent:   "test",
		},
	}

	// Should not panic, returns early
	setup.hub.handleMessage(msg)
}

func TestHandleMessageMissingChannelID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{keyContent: "test"},
	}

	// Should not panic, returns early
	setup.hub.handleMessage(msg)
}

func TestHandleMessageClientNotFound(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: uuid.New(), // non-existent client
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   "test",
		},
	}

	// Should not panic
	setup.hub.handleMessage(msg)
}

func TestHandleMessageRateLimitExceeded(t *testing.T) {
	setup := setupMessageTest(t)

	// Exhaust rate limit
	setup.client.rateTokens = 0
	setup.client.rateLastFill = time.Now()

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   "rate limited message",
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Rate limit")
}

func TestHandleMessageBroadcastToOtherSubscribers(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, _ := uuid.Parse(setup.convID)

	// Add a second user/client subscribed to the same channel
	otherUser := uuid.New()
	otherClient := &Client{
		ID:       uuid.New(),
		UserID:   otherUser,
		Username: "otheruser",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: map[uuid.UUID]bool{channelUUID: true},
	}
	setup.hub.clients[otherClient.ID] = otherClient
	setup.hub.userClients[otherUser] = map[uuid.UUID]bool{otherClient.ID: true}
	setup.hub.channelSubscriptions[channelUUID][otherClient.ID] = true

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    "broadcast test",
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	// Sender gets message_ack
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", resp["type"])

	// The broadcast message goes to the hub's broadcast channel
	select {
	case bMsg := <-setup.hub.broadcast:
		assert.Equal(t, channelUUID, bMsg.ChannelID)
		assert.Equal(t, "message", bMsg.Data.Type)
		assert.Equal(t, &setup.user1, bMsg.ExcludeUser)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected broadcast message")
	}
}

// --- handleSubscribe integration tests ---

func TestHandleSubscribeSuccess(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, _ := uuid.Parse(setup.convID)

	// Remove from current subscriptions to test subscribing
	delete(setup.client.Channels, channelUUID)
	delete(setup.hub.channelSubscriptions[channelUUID], setup.client.ID)

	msg := IncomingMessage{
		Type:     "subscribe",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
		},
	}

	setup.hub.handleSubscribe(msg)

	// Should receive subscribed confirmation
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "subscribed", resp["type"])

	// Client should be in channel subscriptions
	assert.True(t, setup.client.Channels[channelUUID])
	assert.True(t, setup.hub.channelSubscriptions[channelUUID][setup.client.ID])
}

func TestHandleSubscribeDeniedByChannelVisibilityOverride(t *testing.T) {
	tests := []struct {
		name        string
		channelType string
		denyPerm    int64
	}{
		{name: "text", channelType: "text", denyPerm: testPermViewTextChannels},
		{name: "voice", channelType: "voice", denyPerm: testPermViewVoiceChannels},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setup := setupMessageTest(t)
			channelUUID, err := uuid.Parse(setup.convID)
			require.NoError(t, err)

			_, err = setup.db.Exec(`UPDATE channels SET type = $1 WHERE id = $2`, tt.channelType, setup.convID)
			require.NoError(t, err)
			client := addHubMemberClient(t, setup, "hubsubdeny"+tt.name)
			denyDefaultRolePermission(t, setup, tt.denyPerm)

			msg := IncomingMessage{
				Type:     "subscribe",
				UserID:   client.UserID,
				ClientID: client.ID,
				Data: map[string]interface{}{
					keyChannelID: setup.convID,
				},
			}

			setup.hub.handleSubscribe(msg)

			resp := readClientMsg(t, client)
			assert.Equal(t, "error", resp["type"])
			assert.False(t, client.Channels[channelUUID])
			assert.False(t, setup.hub.channelSubscriptions[channelUUID][client.ID])
		})
	}
}

func TestHandleSubscribeInvalidChannelID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "subscribe",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: "not-a-uuid",
		},
	}

	setup.hub.handleSubscribe(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribeMissingChannelID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "subscribe",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleSubscribe(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribeClientNotFound(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "subscribe",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
		},
	}

	// Should not panic
	setup.hub.handleSubscribe(msg)
}

// --- handleSubscribeServer integration tests ---

func TestHandleSubscribeServerSuccess(t *testing.T) {
	setup := setupMessageTest(t)
	serverID := setup.user2 // reused field for serverID

	msg := IncomingMessage{
		Type:     "subscribe_server",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	setup.hub.handleSubscribeServer(msg)

	// Client should be subscribed to server
	assert.True(t, setup.hub.serverSubscriptions[serverID][setup.client.ID])
}

func TestHandleSubscribeServerInvalidServerID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "subscribe_server",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: "not-a-uuid",
		},
	}

	setup.hub.handleSubscribeServer(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribeServerMissingServerID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "subscribe_server",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleSubscribeServer(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

// --- handleSubscribeDM integration tests ---

func TestHandleSubscribeDMSuccess(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
		},
	}

	// Client is already subscribed from setup, remove first
	convUUID, _ := uuid.Parse(setup.convID)
	delete(setup.hub.dmSubscriptions[convUUID], setup.client.ID)

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_subscribed", resp["type"])
	assert.True(t, setup.hub.dmSubscriptions[convUUID][setup.client.ID])
}

func TestHandleSubscribeDMNotParticipant(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	// Create a DM conversation the user is NOT part of
	otherConvID := uuid.New()
	_, err := setup.db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, false, $2)`,
		otherConvID.String(), setup.user2.String())
	require.NoError(t, err)
	_, err = setup.db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`,
		otherConvID.String(), setup.user2.String())
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: otherConvID.String(),
		},
	}

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Not a participant")
}

func TestHandleSubscribeDMInvalidConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: "not-a-uuid",
		},
	}

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribeDMMissingConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

// --- handleDMMessage additional tests ---

func TestHandleDMMessageEmptyContent(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "content is required")
}

func TestHandleDMMessageContentTooLong(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	// Content > 65536 ciphertext cap under E2EE-everywhere (#201).
	longContent := make([]byte, 65537)
	for i := range longContent {
		longContent[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        string(longContent),
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "maximum length")
}

func TestHandleDMMessageRateLimitExceeded(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	setup.client.rateTokens = 0
	setup.client.rateLastFill = time.Now()

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "rate limited",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Rate limit")
}

func TestHandleDMMessageInvalidConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: "not-a-uuid",
			keyContent:        "test",
		},
	}

	// Should return early without panic
	setup.hub.handleDMMessage(msg)
}

func TestHandleDMMessageMissingConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyContent: "test",
		},
	}

	// Should return early without panic
	setup.hub.handleDMMessage(msg)
}

func TestHandleDMMessageSystemType(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "system message",
			"key_version":     float64(1),
			"type":            "system",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_message_ack", resp["type"])
}

func TestHandleDMMessageEncryptedLongerContentAllowed(t *testing.T) {
	setup := setupEpochTest(t, false, false)

	content := make([]byte, 5000)
	for i := range content {
		content[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        string(content),
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_message_ack", resp["type"])
}

func TestHandleDMMessageBroadcastToSubscribers(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// Add user2 as a subscriber
	client2ID := uuid.New()
	client2 := &Client{
		ID:       client2ID,
		UserID:   setup.user2,
		Username: "hubuser2",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[client2ID] = client2
	setup.hub.userClients[setup.user2] = map[uuid.UUID]bool{client2ID: true}
	setup.hub.dmSubscriptions[convUUID][client2ID] = true

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "dm broadcast test",
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	// Sender gets ack
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_message_ack", resp["type"])

	// Broadcast goes to dmBroadcast channel
	select {
	case dmMsg := <-setup.hub.dmBroadcast:
		assert.Equal(t, convUUID, dmMsg.ConversationID)
		assert.Equal(t, "dm_message", dmMsg.Data.Type)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected DM broadcast")
	}
}

// --- handleSetStatus with valid statuses (requires Redis) ---

func TestHandleSetStatusOnline(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Username: "statususer",
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data: map[string]interface{}{
			keyStatus: "online",
		},
	}

	hub.handleSetStatus(msg)

	// Verify Redis was set
	val, err := redisClient.Get(context.Background(), "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "online", val)
}

func TestHandleSetStatusDND(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data: map[string]interface{}{
			keyStatus: "dnd",
		},
	}

	hub.handleSetStatus(msg)

	val, err := redisClient.Get(context.Background(), "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "dnd", val)
}

func TestHandleSetStatusInvisible(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data: map[string]interface{}{
			keyStatus: "invisible",
		},
	}

	hub.handleSetStatus(msg)

	// Invisible stores real status in Redis
	val, err := redisClient.Get(context.Background(), "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "invisible", val)

	// But broadcasts as offline
	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		assert.Equal(t, "presence", msg["type"])
		msgData := msg["data"].(map[string]interface{})
		assert.Equal(t, "offline", msgData["status"])
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected presence broadcast")
	}
}

// --- handleHeartbeat tests (requires Redis) ---

func TestHandleHeartbeatRefreshesTTL(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := "presence:" + userID.String()
	require.NoError(t, redisClient.Set(ctx, key, "online", 120*time.Second).Err())

	msg := IncomingMessage{
		Type:   "heartbeat",
		UserID: userID,
	}

	hub.handleHeartbeat(msg)

	ttl := redisClient.TTL(ctx, key).Val()
	assert.Greater(t, ttl, 100*time.Second)
}

func TestHandleHeartbeatMissingKeyResetsOnline(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	msg := IncomingMessage{
		Type:   "heartbeat",
		UserID: userID,
	}

	hub.handleHeartbeat(msg)

	ctx := context.Background()
	val, err := redisClient.Get(ctx, "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "online", val)
}

func TestHandleHeartbeatDNDRefreshesWithoutChangingStatus(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := "presence:" + userID.String()
	require.NoError(t, redisClient.Set(ctx, key, "dnd", 60*time.Second).Err())

	msg := IncomingMessage{
		Type:   "heartbeat",
		UserID: userID,
	}

	hub.handleHeartbeat(msg)

	// Status should remain dnd
	val, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, "dnd", val)
}

// --- handleProfileUpdate integration tests ---

func TestHandleProfileUpdateSuccess(t *testing.T) {
	setup := setupMessageTest(t)

	// Update the user's display name in DB
	newDisplayName := "Updated Name"
	_, err := setup.db.Exec(`UPDATE users SET display_name = $1 WHERE id = $2`, newDisplayName, setup.user1.String())
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     "profile_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleProfileUpdate(msg)

	// Client's cached DisplayName should be updated
	assert.Equal(t, newDisplayName, *setup.client.DisplayName)
}

func TestHandleProfileUpdateClientNotFound(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "profile_update",
		UserID:   setup.user1,
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	setup.hub.handleProfileUpdate(msg)
}

// --- handleServerUpdate integration tests ---

func TestHandleServerUpdateSuccess(t *testing.T) {
	setup := setupMessageTest(t)
	serverID := setup.user2 // reused field for serverID

	// Add client as server subscriber
	setup.hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{setup.client.ID: true}

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	setup.hub.handleServerUpdate(msg)

	// Should receive server_updated broadcast
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "server_updated", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, serverID.String(), data[keyServerID])
	assert.Equal(t, "Test Server", data["name"])
}

func TestHandleServerUpdateInvalidServerID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: "not-a-uuid",
		},
	}

	// Should not panic
	setup.hub.handleServerUpdate(msg)
}

func TestHandleServerUpdateMissingServerID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	// Should not panic
	setup.hub.handleServerUpdate(msg)
}

func TestHandleServerUpdateNonexistentServer(t *testing.T) {
	setup := setupMessageTest(t)

	fakeServerID := uuid.New()

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: fakeServerID.String(),
		},
	}

	// Should not panic (DB query fails gracefully)
	setup.hub.handleServerUpdate(msg)
}

func TestHandleServerUpdateNoSubscribers(t *testing.T) {
	setup := setupMessageTest(t)
	serverID := setup.user2

	// Remove all subscribers
	delete(setup.hub.serverSubscriptions, serverID)

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	// Should not panic
	setup.hub.handleServerUpdate(msg)
}

// --- handleIncoming routing tests (additional coverage) ---

// --- broadcastPresenceToAll tests ---

// --- #47 base-presence audience-filtering helpers (inlined: the websocket
// package's own tests cannot import internal/testhelpers — that package imports
// websocket, which would be a test import cycle). ---

func presenceTestUser(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2 || '@presence.test', 'p_' || left($2, 8), 'x', true, true)`,
		id, id.String(),
	)
	require.NoError(t, err)
	return id
}

func presenceTestFriendship(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		a, b,
	)
	require.NoError(t, err)
}

func presenceTestServerWithMembers(t *testing.T, db *sql.DB, owner uuid.UUID, members ...uuid.UUID) {
	t.Helper()
	srv := uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 's_' || left($2, 8), $3)`, srv, srv.String(), owner)
	require.NoError(t, err)
	for _, m := range members {
		_, merr := db.Exec(`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, srv, m)
		require.NoError(t, merr)
	}
}

func TestBroadcastPresenceToAll(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	// #47: base presence is audience-filtered. A friend receives the broadcast;
	// an unrelated user must NOT (regression-locks the closed leak — previously
	// this test asserted an unrelated user DID receive it).
	sender := presenceTestUser(t, db)
	friend := presenceTestUser(t, db)
	stranger := presenceTestUser(t, db)
	presenceTestFriendship(t, db, sender, friend)

	friendClient := &Client{ID: uuid.New(), UserID: friend, Send: make(chan []byte, 10)}
	strangerClient := &Client{ID: uuid.New(), UserID: stranger, Send: make(chan []byte, 10)}
	hub.clients[friendClient.ID] = friendClient
	hub.clients[strangerClient.ID] = strangerClient
	hub.userClients[friend] = map[uuid.UUID]bool{friendClient.ID: true}
	hub.userClients[stranger] = map[uuid.UUID]bool{strangerClient.ID: true}

	hub.broadcastPresenceToAll(sender, "online", time.Now().Unix())

	resp := readClientMsg(t, friendClient)
	assert.Equal(t, "presence", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, sender.String(), data[keyUserID])
	assert.Equal(t, "online", data[keyStatus])

	select {
	case <-strangerClient.Send:
		t.Fatal("#47 leak: an unrelated user received a base presence broadcast")
	default:
	}
}

// TestBroadcastPresenceToAll_SharedServerPeerReceives locks the server-membership
// arm of the audience: a peer who shares a server with the sender receives the
// broadcast even without a friendship.
func TestBroadcastPresenceToAll_SharedServerPeerReceives(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	sender := presenceTestUser(t, db)
	peer := presenceTestUser(t, db)
	presenceTestServerWithMembers(t, db, sender, sender, peer)

	peerClient := &Client{ID: uuid.New(), UserID: peer, Send: make(chan []byte, 10)}
	hub.clients[peerClient.ID] = peerClient
	hub.userClients[peer] = map[uuid.UUID]bool{peerClient.ID: true}

	hub.broadcastPresenceToAll(sender, "online", time.Now().Unix())

	resp := readClientMsg(t, peerClient)
	assert.Equal(t, "presence", resp["type"])
	assert.Equal(t, sender.String(), resp["data"].(map[string]interface{})[keyUserID])
}

// TestSendPresenceSnapshotExcludesNonAudience locks the snapshot path: a
// connecting viewer's presence_snapshot includes a friend but NOT an unrelated
// connected user.
func TestSendPresenceSnapshotExcludesNonAudience(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	viewer := presenceTestUser(t, db)
	friend := presenceTestUser(t, db)
	stranger := presenceTestUser(t, db)
	presenceTestFriendship(t, db, viewer, friend)

	friendClient := &Client{ID: uuid.New(), UserID: friend, Send: make(chan []byte, 10)}
	strangerClient := &Client{ID: uuid.New(), UserID: stranger, Send: make(chan []byte, 10)}
	hub.clients[friendClient.ID] = friendClient
	hub.clients[strangerClient.ID] = strangerClient
	hub.userClients[friend] = map[uuid.UUID]bool{friendClient.ID: true}
	hub.userClients[stranger] = map[uuid.UUID]bool{strangerClient.ID: true}

	viewerClient := &Client{ID: uuid.New(), UserID: viewer, Send: make(chan []byte, 10)}
	hub.sendPresenceSnapshot(viewerClient)

	resp := readClientMsg(t, viewerClient)
	assert.Equal(t, "presence_snapshot", resp["type"])
	rawUsers := resp["data"].(map[string]interface{})["users"].([]interface{})
	ids := map[string]bool{}
	for _, u := range rawUsers {
		ids[u.(map[string]interface{})["user_id"].(string)] = true
	}
	assert.True(t, ids[friend.String()], "friend should appear in the viewer's snapshot")
	assert.False(t, ids[stranger.String()], "#47 leak: an unrelated user appeared in the viewer's snapshot")
}

// --- sendDMUnreadNotify tests ---

// testLastMessage returns a deterministic dmUnreadLastMessage fixture for tests.
var testLastMessageTime = time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)

func testLastMessage() dmUnreadLastMessage {
	return dmUnreadLastMessage{
		content:   "hello from test",
		userID:    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		username:  "testuser",
		createdAt: testLastMessageTime,
	}
}

func TestSendDMUnreadNotifySendsToUnsubscribedParticipants(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// user2 is a participant but NOT subscribed to the DM
	client2ID := uuid.New()
	client2 := &Client{
		ID:       client2ID,
		UserID:   setup.user2,
		Username: "hubuser2",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[client2ID] = client2
	setup.hub.userClients[setup.user2] = map[uuid.UUID]bool{client2ID: true}

	lastMsg := testLastMessage()
	setup.hub.sendDMUnreadNotify(convUUID, setup.user1, lastMsg)

	select {
	case data := <-client2.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		assert.Equal(t, "dm_unread_notify", msg["type"])

		// Verify last_message is included with correct fields
		msgData, ok := msg["data"].(map[string]interface{})
		require.True(t, ok)
		lm, ok := msgData["last_message"].(map[string]interface{})
		require.True(t, ok, "last_message must be present in dm_unread_notify")
		assert.Equal(t, lastMsg.content, lm["content"])
		assert.Equal(t, lastMsg.userID, lm["user_id"])
		assert.Equal(t, lastMsg.username, lm["username"])
		assert.NotEmpty(t, lm["created_at"])
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected dm_unread_notify for unsubscribed participant")
	}
}

func TestSendDMUnreadNotifySkipsSender(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// The sender (user1) should NOT receive unread notify
	// user1's client is already registered
	setup.hub.sendDMUnreadNotify(convUUID, setup.user1, testLastMessage())

	select {
	case <-setup.client.Send:
		t.Fatal("sender should not receive dm_unread_notify")
	case <-time.After(100 * time.Millisecond):
		// Expected: no message for sender
	}
}

func TestSendDMUnreadNotifySkipsSubscribedParticipants(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// user2 is a participant AND subscribed to the DM
	client2ID := uuid.New()
	client2 := &Client{
		ID:       client2ID,
		UserID:   setup.user2,
		Username: "hubuser2",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[client2ID] = client2
	setup.hub.userClients[setup.user2] = map[uuid.UUID]bool{client2ID: true}
	setup.hub.dmSubscriptions[convUUID][client2ID] = true

	setup.hub.sendDMUnreadNotify(convUUID, setup.user1, testLastMessage())

	select {
	case <-client2.Send:
		t.Fatal("subscribed participant should not receive dm_unread_notify")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}
}

// --- DisconnectUser / DisconnectSession thread-safe wrappers ---

func TestDisconnectUserQueuesOnChannel(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()

	hub.DisconnectUser(userID)

	select {
	case received := <-hub.disconnectUser:
		assert.Equal(t, userID, received)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected user ID on disconnectUser channel")
	}
}

func TestDisconnectSessionQueuesOnChannel(t *testing.T) {
	hub := NewHub(nil, nil)

	hub.DisconnectSession("session-abc")

	select {
	case received := <-hub.disconnectSession:
		assert.Equal(t, "session-abc", received)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected session ID on disconnectSession channel")
	}
}

// --- Epoch enforcement in handleMessage ---

func TestHandleMessageEpochRevoked(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, _ := uuid.Parse(setup.convID)

	// Seed a channel key and revocation
	_, err := setup.db.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
		channelUUID.String(), setup.user1.String(), []byte("test-key"))
	require.NoError(t, err)

	_, err = setup.db.Exec(`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'test', $2)`,
		channelUUID.String(), setup.user1.String())
	require.NoError(t, err)

	_, err = setup.db.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 2)`,
		channelUUID.String(), setup.user1.String(), []byte("test-key-v2"))
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    "encrypted with revoked epoch",
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, "epoch_revoked", data["code"])
	assert.Equal(t, float64(2), data["current_epoch"])
}

// --- validateReplyToID integration tests ---

func TestValidateReplyToIDNoReply(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}
	replyID, ok := setup.hub.validateReplyToID(msg, setup.convID)
	assert.True(t, ok)
	assert.Nil(t, replyID)
}

func TestValidateReplyToIDEmptyString(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			"reply_to_id": "",
		},
	}
	replyID, ok := setup.hub.validateReplyToID(msg, setup.convID)
	assert.True(t, ok)
	assert.Nil(t, replyID)
}

func TestValidateReplyToIDInvalidUUID(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			"reply_to_id": "not-a-uuid",
		},
	}
	_, ok := setup.hub.validateReplyToID(msg, setup.convID)
	assert.False(t, ok)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestValidateReplyToIDMessageNotFound(t *testing.T) {
	setup := setupMessageTest(t)

	msg := IncomingMessage{
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			"reply_to_id": uuid.New().String(),
		},
	}
	_, ok := setup.hub.validateReplyToID(msg, setup.convID)
	assert.False(t, ok)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestValidateReplyToIDValidReply(t *testing.T) {
	setup := setupMessageTest(t)
	channelID := setup.convID

	// Insert a message to reply to
	replyMsgID := uuid.New()
	_, err := setup.db.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at) VALUES ($1, $2, $3, $4, 1, NOW(), NOW())`,
		replyMsgID.String(), channelID, setup.user1.String(), "original message",
	)
	require.NoError(t, err)

	msg := IncomingMessage{
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			"reply_to_id": replyMsgID.String(),
		},
	}
	replyID, ok := setup.hub.validateReplyToID(msg, channelID)
	assert.True(t, ok)
	require.NotNil(t, replyID)
	assert.Equal(t, replyMsgID.String(), *replyID)
}

func TestValidateReplyToIDWrongChannel(t *testing.T) {
	setup := setupMessageTest(t)

	// Insert a message in the real channel
	replyMsgID := uuid.New()
	_, err := setup.db.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at) VALUES ($1, $2, $3, $4, 1, NOW(), NOW())`,
		replyMsgID.String(), setup.convID, setup.user1.String(), "original message",
	)
	require.NoError(t, err)

	// Try to reply referencing the right message ID but a different channel
	otherChannelID := uuid.New().String()
	msg := IncomingMessage{
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			"reply_to_id": replyMsgID.String(),
		},
	}
	_, ok := setup.hub.validateReplyToID(msg, otherChannelID)
	assert.False(t, ok)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

// --- resolveVisibleOnline integration tests ---

func TestResolveVisibleOnlineConnectedOnline(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	uid := uuid.New()
	client := newTestClient(hub, uid)
	hub.clients[client.ID] = client
	hub.userClients[uid] = map[uuid.UUID]bool{client.ID: true}

	// Set status to online in Redis
	ctx := context.Background()
	redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, uid), statusOnline, 120*time.Second)

	allMembers := map[uuid.UUID]bool{uid: true}
	visible := hub.resolveVisibleOnline(allMembers)
	assert.True(t, visible[uid])
}

func TestResolveVisibleOnlineInvisibleNotCounted(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	uid := uuid.New()
	client := newTestClient(hub, uid)
	hub.clients[client.ID] = client
	hub.userClients[uid] = map[uuid.UUID]bool{client.ID: true}

	// Set status to invisible in Redis
	ctx := context.Background()
	redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, uid), statusInvisible, 120*time.Second)

	allMembers := map[uuid.UUID]bool{uid: true}
	visible := hub.resolveVisibleOnline(allMembers)
	assert.False(t, visible[uid])
}

func TestResolveVisibleOnlineMissingRedisKey(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	uid := uuid.New()
	client := newTestClient(hub, uid)
	hub.clients[client.ID] = client
	hub.userClients[uid] = map[uuid.UUID]bool{client.ID: true}

	// No Redis key — should treat as online
	allMembers := map[uuid.UUID]bool{uid: true}
	visible := hub.resolveVisibleOnline(allMembers)
	assert.True(t, visible[uid])
}

func TestResolveVisibleOnlineNotConnected(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	uid := uuid.New()
	// uid is NOT in hub.userClients — not connected
	allMembers := map[uuid.UUID]bool{uid: true}
	visible := hub.resolveVisibleOnline(allMembers)
	assert.False(t, visible[uid])
}

func TestResolveVisibleOnlineEmpty(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	visible := hub.resolveVisibleOnline(map[uuid.UUID]bool{})
	assert.Empty(t, visible)
}

// --- broadcastServerVoiceCounts integration tests ---

func TestBroadcastServerVoiceCountsEmptyResult(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	hub.broadcastServerVoiceCounts()

	// Should receive server_voice_counts with empty counts
	resp := readClientMsg(t, client)
	assert.Equal(t, "server_voice_counts", resp["type"])
}

// --- sendVoiceCountsSnapshot integration tests ---

func TestSendVoiceCountsSnapshotSuccess(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	hub.sendVoiceCountsSnapshot(context.Background(), client)

	// Should receive server_voice_counts message
	resp := readClientMsg(t, client)
	assert.Equal(t, "server_voice_counts", resp["type"])
}

// --- handleDisconnectUser integration tests ---

func TestHandleDisconnectUserRemovesAllUserClients(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	userID := uuid.New()
	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{
		client1.ID: true,
		client2.ID: true,
	}

	// Set presence so last-client unregister path works
	ctx := context.Background()
	redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, userID), statusOnline, 120*time.Second)

	hub.handleDisconnectUser(userID)

	assert.Empty(t, hub.clients, "all clients should be removed")
	assert.Empty(t, hub.userClients[userID], "user client map should be empty")

	// Both clients should have received session_revoked
	for _, c := range []*Client{client1, client2} {
		select {
		case data := <-c.Send:
			var msg map[string]interface{}
			require.NoError(t, json.Unmarshal(data, &msg))
			assert.Equal(t, sessionRevoked, msg["type"])
		default:
			// Channel may be closed — that's also valid
		}
	}
}

// --- flushOnlineCounts integration test ---

func TestFlushOnlineCountsFullPipeline(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec

	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		userID.String(), "flushcount@test.concord.chat", "flushcount", hash)
	require.NoError(t, err)

	serverID := uuid.New()
	_, err = db.Exec(`INSERT INTO servers (id, name, owner_id, allow_embedded_content) VALUES ($1, $2, $3, true)`,
		serverID.String(), "Count Server", userID.String())
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		serverID.String(), userID.String())
	require.NoError(t, err)

	// Set up connected client
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	// Set presence to online
	ctx := context.Background()
	redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, userID), statusOnline, 120*time.Second)

	// Mark user as pending
	hub.onlineCountPending[userID] = true

	hub.flushOnlineCounts()

	// Should receive server_online_counts
	resp := readClientMsg(t, client)
	assert.Equal(t, "server_online_counts", resp["type"])
	data := resp["data"].(map[string]interface{})
	counts := data[keyCounts].(map[string]interface{})
	assert.Equal(t, float64(1), counts[serverID.String()])
}

// TestHandleMessageAcceptsCiphertextAt65536 verifies the hub accepts content
// of exactly 65536 bytes (the new ciphertext cap). Boundary test for the
// message-length-policy cap raise from 24000 to 65536.
func TestHandleMessageAcceptsCiphertextAt65536(t *testing.T) {
	setup := setupMessageTest(t)

	// Exactly 65536 bytes — at the cap boundary, must be accepted.
	content := make([]byte, 65536)
	for i := range content {
		content[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    string(content),
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	// Should succeed (65536 == cap, not > cap)
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", resp["type"])
}

// TestHandleMessageRejectsCiphertextAt65537 verifies the hub rejects content
// of 65537 bytes via the len(content) > 65536 check.
func TestHandleMessageRejectsCiphertextAt65537(t *testing.T) {
	setup := setupMessageTest(t)

	// 65537 bytes — one byte over the cap, must be rejected.
	content := make([]byte, 65537)
	for i := range content {
		content[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    string(content),
			keyKeyVersion: float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "maximum length")
}
