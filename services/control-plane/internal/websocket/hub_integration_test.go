package websocket

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPermViewVoiceChannels int64 = 1 << 9
	testPermViewTextChannels  int64 = 1 << 10
	testPermSendMessages      int64 = 1 << 11
	testBaseChannelPerms            = testPermViewVoiceChannels | testPermViewTextChannels | testPermSendMessages
)

type commandErrorHook struct {
	failures map[string]error
}

type failOnceCommandHook struct {
	command string
	err     error
	failed  atomic.Bool
}

func (h commandErrorHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h commandErrorHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := h.failures[cmd.Name()]; err != nil {
			return err
		}
		return next(ctx, cmd)
	}
}

func (h commandErrorHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if err := h.failures[cmd.Name()]; err != nil {
				return err
			}
		}
		return next(ctx, cmds)
	}
}

func (h *failOnceCommandHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *failOnceCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == h.command && h.failed.CompareAndSwap(false, true) {
			return h.err
		}
		return next(ctx, cmd)
	}
}

func (h *failOnceCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func captureHubLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return &buf
}

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
) (allowed bool, err error) {
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
	defer func() {
		err = errors.Join(err, rows.Close())
	}()

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

// TestBroadcastToChannelAuthorized_FiltersStaleSubscriber covers CV-CAN-021..026:
// a REST-triggered channel-mutation broadcast (edit / delete / embed-suppress /
// reaction / pin / unpin) routed through BroadcastToChannelAuthorized must be
// filtered by per-recipient view permission, so a stale subscriber that has lost
// channel view access does not receive it — unlike the plain BroadcastToChannel
// path, whose zero ServerID/ViewPermission is treated as allow-all.
func TestBroadcastToChannelAuthorized_FiltersStaleSubscriber(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, err := uuid.Parse(setup.convID)
	require.NoError(t, err)

	// A member who subscribed earlier but has since been denied channel view.
	member := addHubMemberClient(t, setup, "hubbcastdeny")
	member.Channels[channelUUID] = true
	setup.hub.channelSubscriptions[channelUUID][member.ID] = true
	denyDefaultRolePermission(t, setup, testPermViewTextChannels)

	// A message_delete event, as DeleteMessage now emits it via the authorized fanout.
	setup.hub.handleBroadcast(BroadcastMessage{
		ChannelID: channelUUID,
		Data: OutgoingMessage{
			Type: "message_delete",
			Data: map[string]interface{}{keyChannelID: setup.convID},
		},
		RequireViewAuth: true,
	})

	select {
	case result := <-setup.hub.channelDeliveryResults:
		allowedByClient := make(map[uuid.UUID]bool)
		for _, d := range result.decisions {
			allowedByClient[d.clientID] = d.allowed
		}
		assert.True(t, allowedByClient[setup.client.ID], "server owner must still receive the event")
		assert.False(t, allowedByClient[member.ID], "view-denied stale subscriber must be filtered out")
	case <-time.After(time.Second):
		t.Fatal("expected an async channel delivery result")
	}
}

// TestBroadcastToChannelAuthorized_DropsWhenChannelGone covers the authorized
// broadcast's `!authOK` branch: when the channel no longer resolves (deleted),
// deliveryAuthForChannel prunes its subscriptions and returns authOK=false, and
// handleBroadcast drops the message before any delivery — no delivery result is
// emitted.
func TestBroadcastToChannelAuthorized_DropsWhenChannelGone(t *testing.T) {
	setup := setupMessageTest(t)
	goneChannel := uuid.New() // never inserted -> fetchChannelContextForAuth ErrNoRows

	// Seed a stale subscriber for the gone channel. Without a subscription
	// entry, handleBroadcast returns at its "no subscribers" guard before it
	// ever evaluates RequireViewAuth, so the !authOK branch would go
	// unexercised (the test would pass even if deliveryAuthForChannel stopped
	// dropping/pruning deleted channels). With the subscriber present,
	// resolution must reach deliveryAuthForChannel, hit ErrNoRows, prune the
	// subscription, and drop the broadcast.
	stale := addHubMemberClient(t, setup, "hubgonechan")
	stale.Channels[goneChannel] = true
	setup.hub.channelSubscriptions[goneChannel] = map[uuid.UUID]bool{stale.ID: true}

	setup.hub.handleBroadcast(BroadcastMessage{
		ChannelID: goneChannel,
		Data: OutgoingMessage{
			Type: "message_delete",
			Data: map[string]interface{}{keyChannelID: goneChannel.String()},
		},
		RequireViewAuth: true,
	})

	select {
	case <-setup.hub.channelDeliveryResults:
		t.Fatal("a gone channel must drop the authorized broadcast (no delivery result)")
	case <-time.After(300 * time.Millisecond):
		// expected: authOK=false short-circuited handleBroadcast before delivery.
	}

	// deliveryAuthForChannel must have pruned the stale subscription on ErrNoRows.
	assert.Empty(t, setup.hub.channelSubscriptions[goneChannel],
		"gone-channel resolution must prune the stale channel subscription")
	assert.False(t, stale.Channels[goneChannel],
		"gone-channel resolution must clear the client's channel membership")
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

func TestHandleSetStatusInvisibleRedisWriteFailureFailsClosedLocally(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	logs := captureHubLog(t)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 120*time.Second).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"set": errors.New("redis SET failed")}})

	hub.handleSetStatus(IncomingMessage{
		UserID: userID,
		Data:   map[string]interface{}{keyStatus: statusInvisible},
	})

	assert.Contains(t, logs.String(), "failed to persist presence status")
	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
	assert.Equal(t, statusInvisible, hub.resolveVisibleStatus(ctx, userID, userID))
	assert.NotContains(t, hub.resolveVisibleOnline(map[uuid.UUID]bool{userID: true}), userID)
	select {
	case data := <-client.Send:
		var outgoing OutgoingMessage
		require.NoError(t, json.Unmarshal(data, &outgoing))
		assert.Equal(t, "presence", outgoing.Type)
		assert.Equal(t, statusOffline, outgoing.Data[keyStatus])
	default:
		t.Fatal("expected fail-closed offline broadcast")
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

func TestHandleHeartbeatMissingKeyFailsClosed(t *testing.T) {
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
	_, err := redisClient.Get(ctx, "presence:"+userID.String()).Result()
	assert.ErrorIs(t, err, redis.Nil)
	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
}

// Regression for #2202.
func TestHandleHeartbeatRecoversLocallyHiddenOfflineAfterRedisRecovers(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}
	hub.setHiddenPresence(userID, statusOffline)

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	_, hidden := hub.hiddenPresence[userID]
	assert.False(t, hidden, "a valid persisted status should clear the transient fail-closed override")
	assert.Equal(t, statusOnline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
	assert.Contains(t, hub.resolveVisibleOnline(map[uuid.UUID]bool{userID: true}), userID)
	assert.Greater(t, redisClient.TTL(ctx, key).Val(), 100*time.Second)
}

func TestHandleHeartbeatRepairsInvisibleWhenRedisHasStaleVisibleStatus(t *testing.T) {
	tests := []struct {
		name       string
		setFailure error
		wantStored string
		wantLog    string
	}{
		{name: "repair succeeds", wantStored: statusInvisible},
		{
			name:       "repair fails closed",
			setFailure: errors.New("redis SET failed"),
			wantStored: statusOnline,
			wantLog:    "failed to repair invisible presence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redisClient := setupHubTestRedis(t)
			hub := NewHub(nil, redisClient)
			logs := captureHubLog(t)
			userID := uuid.New()
			hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}
			hub.setHiddenPresence(userID, statusInvisible)

			ctx := context.Background()
			key := fmt.Sprintf(presenceKeyFmt, userID)
			require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())
			if tt.setFailure != nil {
				redisClient.AddHook(commandErrorHook{failures: map[string]error{"set": tt.setFailure}})
			}

			hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

			stored, err := redisClient.Get(ctx, key).Result()
			require.NoError(t, err)
			assert.Equal(t, tt.wantStored, stored)
			assert.Equal(t, statusInvisible, hub.hiddenPresence[userID])
			assert.Equal(t, statusOffline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
			assert.NotContains(t, hub.resolveVisibleOnline(map[uuid.UUID]bool{userID: true}), userID)
			if tt.wantLog != "" {
				assert.Contains(t, logs.String(), tt.wantLog)
			}
		})
	}
}

func TestHandleHeartbeatVisibleTTLRefreshFailureKeepsKnownVisibleState(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	logs := captureHubLog(t)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"expire": errors.New("redis EXPIRE failed")}})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.Contains(t, logs.String(), "failed to refresh presence TTL")
	_, hidden := hub.hiddenPresence[userID]
	assert.False(t, hidden)
	assert.Equal(t, statusOnline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
	assert.Contains(t, hub.resolveVisibleOnline(map[uuid.UUID]bool{userID: true}), userID)
}

func TestHandleHeartbeatInvalidPersistedStatusFailsClosedOnce(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, "corrupt", 60*time.Second).Err())

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
	assert.Contains(t, hub.onlineCountPending, userID)
	clear(hub.onlineCountPending)

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.NotContains(t, hub.onlineCountPending, userID, "an already-hidden user should not rebroadcast offline")
}

func TestHandleHeartbeatMissingKeyDisconnectedDoesNothing(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	_, hidden := hub.hiddenPresence[userID]
	assert.False(t, hidden)
	assert.NotContains(t, hub.onlineCountPending, userID)
}

func TestHandleHeartbeatRestoresOnlineAfterTransientInitialPersistenceFailure(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	redisClient.AddHook(&failOnceCommandHook{command: "set", err: errors.New("redis SET failed")})
	hub := NewHub(db, redisClient)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.handleRegister(client)
	t.Cleanup(func() {
		if _, registered := hub.clients[client.ID]; registered {
			hub.handleUnregister(client)
		}
	})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	stored, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusOnline, stored)
	_, hidden := hub.hiddenPresence[userID]
	assert.False(t, hidden)
	assert.Equal(t, statusOnline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
	assert.Contains(t, hub.onlineCountPending, userID)
}

func TestHandleHeartbeatInitialPresenceRestoreFailureStaysHidden(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	redisClient.AddHook(&failOnceCommandHook{command: "set", err: errors.New("initial Redis SET failed")})
	hub := NewHub(db, redisClient)
	logs := captureHubLog(t)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.handleRegister(client)
	t.Cleanup(func() {
		if _, registered := hub.clients[client.ID]; registered {
			hub.handleUnregister(client)
		}
	})
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"set": errors.New("restore Redis SET failed")}})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	ctx := context.Background()
	stored, err := redisClient.Get(ctx, fmt.Sprintf(presenceKeyFmt, userID)).Result()
	assert.ErrorIs(t, err, redis.Nil)
	assert.Empty(t, stored)
	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.Contains(t, logs.String(), "failed to restore visible presence")
}

func TestHandleHeartbeatRepeatedMissingKeyStaysFailClosed(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	clear(hub.onlineCountPending)
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	ctx := context.Background()
	stored, err := redisClient.Get(ctx, fmt.Sprintf(presenceKeyFmt, userID)).Result()
	assert.ErrorIs(t, err, redis.Nil)
	assert.Empty(t, stored)
	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.NotContains(t, hub.onlineCountPending, userID)
}

func TestHandleHeartbeatInvalidStatusCancelsInitialOnlineRestore(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	redisClient.AddHook(&failOnceCommandHook{command: "set", err: errors.New("initial Redis SET failed")})
	hub := NewHub(db, redisClient)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.handleRegister(client)
	t.Cleanup(func() {
		if _, registered := hub.clients[client.ID]; registered {
			hub.handleUnregister(client)
		}
	})

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, "corrupt", 60*time.Second).Err())
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	require.NoError(t, redisClient.Del(ctx, key).Err())
	clear(hub.onlineCountPending)

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	stored, err := redisClient.Get(ctx, key).Result()
	assert.ErrorIs(t, err, redis.Nil)
	assert.Empty(t, stored)
	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.NotContains(t, hub.onlineCountPending, userID)
}

func TestHandleHeartbeatRestoresOnlineWhenKeyExpiresAfterReadError(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	redisClient.AddHook(&failOnceCommandHook{command: "get", err: errors.New("redis GET failed")})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	require.NoError(t, redisClient.Del(ctx, key).Err())
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	stored, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusOnline, stored)
	assert.NotContains(t, hub.hiddenPresence, userID)
	recovery, known := hub.presenceRecovery[userID]
	require.True(t, known)
	assert.Equal(t, presenceRecoveryState{status: statusOnline}, recovery)
}

func TestHandleHeartbeatRestoresDNDWhenKeyExpiresAfterReadError(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusDND, 60*time.Second).Err())
	// Observe the user-selected status before the outage so recovery has an
	// authoritative local value if the Redis key later expires.
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	redisClient.AddHook(&failOnceCommandHook{command: "get", err: errors.New("redis GET failed")})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	recovery, known := hub.presenceRecovery[userID]
	require.True(t, known)
	assert.Equal(t, presenceRecoveryState{status: statusDND, pending: true}, recovery)
	clear(hub.onlineCountPending)
	redisClient.AddHook(&failOnceCommandHook{command: "get", err: errors.New("second redis GET failed")})
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	assert.Equal(t, presenceRecoveryState{status: statusDND, pending: true}, hub.presenceRecovery[userID])
	assert.NotContains(t, hub.onlineCountPending, userID)

	require.NoError(t, redisClient.Del(ctx, key).Err())
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	stored, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusDND, stored)
	assert.NotContains(t, hub.hiddenPresence, userID)
	assert.Equal(t, presenceRecoveryState{status: statusDND}, hub.presenceRecovery[userID])
}

func TestHandleSetStatusFailureRetainsLastTrustedRecoveryStatus(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	redisClient.AddHook(&failOnceCommandHook{command: "set", err: errors.New("redis SET failed")})

	hub.handleSetStatus(IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data:   map[string]interface{}{keyStatus: statusDND},
	})

	recovery, known := hub.presenceRecovery[userID]
	require.True(t, known)
	assert.Equal(t, presenceRecoveryState{status: statusOnline, pending: true}, recovery)

	require.NoError(t, redisClient.Del(ctx, key).Err())
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	stored, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusOnline, stored)
	assert.NotContains(t, hub.hiddenPresence, userID)
}

func TestHandleHeartbeatRestoresVisibleStatusAfterTTLRefreshFailure(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	logs := captureHubLog(t)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusDND, 60*time.Second).Err())
	redisClient.AddHook(&failOnceCommandHook{command: "expire", err: errors.New("redis EXPIRE failed")})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	recovery, known := hub.presenceRecovery[userID]
	require.True(t, known)
	assert.Equal(t, presenceRecoveryState{status: statusDND, pending: true}, recovery)
	require.NoError(t, redisClient.Del(ctx, key).Err())
	redisClient.AddHook(&failOnceCommandHook{command: "set", err: errors.New("redis SET failed")})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.Equal(t, presenceRecoveryState{status: statusDND, pending: true}, hub.presenceRecovery[userID])
	assert.Contains(t, logs.String(), "failed to restore visible presence")
	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	stored, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusDND, stored)
	assert.NotContains(t, hub.hiddenPresence, userID)
}

func TestHandleHeartbeatRestoresVisibleStatusWhenTTLKeyAlreadyMissing(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusDND, 60*time.Second).Err())
	// Returning nil without running EXPIRE leaves BoolCmd at false with no error,
	// matching Redis when the key vanishes between GET and EXPIRE.
	redisClient.AddHook(&failOnceCommandHook{command: "expire"})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	recovery, known := hub.presenceRecovery[userID]
	require.True(t, known)
	assert.Equal(t, presenceRecoveryState{status: statusDND, pending: true}, recovery)
	require.NoError(t, redisClient.Del(ctx, key).Err())

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	stored, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusDND, stored)
	assert.NotContains(t, hub.hiddenPresence, userID)
}

func TestHandleHeartbeatReadErrorWithoutKnownStatusStaysFailClosed(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}
	redisClient.AddHook(&failOnceCommandHook{command: "get", err: errors.New("redis GET failed")})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.NotContains(t, hub.presenceRecovery, userID)
	clear(hub.onlineCountPending)

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	stored, err := redisClient.Get(ctx, key).Result()
	assert.ErrorIs(t, err, redis.Nil)
	assert.Empty(t, stored)
	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.NotContains(t, hub.presenceRecovery, userID)
	assert.NotContains(t, hub.onlineCountPending, userID)
}

func TestHandleUnregisterClearsRecoveryOnlyAfterLastClient(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	first := newTestClient(hub, userID)
	second := newTestClient(hub, userID)
	hub.clients[first.ID] = first
	hub.clients[second.ID] = second
	hub.userClients[userID] = map[uuid.UUID]bool{first.ID: true, second.ID: true}
	hub.presenceRecovery[userID] = presenceRecoveryState{status: statusDND, pending: true}

	hub.handleUnregister(first)

	assert.Contains(t, hub.presenceRecovery, userID)

	hub.handleUnregister(second)

	assert.NotContains(t, hub.presenceRecovery, userID)
}

func TestHandleHeartbeatRedisReadErrorFailsClosedOnce(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"get": errors.New("redis GET failed")}})
	hub := NewHub(nil, redisClient)
	logs := captureHubLog(t)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.Contains(t, logs.String(), "failed to read presence heartbeat state")
	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(context.Background(), userID, uuid.New()))
	assert.Contains(t, hub.onlineCountPending, userID)
	clear(hub.onlineCountPending)

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	assert.NotContains(t, hub.onlineCountPending, userID, "an already-hidden user should not rebroadcast offline")
}

func TestSetHiddenPresenceInitializesNilMap(t *testing.T) {
	hub := newMinimalHub()
	hub.hiddenPresence = nil
	userID := uuid.New()

	hub.setHiddenPresence(userID, statusOffline)

	assert.Equal(t, statusOffline, hub.hiddenPresence[userID])
}

func TestHandleIncomingDropsSetStatusAfterUnregister(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleUnregister(client)
	hub.handleIncoming(IncomingMessage{
		Type:     "set_status",
		ClientID: client.ID,
		UserID:   userID,
		Data:     map[string]interface{}{keyStatus: statusOnline},
	})

	_, err := redisClient.Get(context.Background(), fmt.Sprintf(presenceKeyFmt, userID)).Result()
	assert.ErrorIs(t, err, redis.Nil)
}

func TestHandleIncomingDropsHeartbeatAfterUnregister(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"del": errors.New("redis DEL failed")}})
	hub.handleUnregister(client)

	hub.handleIncoming(IncomingMessage{
		Type:     "heartbeat",
		ClientID: client.ID,
		UserID:   userID,
	})

	assert.Less(t, redisClient.TTL(ctx, key).Val(), 100*time.Second)
}

func TestHandleIncomingRoutesPresenceForRegisteredClient(t *testing.T) {
	t.Run("heartbeat", func(t *testing.T) {
		redisClient := setupHubTestRedis(t)
		hub := NewHub(nil, redisClient)
		userID := uuid.New()
		client := newTestClient(hub, userID)
		hub.clients[client.ID] = client
		hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

		ctx := context.Background()
		key := fmt.Sprintf(presenceKeyFmt, userID)
		require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())

		hub.handleIncoming(IncomingMessage{Type: "heartbeat", ClientID: client.ID, UserID: userID})

		assert.Greater(t, redisClient.TTL(ctx, key).Val(), 100*time.Second)
	})

	t.Run("set status", func(t *testing.T) {
		redisClient := setupHubTestRedis(t)
		hub := NewHub(nil, redisClient)
		userID := uuid.New()
		client := newTestClient(hub, userID)
		hub.clients[client.ID] = client
		hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

		hub.handleIncoming(IncomingMessage{
			Type:     "set_status",
			ClientID: client.ID,
			UserID:   userID,
			Data:     map[string]interface{}{keyStatus: "dnd"},
		})

		stored, err := redisClient.Get(context.Background(), fmt.Sprintf(presenceKeyFmt, userID)).Result()
		require.NoError(t, err)
		assert.Equal(t, "dnd", stored)
	})
}

func TestHandleIncomingDropsMismatchedPresenceUser(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}
	otherUserID := uuid.New()

	hub.handleIncoming(IncomingMessage{
		Type:     "set_status",
		ClientID: client.ID,
		UserID:   otherUserID,
		Data:     map[string]interface{}{keyStatus: statusOnline},
	})

	_, err := redisClient.Get(context.Background(), fmt.Sprintf(presenceKeyFmt, otherUserID)).Result()
	assert.ErrorIs(t, err, redis.Nil)
}

func TestHandleHeartbeatDoesNotExposeInvisibleUserAfterTTLRefreshFailure(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	logs := captureHubLog(t)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusInvisible, 120*time.Second).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"expire": errors.New("redis EXPIRE failed")}})

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	assert.Contains(t, logs.String(), "failed to refresh presence TTL")
	require.NoError(t, redisClient.Del(ctx, key).Err())

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})
	value, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusInvisible, value)
	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
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

func TestHandleHeartbeatLogsTTLRefreshFailure(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	userID := uuid.New()
	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusInvisible, 120*time.Second).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"expire": errors.New("redis EXPIRE failed")}})
	hub := NewHub(nil, redisClient)
	logs := captureHubLog(t)

	hub.handleHeartbeat(IncomingMessage{UserID: userID})

	assert.Contains(t, logs.String(), "failed to refresh presence TTL")
}

func TestHandleHeartbeatLogsInvisibleRestoreFailure(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}
	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, userID)
	require.NoError(t, redisClient.Set(ctx, key, statusInvisible, 120*time.Second).Err())
	hub.handleHeartbeat(IncomingMessage{UserID: userID})
	require.NoError(t, redisClient.Del(ctx, key).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"set": errors.New("redis SET failed")}})
	logs := captureHubLog(t)

	hub.handleHeartbeat(IncomingMessage{UserID: userID})

	assert.Contains(t, logs.String(), "failed to restore invisible presence")
	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
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
	require.NoError(t, redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, uid), statusOnline, 120*time.Second).Err())

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
	require.NoError(t, redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, uid), statusInvisible, 120*time.Second).Err())

	allMembers := map[uuid.UUID]bool{uid: true}
	visible := hub.resolveVisibleOnline(allMembers)
	assert.False(t, visible[uid])
}

func TestResolveVisibleOnlineMissingRedisKeyFailsClosed(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	uid := uuid.New()
	client := newTestClient(hub, uid)
	hub.clients[client.ID] = client
	hub.userClients[uid] = map[uuid.UUID]bool{client.ID: true}

	// No Redis key means the persisted status is unavailable; do not risk
	// exposing a connected user whose invisible write or TTL refresh failed.
	allMembers := map[uuid.UUID]bool{uid: true}
	visible := hub.resolveVisibleOnline(allMembers)
	assert.False(t, visible[uid])
}

func TestResolveVisibleStatusRedisErrorFailsClosedForOtherViewer(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	uid := uuid.New()
	viewerID := uuid.New()

	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(presenceKeyFmt, uid), statusInvisible, 120*time.Second).Err())
	require.NoError(t, redisClient.Close())

	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(context.Background(), uid, viewerID))
}

func TestResolveVisibleStatusRedisErrorKeepsSelfOnline(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	uid := uuid.New()

	require.NoError(t, redisClient.Close())

	assert.Equal(t, statusOnline, hub.resolveVisibleStatus(context.Background(), uid, uid))
}

func TestResolveVisibleStatusMissingKeyFailsClosedForOtherViewer(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	uid := uuid.New()

	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(context.Background(), uid, uuid.New()))
}

func TestResolveVisibleStatusInvalidValueFailsClosedForOtherViewer(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	uid := uuid.New()
	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(presenceKeyFmt, uid), "corrupt", 120*time.Second).Err())

	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(context.Background(), uid, uuid.New()))
}

func TestResolveVisibleOnlineRedisErrorFailsClosed(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	uid := uuid.New()
	client := newTestClient(hub, uid)
	hub.userClients[uid] = map[uuid.UUID]bool{client.ID: true}

	require.NoError(t, redisClient.Set(context.Background(), fmt.Sprintf(presenceKeyFmt, uid), statusInvisible, 120*time.Second).Err())
	require.NoError(t, redisClient.Close())

	visible := hub.resolveVisibleOnline(map[uuid.UUID]bool{uid: true})
	assert.False(t, visible[uid])
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

	// CV-CAN-030: broadcasts are scoped to a client's server subscriptions.
	// Subscribe the client to a server that has no voice participants; the empty
	// voice state must still reach the subscribed client as a 0 count (a
	// drop-to-zero is delivered). Unsubscribed clients receive nothing — that is
	// the scoping this fix introduces.
	serverID := uuid.New()
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	hub.broadcastServerVoiceCounts()

	resp := readClientMsg(t, client)
	assert.Equal(t, "server_voice_counts", resp["type"])
	data, _ := resp["data"].(map[string]interface{})
	counts, _ := data["counts"].(map[string]interface{})
	assert.Equal(t, float64(0), counts[serverID.String()],
		"empty voice state reports 0 for the subscribed server")
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

// seedVoiceServer creates a server (owned by owner) with a voice channel that has
// one voice participant, and returns the server ID.
func seedVoiceServer(t *testing.T, db *sql.DB, owner uuid.UUID, name string) uuid.UUID {
	t.Helper()
	serverID := uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id, allow_embedded_content) VALUES ($1, $2, $3, true)`,
		serverID.String(), name, owner.String())
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`, serverID.String(), owner.String())
	require.NoError(t, err)
	channelID := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'voice', 'voice')`, channelID.String(), serverID.String())
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID.String(), owner.String())
	require.NoError(t, err)
	return serverID
}

func seedVoiceTestUser(t *testing.T, db *sql.DB, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec
	// Email is derived from the parameterized $2 in SQL (|| ), so no Go-side
	// string is composed into the query — fully parameterized.
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2 || '@test.concord.chat', $2, $3, true, true)`,
		id.String(), name, hash)
	require.NoError(t, err)
	return id
}

// TestSendVoiceCountsSnapshot_ExcludesNonMemberServers covers CV-CAN-030: the
// initial snapshot must not report voice activity for servers the user does not
// belong to.
func TestSendVoiceCountsSnapshot_ExcludesNonMemberServers(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	owner := seedVoiceTestUser(t, db, "vcsnapowner")
	member := seedVoiceTestUser(t, db, "vcsnapmember")
	serverA := seedVoiceServer(t, db, owner, "Snap A")
	serverB := seedVoiceServer(t, db, owner, "Snap B")
	// member belongs only to serverA.
	_, err := db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverA.String(), member.String())
	require.NoError(t, err)

	client := newTestClient(hub, member)
	hub.clients[client.ID] = client
	hub.sendVoiceCountsSnapshot(context.Background(), client)

	resp := readClientMsg(t, client)
	require.Equal(t, "server_voice_counts", resp["type"])
	counts := resp["data"].(map[string]interface{})[keyCounts].(map[string]interface{})
	assert.Contains(t, counts, serverA.String(), "member's server must be present")
	assert.NotContains(t, counts, serverB.String(), "non-member server must be absent")
}

// TestBroadcastServerVoiceCounts_ScopedToSubscribedServers covers CV-CAN-030: the
// periodic broadcast must send each client only the counts for servers it is
// subscribed to, not the full cross-server map.
func TestBroadcastServerVoiceCounts_ScopedToSubscribedServers(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	owner := seedVoiceTestUser(t, db, "vcbcowner")
	serverA := seedVoiceServer(t, db, owner, "BC A")
	serverB := seedVoiceServer(t, db, owner, "BC B")

	clientA := newTestClient(hub, seedVoiceTestUser(t, db, "vcbca"))
	clientB := newTestClient(hub, seedVoiceTestUser(t, db, "vcbcb"))
	hub.clients[clientA.ID] = clientA
	hub.clients[clientB.ID] = clientB
	hub.serverSubscriptions[serverA] = map[uuid.UUID]bool{clientA.ID: true}
	hub.serverSubscriptions[serverB] = map[uuid.UUID]bool{clientB.ID: true}

	hub.broadcastServerVoiceCounts()

	respA := readClientMsg(t, clientA)
	countsA := respA["data"].(map[string]interface{})[keyCounts].(map[string]interface{})
	assert.Contains(t, countsA, serverA.String())
	assert.NotContains(t, countsA, serverB.String(), "clientA must not see serverB's voice count")

	respB := readClientMsg(t, clientB)
	countsB := respB["data"].(map[string]interface{})[keyCounts].(map[string]interface{})
	assert.Contains(t, countsB, serverB.String())
	assert.NotContains(t, countsB, serverA.String(), "clientB must not see serverA's voice count")
}

// TestBroadcastServerVoiceCounts_SharedAndDistinctPayloads guards the per-broadcast
// payload memoization: two clients with an identical subscribed-server set must
// each receive the same full, correct counts (they share one marshaled payload via
// the cache), while a client with a different set must receive its own distinct
// counts (no cross-client payload bleed from the cache).
func TestBroadcastServerVoiceCounts_SharedAndDistinctPayloads(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	owner := seedVoiceTestUser(t, db, "vcshareowner")
	serverA := seedVoiceServer(t, db, owner, "Share A") // count = 1
	serverB := seedVoiceServer(t, db, owner, "Share B") // count = 1

	// share1 and share2 subscribe to the same set {A, B}; solo subscribes to {A}.
	share1 := newTestClient(hub, seedVoiceTestUser(t, db, "vcshare1"))
	share2 := newTestClient(hub, seedVoiceTestUser(t, db, "vcshare2"))
	solo := newTestClient(hub, seedVoiceTestUser(t, db, "vcsolo"))
	hub.clients[share1.ID] = share1
	hub.clients[share2.ID] = share2
	hub.clients[solo.ID] = solo
	hub.serverSubscriptions[serverA] = map[uuid.UUID]bool{share1.ID: true, share2.ID: true, solo.ID: true}
	hub.serverSubscriptions[serverB] = map[uuid.UUID]bool{share1.ID: true, share2.ID: true}

	hub.broadcastServerVoiceCounts()

	// Both shared-set clients get identical full counts for {A, B}.
	for _, c := range []*Client{share1, share2} {
		resp := readClientMsg(t, c)
		counts := resp["data"].(map[string]interface{})[keyCounts].(map[string]interface{})
		assert.Equal(t, float64(1), counts[serverA.String()])
		assert.Equal(t, float64(1), counts[serverB.String()])
		assert.Len(t, counts, 2, "shared-set client must see exactly its two subscribed servers")
	}

	// The distinct-set client must not receive the shared payload from the cache.
	resp := readClientMsg(t, solo)
	counts := resp["data"].(map[string]interface{})[keyCounts].(map[string]interface{})
	assert.Equal(t, float64(1), counts[serverA.String()])
	assert.NotContains(t, counts, serverB.String(), "solo client must not see serverB from a cached shared payload")
	assert.Len(t, counts, 1, "distinct-set client must see only its one subscribed server")
}

// TestHandleSubscribeServer_SendsSubscribedVoiceCounts covers CV-CAN-030:
// subscribing to a server must immediately deliver a voice-count snapshot so a
// client that subscribes after its initial snapshot cannot be left with a stale
// count for an event missed in the window before the subscription was recorded.
// The frontend replaces its voice-count map, so the catch-up must carry the
// client's full subscribed set, not just the newly added server. The count query
// runs off the Run loop and is handed back via voiceCountCatchupResults; this test
// applies that result the way Run() would.
func TestHandleSubscribeServer_SendsSubscribedVoiceCounts(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	owner := seedVoiceTestUser(t, db, "vcsubowner")
	serverA := seedVoiceServer(t, db, owner, "Sub A") // owner is a member + 1 voice participant
	serverB := seedVoiceServer(t, db, owner, "Sub B") // owner is a member + 1 voice participant

	client := newTestClient(hub, owner)
	hub.clients[client.ID] = client
	// Already subscribed to serverB before subscribing to serverA.
	hub.serverSubscriptions[serverB] = map[uuid.UUID]bool{client.ID: true}

	hub.handleSubscribeServer(IncomingMessage{
		Type:     "subscribe_server",
		UserID:   owner,
		ClientID: client.ID,
		Data:     map[string]interface{}{keyServerID: serverA.String()},
	})

	require.True(t, hub.serverSubscriptions[serverA][client.ID], "client must be subscribed to serverA")

	// The catch-up query is dispatched off the Run loop; apply its result on this
	// goroutine as Run() would, then assert the delivered snapshot.
	select {
	case c := <-hub.voiceCountCatchupResults:
		hub.applyVoiceCountCatchup(c)
	case <-time.After(2 * time.Second):
		t.Fatal("expected an off-loop voice-count catch-up result after subscribe")
	}

	resp := readClientMsg(t, client)
	require.Equal(t, "server_voice_counts", resp["type"])
	counts := resp["data"].(map[string]interface{})[keyCounts].(map[string]interface{})
	// The catch-up carries the full subscribed set (client replaces its map), so
	// both servers must be present, not just the newly subscribed one.
	assert.Equal(t, float64(1), counts[serverA.String()], "newly subscribed server's count present")
	assert.Equal(t, float64(1), counts[serverB.String()], "already-subscribed server's count retained")
}

// TestHandleSubscribeServer_SkipsCatchupForDuplicateSubscription covers CV-CAN-030
// hardening: a duplicate subscribe_server for a server the client already tracks
// opens no new staleness window, so it must not dispatch another voice-count
// catch-up. Otherwise an authenticated client could spam duplicate frames to force
// repeated full voice_participants aggregations (singleflight only collapses
// concurrent in-flight queries, not sequential ones).
func TestHandleSubscribeServer_SkipsCatchupForDuplicateSubscription(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	owner := seedVoiceTestUser(t, db, "vcdupowner")
	serverA := seedVoiceServer(t, db, owner, "Dup A")

	client := newTestClient(hub, owner)
	hub.clients[client.ID] = client
	// Client is already subscribed to serverA.
	hub.serverSubscriptions[serverA] = map[uuid.UUID]bool{client.ID: true}

	hub.handleSubscribeServer(IncomingMessage{
		Type:     "subscribe_server",
		UserID:   owner,
		ClientID: client.ID,
		Data:     map[string]interface{}{keyServerID: serverA.String()},
	})

	require.True(t, hub.serverSubscriptions[serverA][client.ID],
		"client must remain subscribed to serverA")

	// No catch-up must be dispatched for the duplicate subscription. The dispatch
	// path is skipped entirely, so nothing lands on voiceCountCatchupResults.
	select {
	case <-hub.voiceCountCatchupResults:
		t.Fatal("duplicate subscribe_server must not dispatch a voice-count catch-up")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestApplyVoiceCountCatchup_SkipsDisconnectedClient covers the race the off-loop
// dispatch introduces: if the client disconnects between subscribing and the
// query returning, applying the catch-up must be a no-op rather than panic or
// resurrect state.
func TestApplyVoiceCountCatchup_SkipsDisconnectedClient(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	owner := seedVoiceTestUser(t, db, "vccatchupgone")
	serverA := seedVoiceServer(t, db, owner, "Catchup A")

	client := newTestClient(hub, owner)
	// Client is NOT registered in hub.clients (simulates disconnect before the
	// off-loop query returned).
	hub.serverSubscriptions[serverA] = map[uuid.UUID]bool{client.ID: true}

	hub.applyVoiceCountCatchup(voiceCountCatchup{
		clientID: client.ID,
		counts:   map[string]int{serverA.String(): 1},
	})

	assert.Equal(t, 0, len(client.Send), "no snapshot should be sent to a disconnected client")
}

// TestEvictServerSubscriber_StopsVoiceCountFanout covers CV-CAN-030: once a
// member is removed/banned, evicting their server subscription (via the
// CV-CAN-027/028 BroadcastToServerAndPrune path) must stop the scoped
// voice-count fanout from reaching their still-connected client, without
// disturbing other subscribers.
func TestEvictServerSubscriber_StopsVoiceCountFanout(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	owner := seedVoiceTestUser(t, db, "vcrevowner")
	serverA := seedVoiceServer(t, db, owner, "Rev A") // owner is a voice participant (count=1)

	removedUser := seedVoiceTestUser(t, db, "vcrevremoved")
	removedClient := newTestClient(hub, removedUser)
	stayClient := newTestClient(hub, owner)
	hub.clients[removedClient.ID] = removedClient
	hub.clients[stayClient.ID] = stayClient
	hub.userClients[removedUser] = map[uuid.UUID]bool{removedClient.ID: true}
	hub.userClients[owner] = map[uuid.UUID]bool{stayClient.ID: true}
	hub.serverSubscriptions[serverA] = map[uuid.UUID]bool{
		removedClient.ID: true,
		stayClient.ID:    true,
	}

	// Simulate what member removal/ban triggers server-side: BroadcastToServerAndPrune
	// evicts the removed member from serverSubscriptions after delivering member_removed.
	hub.evictServerSubscriber(serverA, removedUser)

	assert.False(t, hub.serverSubscriptions[serverA][removedClient.ID],
		"removed user's connection must be dropped from the subscription set")
	assert.True(t, hub.serverSubscriptions[serverA][stayClient.ID],
		"other members' subscriptions must be untouched")

	// The next scoped broadcast skips the removed client but still reaches the
	// remaining subscriber.
	hub.broadcastServerVoiceCounts()
	assert.Equal(t, 0, len(removedClient.Send),
		"removed client must not receive voice counts for a server it left")

	resp := readClientMsg(t, stayClient)
	counts := resp["data"].(map[string]interface{})[keyCounts].(map[string]interface{})
	assert.Equal(t, float64(1), counts[serverA.String()],
		"remaining subscriber still receives the server's voice count")
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
	require.NoError(t, redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, userID), statusOnline, 120*time.Second).Err())

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

func TestHandleUnregisterDoesNotRetainHiddenPresenceWhenRedisDeleteFails(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleSetStatus(IncomingMessage{
		UserID: userID,
		Data:   map[string]interface{}{keyStatus: statusInvisible},
	})
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"del": errors.New("redis DEL failed")}})

	hub.handleUnregister(client)

	assert.NotContains(t, hub.hiddenPresence, userID)
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
	require.NoError(t, redisClient.Set(ctx, fmt.Sprintf(presenceKeyFmt, userID), statusOnline, 120*time.Second).Err())

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
