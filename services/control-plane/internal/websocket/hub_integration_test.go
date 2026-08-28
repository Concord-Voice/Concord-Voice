package websocket

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
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

func (c *testChannelPermissionChecker) HasChannelPermissionsUncached(
	ctx context.Context,
	serverID, userID, channelID string,
	permBits ...int64,
) (bool, error) {
	for _, permBit := range permBits {
		allowed, err := c.HasChannelPermission(ctx, serverID, userID, channelID, permBit)
		if err != nil || !allowed {
			return allowed, err
		}
	}
	return true, nil
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
	data := presenceFrameData(t, resp)
	assert.Equal(t, "nonce-123", data[keyNonce])
	assert.NotEmpty(t, data["id"])
}

func TestHandleMessageUsesFreshChannelPermissions(t *testing.T) {
	setup := setupMessageTest(t)
	setup.hub.SetChannelPermissionChecker(staleChannelPermissionChecker{})

	setup.hub.handleMessage(IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:  setup.convID,
			keyContent:    "must use fresh permissions",
			keyKeyVersion: float64(1),
		},
	})

	response := readClientMsg(t, setup.client)
	assert.Equal(t, "error", response["type"])
	var count int
	require.NoError(t, setup.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1`, setup.convID).Scan(&count))
	assert.Zero(t, count, "a stale permission cache must not authorize a message")
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
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

	// The audience query is async (#1654) and this hub never starts Run, so
	// nothing drains the result channel. Pump it. This also exercises the
	// apply-time hiddenPresence read: setHiddenPresence ran before the dispatch,
	// so the self frame below must still carry "invisible" rather than the
	// "offline" the audience receives.
	hub.applyPresenceAudience(<-hub.presenceAudienceResults)

	// The sender's own devices receive the committed status as the acknowledgement.
	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		assert.Equal(t, "presence", msg["type"])
		msgData := msg["data"].(map[string]interface{})
		assert.Equal(t, "invisible", msgData["status"])
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected presence broadcast")
	}
}

func TestHandleSetStatusInvisibleRedisWriteFailureLeavesPriorStatusUnchanged(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	logs := captureHubLog(t)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
	ctx := context.Background()
	key := presence.StatusRedisKey(userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 120*time.Second).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"set": errors.New("redis SET failed")}})
	var suppressions int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) { atomic.AddInt32(&suppressions, 1) })

	hub.handleSetStatus(IncomingMessage{
		ClientID: client.ID,
		UserID:   userID,
		Data:     map[string]interface{}{keyStatus: statusInvisible},
	})

	assert.Contains(t, logs.String(), "failed to persist presence status")
	assert.NotContains(t, hub.hiddenPresence, userID)
	assert.Equal(t, statusOnline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
	assert.Equal(t, statusOnline, hub.resolveVisibleStatus(ctx, userID, userID))
	assert.Contains(t, hub.resolveVisibleOnline(map[uuid.UUID]bool{userID: true}), userID)
	assert.Zero(t, atomic.LoadInt32(&suppressions))
	message := readClientMsg(t, client)
	assert.Equal(t, "error", message["type"])
	assert.Equal(t, "presence_status_unavailable", message["data"].(map[string]interface{})["code"])
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
	key := presence.StatusRedisKey(userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())

	hub.handleHeartbeat(IncomingMessage{Type: "heartbeat", UserID: userID})

	_, hidden := hub.hiddenPresence[userID]
	assert.False(t, hidden, "a valid persisted status should clear the transient fail-closed override")
	assert.Equal(t, statusOnline, hub.resolveVisibleStatus(ctx, userID, uuid.New()))
	assert.Contains(t, hub.resolveVisibleOnline(map[uuid.UUID]bool{userID: true}), userID)
	assert.Greater(t, redisClient.TTL(ctx, key).Val(), 100*time.Second)
}

// Regression for the production 1006 churn: every client heartbeat must be
// answered with an application-level heartbeat_ack DATA frame. The hub's WS
// protocol pings do not reliably count as activity across the Cloudflare
// edge's ~100s idle tracking, so without this echo the CF→client leg starves
// and the edge abruptly closes the socket (client sees close code 1006).
func TestHandlePresenceIncomingHeartbeatSendsAck(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	// Seed a persisted status so handleHeartbeat takes the quiet TTL-refresh
	// path and the ack is deterministically the only frame on client.Send.
	ctx := context.Background()
	require.NoError(t, redisClient.Set(ctx, presence.StatusRedisKey(userID), statusOnline, 120*time.Second).Err())

	hub.handlePresenceIncoming(IncomingMessage{Type: "heartbeat", UserID: userID, ClientID: client.ID})

	message := readClientMsg(t, client)
	assert.Equal(t, "heartbeat_ack", message["type"])
	assert.Equal(t, map[string]interface{}{}, message["data"])
}

// The ack must never block the hub loop or wedge a slow consumer further: a
// full send buffer drops the ack silently (real traffic is already flowing,
// which serves the keepalive purpose the ack exists for).
func TestHandlePresenceIncomingHeartbeatAckDropsOnFullBuffer(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	ctx := context.Background()
	require.NoError(t, redisClient.Set(ctx, presence.StatusRedisKey(userID), statusOnline, 120*time.Second).Err())

	for i := 0; i < cap(client.Send); i++ {
		client.Send <- []byte(`{"type":"filler","data":{}}`)
	}

	// Must return without blocking and without displacing queued frames.
	hub.handlePresenceIncoming(IncomingMessage{Type: "heartbeat", UserID: userID, ClientID: client.ID})

	assert.Len(t, client.Send, cap(client.Send))
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
			key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	stored, err := redisClient.Get(ctx, presence.StatusRedisKey(userID)).Result()
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
	stored, err := redisClient.Get(ctx, presence.StatusRedisKey(userID)).Result()
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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

	status, err := redisClient.Get(context.Background(), presence.StatusRedisKey(userID)).Result()
	require.NoError(t, err)
	assert.Equal(t, statusOffline, status)
}

func TestHandleIncomingDropsHeartbeatAfterUnregister(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	ctx := context.Background()
	key := presence.StatusRedisKey(userID)
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 60*time.Second).Err())
	redisClient.AddHook(commandErrorHook{failures: map[string]error{"del": errors.New("redis DEL failed")}})
	hub.handleUnregister(client)

	hub.handleIncoming(IncomingMessage{
		Type:     "heartbeat",
		ClientID: client.ID,
		UserID:   userID,
	})

	status, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, statusOffline, status)
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
		key := presence.StatusRedisKey(userID)
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

		stored, err := redisClient.Get(context.Background(), presence.StatusRedisKey(userID)).Result()
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

	_, err := redisClient.Get(context.Background(), presence.StatusRedisKey(otherUserID)).Result()
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	key := presence.StatusRedisKey(userID)
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
	data := presenceFrameData(t, resp)
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

// presenceFrameData extracts a decoded presence frame's data object.
//
// Comma-ok rather than a single-return type assertion, per [internal]rules/tests.md:
// a shape mismatch must fail THIS test, not panic and abort the entire package
// test binary — which would report every other test in the package as failed and
// bury the assertion that actually tripped in a stack trace.
//
// testhelpers.JSONField would be the standard tool, but this package's tests
// cannot import it: testhelpers imports websocket, so it would be a test import
// cycle (see the note above presenceTestUser).
//
// Introducing it swept every `data := <frame>["data"].(...)` site in this file,
// not only the ones this branch added. A handful of other single-return shapes
// remain (mostly the chained `resp["data"].(…)[keyCounts].(…)` form), so the
// file's entry in scripts/json-assertion-allowlist.txt is still live — finishing
// that sweep and deleting the entry is #2811's own PR, since the guard errors on
// a stale entry. What this helper guarantees is that no NEW ones are added.
func presenceFrameData(t *testing.T, frame map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, ok := frame["data"].(map[string]interface{})
	require.True(t, ok, "presence frame carries no data object: %#v", frame["data"])
	return data
}

// presenceFrameStatus reads a presence frame's status, comma-ok for the same
// reason as presenceFrameData: a missing or non-string status must fail this
// test, not panic and take the package binary down with it.
func presenceFrameStatus(t *testing.T, frame map[string]interface{}) string {
	t.Helper()
	status, ok := presenceFrameData(t, frame)[keyStatus].(string)
	require.True(t, ok, "presence frame carries no string status")
	return status
}

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
	// The audience query is async (#1654) and this hub never starts Run, so
	// nothing drains the result channel. Pump it synchronously: this drives the
	// real production path and keeps the stranger-leak assertion below
	// meaningful — without it that non-blocking select passes because the worker
	// has not finished, not because the leak is closed.
	hub.applyPresenceAudience(<-hub.presenceAudienceResults)

	resp := readClientMsg(t, friendClient)
	assert.Equal(t, "presence", resp["type"])
	data := presenceFrameData(t, resp)
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
	// Async since #1654; this hub never starts Run. See the pump note in
	// TestBroadcastPresenceToAll.
	hub.applyPresenceAudience(<-hub.presenceAudienceResults)

	resp := readClientMsg(t, peerClient)
	assert.Equal(t, "presence", resp["type"])
	assert.Equal(t, sender.String(), resp["data"].(map[string]interface{})[keyUserID])
}

// TestSpawnPresenceAudienceWorker_PostsResultWithGeneration locks the worker
// contract: it posts exactly one result, carrying back the generation it was
// handed, so the Run-side fence can compare it.
func TestSpawnPresenceAudienceWorker_PostsResultWithGeneration(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()
	want := map[uuid.UUID]bool{uuid.New(): true}
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		return want, nil
	}

	hub.spawnPresenceAudienceWorker(userID, statusOnline, 1234, 7)

	result := <-hub.presenceAudienceResults
	assert.Equal(t, userID, result.userID)
	assert.Equal(t, statusOnline, result.status)
	assert.Equal(t, int64(1234), result.timestamp)
	assert.Equal(t, uint64(7), result.generation)
	assert.Equal(t, want, result.audience)
	assert.NoError(t, result.err)
}

// TestSpawnPresenceAudienceWorker_PostsErrorAndNoAudience locks fail-closed at
// the worker boundary: a query error still posts, so Run can suppress and then
// re-dial, rather than leaving the user wedged in-flight forever.
func TestSpawnPresenceAudienceWorker_PostsErrorAndNoAudience(t *testing.T) {
	hub := NewHub(nil, nil)
	wantErr := errors.New("audience query exploded")
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		return nil, wantErr
	}

	hub.spawnPresenceAudienceWorker(uuid.New(), statusOnline, 1, 1)

	result := <-hub.presenceAudienceResults
	assert.ErrorIs(t, result.err, wantErr)
	assert.Nil(t, result.audience)
}

// TestComputePresenceAudience_CancelledSlotWaitFailsClosed proves an abandoned
// slot wait produces a suppressing error rather than an empty audience — an
// empty audience would silently deliver to nobody and read as success.
//
// It is named for the CONTEXT arm, not for slot exhaustion: the context is
// pre-cancelled, so that arm is taken whether or not a slot is free. Filling the
// only slot is what makes the outcome deterministic, and the t.Error inside the
// computer is what proves no query ran. The h.done arm has its own test.
func TestComputePresenceAudience_CancelledSlotWaitFailsClosed(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.presenceAudienceSlots = make(chan struct{}, 1)
	hub.presenceAudienceSlots <- struct{}{} // occupy the only slot; never released
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		t.Error("the computer must not run when no slot was acquired")
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	audience, err := hub.computePresenceAudience(ctx, uuid.New())

	require.Error(t, err)
	assert.Nil(t, audience, "a slot-acquire failure must not fabricate an empty audience")
}

// TestApplyPresenceAudience_DropsStaleGeneration is the privacy regression lock
// for #1654. An `online` computed before a later `offline` must never land after
// it — that renders a disconnected user online to their whole audience, and
// offline is a privacy state, not a cosmetic one.
func TestApplyPresenceAudience_DropsStaleGeneration(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	sender := presenceTestUser(t, db)
	friend := presenceTestUser(t, db)
	presenceTestFriendship(t, db, sender, friend)
	friendClient := &Client{ID: uuid.New(), UserID: friend, Send: make(chan []byte, 10)}
	hub.clients[friendClient.ID] = friendClient
	hub.userClients[friend] = map[uuid.UUID]bool{friendClient.ID: true}

	// Generation 2 is current; a result carrying generation 1 is superseded.
	hub.presenceGenCounter = 2
	hub.presenceGeneration[sender] = 2
	hub.presenceInFlight[sender] = struct{}{}

	hub.applyPresenceAudience(presenceAudienceResult{
		userID:     sender,
		status:     statusOnline,
		timestamp:  time.Now().Unix(),
		generation: 1,
		audience:   map[uuid.UUID]bool{friend: true},
	})

	select {
	case <-friendClient.Send:
		t.Fatal("a superseded presence result was delivered")
	default:
	}
}

// TestApplyPresenceAudience_SuppressesOnError locks fail-closed on the Run side.
//
// The #47 property — an errored result fans out NOTHING — holds at every point in
// the retry budget, and both subtests assert it. What the retry changed is the
// second half: an errored result no longer clears presenceInFlight immediately,
// because the marker is exactly what keeps the retry invisible to the coalescer.
// It must still clear once the budget is spent, or the user wedges forever, so
// that assertion moved from "on any error" to "on the last attempt" rather than
// being dropped.
func TestApplyPresenceAudience_SuppressesOnError(t *testing.T) {
	setup := func(t *testing.T) (*Hub, uuid.UUID, *Client) {
		t.Helper()
		db := setupHubTestDB(t)
		redisClient := setupHubTestRedis(t)
		hub := NewHub(db, redisClient)
		// Keep any retry off the real database; this test is about Run-side
		// bookkeeping, not about what a query does.
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			return nil, errors.New("audience query failed")
		}

		sender := presenceTestUser(t, db)
		friend := presenceTestUser(t, db)
		presenceTestFriendship(t, db, sender, friend)
		friendClient := &Client{ID: uuid.New(), UserID: friend, Send: make(chan []byte, 10)}
		hub.clients[friendClient.ID] = friendClient
		hub.userClients[friend] = map[uuid.UUID]bool{friendClient.ID: true}

		hub.presenceGenCounter = 1
		hub.presenceGeneration[sender] = 1
		hub.presenceInFlight[sender] = struct{}{}
		return hub, sender, friendClient
	}
	assertNoFanout := func(t *testing.T, friendClient *Client) {
		t.Helper()
		select {
		case <-friendClient.Send:
			t.Fatal("#47: presence was fanned out despite an audience-computation error")
		default:
		}
	}

	t.Run("a retryable error suppresses and holds the coalescer", func(t *testing.T) {
		hub, sender, friendClient := setup(t)

		hub.applyPresenceAudience(presenceAudienceResult{
			userID: sender, status: statusOnline, generation: 1,
			err: errors.New("audience query failed"),
		})

		assertNoFanout(t, friendClient)
		assert.Contains(t, hub.presenceInFlight, sender,
			"the marker must persist across a retry, or a transition arriving mid-retry "+
				"would dispatch a second concurrent worker for the same user")
	})

	t.Run("the last attempt suppresses and releases the coalescer", func(t *testing.T) {
		hub, sender, friendClient := setup(t)

		hub.applyPresenceAudience(presenceAudienceResult{
			userID: sender, status: statusOnline, generation: 1,
			attempt: presenceAudienceMaxAttempts - 1,
			err:     errors.New("audience query failed"),
		})

		assertNoFanout(t, friendClient)
		assert.NotContains(t, hub.presenceInFlight, sender,
			"an exhausted retry budget must clear in-flight or the user wedges forever")
	})
}

// TestApplyPresenceAudience_RedialsPendingTransition proves coalescing converges:
// a transition that arrived while a query was in flight is re-dispatched when
// that query lands, carrying the NEWEST status.
func TestApplyPresenceAudience_RedialsPendingTransition(t *testing.T) {
	hub := NewHub(nil, nil)
	sender := uuid.New()
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{}, nil
	}
	hub.presenceGenCounter = 2
	hub.presenceGeneration[sender] = 2
	hub.presenceInFlight[sender] = struct{}{}
	hub.presenceDispatchPending[sender] = pendingPresence{status: statusOffline, timestamp: 99}

	hub.applyPresenceAudience(presenceAudienceResult{
		userID: sender, status: statusOnline, generation: 1,
	})

	redialed := <-hub.presenceAudienceResults
	assert.Equal(t, statusOffline, redialed.status, "the re-dial must carry the newest status")
	assert.Equal(t, int64(99), redialed.timestamp)
	assert.Equal(t, uint64(2), redialed.generation)
	assert.NotContains(t, hub.presenceDispatchPending, sender)
}

// TestApplyPresenceAudience_DeletesGenerationWhenIdle keeps the map bounded: a
// user with nothing in flight and nothing pending leaves no entry behind.
func TestApplyPresenceAudience_DeletesGenerationWhenIdle(t *testing.T) {
	hub := NewHub(nil, nil)
	sender := uuid.New()
	hub.presenceGenCounter = 1
	hub.presenceGeneration[sender] = 1
	hub.presenceInFlight[sender] = struct{}{}

	hub.applyPresenceAudience(presenceAudienceResult{
		userID: sender, status: statusOnline, generation: 1,
		audience: map[uuid.UUID]bool{},
		// Stamped because #2992's fence reads a ZERO computedAt as infinitely
		// stale, which would send this result down the retry path and return
		// before the redial — leaking the very entries this test asserts are
		// deleted. Production cannot construct an unstamped result (there is one
		// construction site and it stamps), so this is a fixture requirement, not
		// a workaround. TestPresenceAudienceWithNoComputedAtIsUnproven pins the
		// zero behaviour itself.
		computedAt: time.Now(),
	})

	assert.NotContains(t, hub.presenceGeneration, sender)
	assert.NotContains(t, hub.presenceInFlight, sender)
}

// TestBroadcastPresenceToAll_CoalescesBurstToOneInFlightDispatch is the #1654
// coalescing lock. A burst of rapid transitions for ONE user must produce at
// most one outstanding dispatch: the coalescer keeps the NEWEST superseding
// transition (not the first one it saw), and every transition still bumps the
// hub-wide generation counter even though most never get their own dispatch.
// Draining the in-flight (now-stale) result and letting redialPresenceAudience
// fire must carry the newest status through to the re-dialed worker.
//
// This is deterministic, not merely "usually passes": presenceInFlight is
// cleared ONLY by applyPresenceAudience/redialPresenceAudience, and nothing on
// this goroutine calls either until after the whole burst is issued. So no
// matter how fast the first spawned worker goroutine runs, it cannot cause a
// second dispatch mid-burst — only the explicit drain below can.
func TestBroadcastPresenceToAll_CoalescesBurstToOneInFlightDispatch(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.db = &sql.DB{} // reach the dispatch path without touching a real database
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{}, nil
	}
	sender := uuid.New()

	hub.broadcastPresenceToAll(sender, statusOnline, 1)
	for i := int64(2); i <= 10; i++ {
		hub.broadcastPresenceToAll(sender, statusDND, i)
	}
	hub.broadcastPresenceToAll(sender, statusOffline, 11)

	assert.Len(t, hub.presenceInFlight, 1, "a burst for one user must coalesce to one outstanding dispatch")
	assert.Contains(t, hub.presenceInFlight, sender)
	assert.Equal(t, pendingPresence{status: statusOffline, timestamp: 11}, hub.presenceDispatchPending[sender],
		"the coalescer must retain the NEWEST superseding transition, not the first")
	assert.Equal(t, uint64(11), hub.presenceGeneration[sender],
		"every transition must bump the generation even when coalesced away")

	// Drain the in-flight (superseded, generation 1) result; applyPresenceAudience
	// drops it on the generation fence and then redials the coalesced pending
	// transition.
	hub.applyPresenceAudience(<-hub.presenceAudienceResults)

	redialed := <-hub.presenceAudienceResults
	assert.Equal(t, statusOffline, redialed.status, "the re-dial must carry the newest coalesced status")
	assert.Equal(t, int64(11), redialed.timestamp)
	assert.Equal(t, uint64(11), redialed.generation)
	assert.NotContains(t, hub.presenceDispatchPending, sender)
}

// TestBroadcastPresenceToAll_BurstDuringBootstrapDeliversWithoutOverflowDisconnect
// covers the interaction #1654 changed: WHEN a presence frame reaches a viewer
// relative to completeClientBootstrap. Before #1654 every transition fanned out
// synchronously and inline; now a burst coalesces to (at most) one dispatch in
// flight plus one re-dial, so strictly fewer frames are ever produced for the
// same burst. This test drives an 11-transition burst for a sender while a
// viewer's reconnect replacement is active (client.beginBootstrap()), then
// proves that reduction holds where it is observable from the client's side:
// the viewer is never disconnected for a bootstrap-buffer overflow (asserted
// via the customTextClientDisconnect seam, not a private field), and the
// surviving frame is actually delivered once the bootstrap completes.
//
// Like the coalescing test above, this is deterministic rather than a
// probabilistic burst: exactly two dispatches are ever spawned for this sender
// (the initial one, and the one redialPresenceAudience issues for the coalesced
// pending transition), so draining exactly two results delivers exactly the
// frames the state machine guarantees regardless of worker goroutine timing.
func TestBroadcastPresenceToAll_BurstDuringBootstrapDeliversWithoutOverflowDisconnect(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.db = &sql.DB{}
	sender := uuid.New()
	viewer := uuid.New()
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		return map[uuid.UUID]bool{viewer: true}, nil
	}

	viewerClient := &Client{ID: uuid.New(), UserID: viewer, Send: make(chan []byte, 8), Hub: hub}
	hub.clients[viewerClient.ID] = viewerClient
	hub.userClients[viewer] = map[uuid.UUID]bool{viewerClient.ID: true}

	disconnected := false
	hub.customTextClientDisconnect = func(*Client) error {
		disconnected = true
		return nil
	}

	viewerClient.beginBootstrap() // viewer is mid-reconnect-replacement

	hub.broadcastPresenceToAll(sender, statusOnline, 1)
	for i := int64(2); i <= 10; i++ {
		hub.broadcastPresenceToAll(sender, statusDND, i)
	}
	hub.broadcastPresenceToAll(sender, statusOffline, 11)

	// Drain both dispatches the burst can ever produce: the initial (stale,
	// suppressed-by-generation-fence) one and the redialed coalesced one.
	hub.applyPresenceAudience(<-hub.presenceAudienceResults)
	hub.applyPresenceAudience(<-hub.presenceAudienceResults)

	assert.False(t, disconnected,
		"a coalesced burst must not overflow the bootstrap live buffer and disconnect the viewer")

	require.True(t, hub.completeClientBootstrap(viewerClient, []byte(`{"type":"presence_snapshot"}`)))
	assert.Equal(t, []byte(`{"type":"presence_snapshot"}`), <-viewerClient.Send)

	frame := <-viewerClient.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(frame, &msg))
	assert.Equal(t, "presence", msg["type"])
	data := presenceFrameData(t, msg)
	assert.Equal(t, sender.String(), data[keyUserID])
	assert.Equal(t, statusOffline, data[keyStatus],
		"the surviving (generation-fence-winning) frame must carry the newest coalesced status")

	select {
	case extra := <-viewerClient.Send:
		t.Fatalf("more frames arrived than the two coalesced dispatches can produce: %s", extra)
	default:
	}
}

// TestPresenceAudienceQueueWaitIsNotChargedToTheQueryDeadline is the privacy
// regression lock for the red-team F3 finding on #1654.
//
// presenceAudienceTimeout must bound the QUERY only, measured from slot
// acquisition. When one budget covered both the queue wait and the query, a
// transition that merely waited behind a peer returned DeadlineExceeded without
// ever reaching the database, and deliverPresenceAudienceResult suppressed it as
// though the query had failed.
//
// That loss is PERMANENT, not delayed: redialPresenceAudience re-dials only when
// a newer transition is pending, and the worst case — handleUnregister's offline
// frame — has none by construction, because the user has disconnected. Their
// audience renders them online forever. This test therefore holds a slot for
// longer than the whole timeout and asserts the queued transition still
// succeeds.
//
// It deliberately costs ~presenceAudienceTimeout of wall clock: the defect is a
// deadline, so the elapsed time IS the subject. Do not "speed it up" by
// shrinking the hold below the timeout — that makes it pass against the broken
// code.
func TestPresenceAudienceQueueWaitIsNotChargedToTheQueryDeadline(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.db = &sql.DB{} // reach the dispatch path; the computer intercepts first

	// One slot, so the second transition provably queues behind the first.
	hub.presenceAudienceSlots = make(chan struct{}, 1)

	holding := make(chan struct{})
	var claimed atomic.Bool
	hub.presenceAudienceComputer = func(_ context.Context, _ uuid.UUID) (map[uuid.UUID]bool, error) {
		if !claimed.CompareAndSwap(false, true) {
			return map[uuid.UUID]bool{}, nil
		}
		// Occupy the only slot for longer than the entire query budget, so a
		// shared budget would expire the queued worker while it waits.
		//
		// Deliberately NOT selecting on ctx.Done(): releasing there would free the
		// slot at exactly presenceAudienceTimeout — the deadline this test claims
		// to outlast — so a regressed shared-budget implementation could hand the
		// slot to the queued worker and let this fake's second invocation return
		// success. The test would then pass against the very bug it guards.
		close(holding)
		<-time.After(presenceAudienceTimeout + 500*time.Millisecond)
		return map[uuid.UUID]bool{}, nil
	}

	blocker, queued := uuid.New(), uuid.New()
	hub.broadcastPresenceToAll(blocker, statusOnline, 1)
	<-holding // the slot is now held
	hub.broadcastPresenceToAll(queued, statusOffline, 2)

	results := map[uuid.UUID]presenceAudienceResult{}
	for range 2 {
		result := <-hub.presenceAudienceResults
		results[result.userID] = result
	}

	require.Contains(t, results, queued)
	assert.NoError(t, results[queued].err,
		"a transition that merely queued behind a peer must not be reported as a failure")
	assert.NotNil(t, results[queued].audience,
		"a queued transition must reach the database, not be suppressed fail-closed")
	assert.Equal(t, statusOffline, results[queued].status,
		"the offline frame is the terminal case: nothing re-dials it if it is dropped")
}

// TestShutdownOfflineSurvivesSaturatedAudienceSlots is the P1 regression lock
// from the Codex review of PR #2975.
//
// The shutdown fan-out is the one frame that CANNOT be repaired: the user has
// disconnected, so no later transition follows it. Routing it through the
// presenceAudienceSlots semaphore made it lose a race it must never enter — with
// all four slots held by async workers, a shutdown transition would spend its
// budget queueing and be suppressed as though the database had failed.
//
// broadcastPresenceToAllSync therefore bypasses the semaphore entirely. This
// test saturates every slot and asserts the offline frame still reaches the
// audience, synchronously, with no pump.
func TestShutdownOfflineSurvivesSaturatedAudienceSlots(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	// Occupy every audience slot and never release them.
	for range presenceAudienceConcurrency {
		hub.presenceAudienceSlots <- struct{}{}
	}

	sender := presenceTestUser(t, db)
	friend := presenceTestUser(t, db)
	presenceTestFriendship(t, db, sender, friend)

	senderClient := &Client{ID: uuid.New(), UserID: sender, Send: make(chan []byte, 10)}
	friendClient := &Client{ID: uuid.New(), UserID: friend, Send: make(chan []byte, 10)}
	hub.clients[senderClient.ID] = senderClient
	hub.clients[friendClient.ID] = friendClient
	hub.userClients[sender] = map[uuid.UUID]bool{senderClient.ID: true}
	hub.userClients[friend] = map[uuid.UUID]bool{friendClient.ID: true}

	hub.transitionUserOffline(context.Background(), sender, true /* allowStopping */)

	resp := readClientMsg(t, friendClient)
	assert.Equal(t, "presence", resp["type"])
	data := presenceFrameData(t, resp)
	assert.Equal(t, sender.String(), data[keyUserID])
	assert.Equal(t, statusOffline, data[keyStatus],
		"the terminal offline frame must not be lost to a saturated semaphore")
}

// TestShutdownFansOutOfflineSynchronously locks the shutdown seam. Run is
// exiting, so an async dispatch would post to a channel nothing drains and the
// final offline frame would be lost — a user left rendered online to their
// audience until their next transition, which for a shutting-down hub is never.
func TestShutdownFansOutOfflineSynchronously(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	sender := presenceTestUser(t, db)
	friend := presenceTestUser(t, db)
	presenceTestFriendship(t, db, sender, friend)

	senderClient := &Client{ID: uuid.New(), UserID: sender, Send: make(chan []byte, 10)}
	friendClient := &Client{ID: uuid.New(), UserID: friend, Send: make(chan []byte, 10)}
	hub.clients[senderClient.ID] = senderClient
	hub.clients[friendClient.ID] = friendClient
	hub.userClients[sender] = map[uuid.UUID]bool{senderClient.ID: true}
	hub.userClients[friend] = map[uuid.UUID]bool{friendClient.ID: true}

	hub.transitionUserOffline(context.Background(), sender, true /* allowStopping */)

	// No pump: the frame must already be enqueued when the call returns.
	resp := readClientMsg(t, friendClient)
	assert.Equal(t, "presence", resp["type"])
	data := presenceFrameData(t, resp)
	assert.Equal(t, sender.String(), data[keyUserID])
	assert.Equal(t, statusOffline, data[keyStatus])

	select {
	case r := <-hub.presenceAudienceResults:
		t.Fatalf("shutdown dispatched asynchronously: %+v", r)
	default:
	}
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

	_, err = setup.db.Exec(`INSERT INTO key_revocations (
		channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
		rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
	) VALUES ($1, 1, 2, 'test', $2, $2, TRUE, $3)`,
		channelUUID.String(), setup.user1.String(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	require.NoError(t, err)

	tx, err := setup.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			t.Errorf("rollback epoch-revoked test transaction: %v", rbErr)
		}
	}()
	_, err = tx.Exec(`SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, setup.user1.String())
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 2)`,
		channelUUID.String(), setup.user1.String(), []byte("test-key-v2"))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = setup.db.Exec(`DELETE FROM channel_keys WHERE channel_id = $1 AND user_id = $2 AND key_version = 2`,
		channelUUID.String(), setup.user1.String())
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
	data := presenceFrameData(t, resp)
	assert.Equal(t, "epoch_revoked", data["code"])
	assert.Equal(t, float64(2), data["current_epoch"])
}

func TestEnforceWSEpoch_CurrentEpochLookupFailureFailsClosed(t *testing.T) {
	setup := setupMessageTest(t)
	channelUUID, _ := uuid.Parse(setup.convID)

	_, err := setup.db.Exec(`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'test', $2)`, channelUUID.String(), setup.user1.String())
	require.NoError(t, err)
	_, err = setup.db.Exec(`ALTER TABLE channel_keys RENAME TO channel_keys_epoch_lookup_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, revertErr := setup.db.Exec(`ALTER TABLE channel_keys_epoch_lookup_test RENAME TO channel_keys`)
		require.NoError(t, revertErr)
	})

	msg := IncomingMessage{ClientID: setup.client.ID}
	assert.False(t, setup.hub.enforceWSEpoch(msg, channelUUID, setup.convID, 1))

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := presenceFrameData(t, resp)
	assert.Equal(t, "Failed to verify key epoch", data["message"])
	assert.NotContains(t, data, "current_epoch")
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
	require.NoError(t, redisClient.Set(ctx, presence.StatusRedisKey(uid), statusOnline, 120*time.Second).Err())

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
	require.NoError(t, redisClient.Set(ctx, presence.StatusRedisKey(uid), statusInvisible, 120*time.Second).Err())

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

	require.NoError(t, redisClient.Set(context.Background(), presence.StatusRedisKey(uid), statusInvisible, 120*time.Second).Err())
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
	require.NoError(t, redisClient.Set(context.Background(), presence.StatusRedisKey(uid), "corrupt", 120*time.Second).Err())

	assert.Equal(t, statusOffline, hub.resolveVisibleStatus(context.Background(), uid, uuid.New()))
}

func TestResolveVisibleOnlineRedisErrorFailsClosed(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	uid := uuid.New()
	client := newTestClient(hub, uid)
	hub.userClients[uid] = map[uuid.UUID]bool{client.ID: true}

	require.NoError(t, redisClient.Set(context.Background(), presence.StatusRedisKey(uid), statusInvisible, 120*time.Second).Err())
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
	require.NoError(t, redisClient.Set(ctx, presence.StatusRedisKey(userID), statusOnline, 120*time.Second).Err())

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
	require.NoError(t, redisClient.Set(ctx, presence.StatusRedisKey(userID), statusOnline, 120*time.Second).Err())

	// Mark user as pending
	hub.onlineCountPending[userID] = true

	hub.flushOnlineCounts()

	// Should receive server_online_counts
	resp := readClientMsg(t, client)
	assert.Equal(t, "server_online_counts", resp["type"])
	data := presenceFrameData(t, resp)
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
	data := presenceFrameData(t, resp)
	assert.Contains(t, data[keyMessage], "maximum length")
}

// TestComputePresenceAudience_HubStoppingAbandonsSlotWait covers the h.done arm
// of the slot select — the fail-closed shutdown path, which had no test.
//
// Deterministic by construction: the only slot is occupied and never released,
// and the caller's context is never cancelled, so h.done is the single ready
// case in the select.
func TestComputePresenceAudience_HubStoppingAbandonsSlotWait(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.presenceAudienceSlots = make(chan struct{}, 1)
	hub.presenceAudienceSlots <- struct{}{} // occupy the only slot; never released
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		t.Error("the computer must not run when no slot was acquired")
		return nil, nil
	}
	close(hub.done)

	audience, err := hub.computePresenceAudience(context.Background(), uuid.New())

	require.ErrorIs(t, err, errPresenceHubStopping,
		"an abandoned queue wait must stay distinguishable from a database failure, "+
			"or every graceful shutdown logs a spurious suppression error")
	assert.Nil(t, audience, "an abandoned wait must not fabricate an empty audience")
}

// TestShutdownClientsJoinsInFlightPresenceAudienceWorkers pins worker lifetime
// against shutdown. It fails if EITHER half of the join is removed — the
// Add/Done pair in spawnPresenceAudienceWorker, or the Wait in shutdownClients —
// which is the point: without both, up to presenceAudienceConcurrency queries
// outlive Shutdown holding pool connections while cmd/server closes the database
// underneath them.
//
// The non-return assertion is gated on the presenceAudienceJoinReached SEAM, not
// on a timer. An earlier version simply asserted that shutdownClients had not
// returned within 50ms, which cannot distinguish "parked at the join" from "not
// yet scheduled" — on a loaded runner, deleting the join outright would have left
// it green. The seam makes the precondition observable, so the assertion that
// follows is about the join and nothing else.
func TestShutdownClientsJoinsInFlightPresenceAudienceWorkers(t *testing.T) {
	hub := NewHub(nil, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	atJoin := make(chan struct{})
	hub.presenceAudienceJoinReached = func() { close(atJoin) }
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		close(entered)
		<-release
		return map[uuid.UUID]bool{}, nil
	}

	hub.spawnPresenceAudienceWorker(uuid.New(), statusOnline, 1, 1)
	<-entered // the worker now holds a slot and is inside the query

	returned := make(chan struct{})
	go func() {
		hub.shutdownClients()
		close(returned)
	}()

	select {
	case <-atJoin:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdownClients never reached the presence-worker join")
	}

	// What the seam buys, stated precisely. Proving a negative — "it does not
	// return" — needs a bounded wait; there is no event for the absence of one.
	// What the seam removes is the ambiguity that made the earlier version
	// unsound: a bare window could not distinguish "parked at the join" from "the
	// shutdown goroutine has not been scheduled yet", so on a loaded runner
	// deleting the join left it green. Now the wait starts from a KNOWN position —
	// execution is past the seam and inside the join — where a build without an
	// effective join returns within microseconds. A zero-width check is not
	// sufficient either: the mutant still has to be scheduled to close(returned),
	// and checking instantly passed against both mutants.
	select {
	case <-returned:
		t.Fatal("shutdownClients returned past the join while a presence audience " +
			"worker was still querying; the worker would outlive Shutdown and hold a " +
			"pool connection through database close")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdownClients did not return after the worker completed")
	}
}

// recvPresenceResult takes one audience result, failing fast if none arrives.
// A bare receive here would turn a missing retry — the exact defect these tests
// exist to catch — into a package-wide test-binary timeout with no useful
// message, which is what it did before this helper existed.
func recvPresenceResult(t *testing.T, hub *Hub) presenceAudienceResult {
	t.Helper()
	select {
	case result := <-hub.presenceAudienceResults:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("no audience result arrived; a retry that should have been " +
			"dispatched was not")
		return presenceAudienceResult{}
	}
}

// TestPresenceAudienceRetriesFailedQueryThenSuppresses covers the terminal-frame
// repair path. A failed audience query drops a frame that may be TERMINAL — a
// disconnecting or newly-invisible user produces no later transition, so without
// a retry every viewer renders them online for the rest of their own session.
//
// The two assertions are a pair on purpose: the attempt count proves the retry
// happens, and the cleared presenceInFlight proves it still terminates. A retry
// that never gave up would wedge the coalescer for that user permanently.
func TestPresenceAudienceRetriesFailedQueryThenSuppresses(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.db = &sql.DB{} // never queried; presenceAudienceComputer intercepts first
	var attempts atomic.Int32
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		attempts.Add(1)
		return nil, errors.New("connection pool exhausted")
	}

	user := uuid.New()
	hub.broadcastPresenceToAll(user, statusOffline, 1)
	for range presenceAudienceMaxAttempts {
		hub.applyPresenceAudience(recvPresenceResult(t, hub))
	}

	assert.Equal(t, int32(presenceAudienceMaxAttempts), attempts.Load(),
		"a failed query must be retried up to the cap, and never past it")
	assert.NotContains(t, hub.presenceInFlight, user,
		"the coalescer must be released once the retry budget is spent")
	assert.NotContains(t, hub.presenceGeneration, user)
}

// TestPresenceAudienceRetrySucceedsAndDelivers proves the retry actually repairs
// the frame rather than merely re-running: the first attempt fails, the second
// succeeds, and the transition is delivered carrying its ORIGINAL status and
// timestamp.
func TestPresenceAudienceRetrySucceedsAndDelivers(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.db = &sql.DB{}
	var attempts atomic.Int32
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("connection pool exhausted")
		}
		return map[uuid.UUID]bool{}, nil
	}

	user := uuid.New()
	client := &Client{ID: uuid.New(), UserID: user, Send: make(chan []byte, 10)}
	hub.clients[client.ID] = client
	hub.userClients[user] = map[uuid.UUID]bool{client.ID: true}

	hub.broadcastPresenceToAll(user, statusOffline, 4242)
	hub.applyPresenceAudience(recvPresenceResult(t, hub)) // first: fails, retries
	hub.applyPresenceAudience(recvPresenceResult(t, hub)) // second: succeeds

	assert.Equal(t, int32(2), attempts.Load())
	resp := readClientMsg(t, client)
	assert.Equal(t, "presence", resp["type"])
	data := presenceFrameData(t, resp)
	assert.Equal(t, user.String(), data[keyUserID])
	assert.Equal(t, statusOffline, data[keyStatus],
		"the repaired frame must carry the original transition, not a fresh one")
}

// TestPresenceAudienceRetryIsBoundedByTheFence proves the retry cannot outrun the
// two things that must always beat it: a newer transition, and shutdown.
//
// The assertion is on presenceInFlight, NOT on a timing window. An earlier version
// watched presenceAudienceResults for 50ms and was VACUOUS in both subtests: a
// retry worker backs off presenceAudienceRetryBackoff (250ms) before it touches
// the computer or posts anything, so a spawned retry could not be observed inside
// the window and both mutants — hoisting the retry above the fence, and dropping
// the errPresenceHubStopping guard — went green. presenceInFlight discriminates
// with no clock: a retry returns early and LEAVES the marker set, while every
// non-retry path falls through to redialPresenceAudience, which clears it.
func TestPresenceAudienceRetryIsBoundedByTheFence(t *testing.T) {
	newHub := func(t *testing.T) *Hub {
		t.Helper()
		hub := NewHub(nil, nil)
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			t.Error("no retry worker should have been spawned")
			return nil, nil
		}
		return hub
	}
	// Secondary to the presenceInFlight assertion, and deliberately longer than
	// presenceAudienceRetryBackoff so it can actually observe a spawned retry.
	assertNoWorker := func(t *testing.T, hub *Hub, user uuid.UUID) {
		t.Helper()
		assert.NotContains(t, hub.presenceInFlight, user,
			"the coalescer must be released; a marker still set means a retry was dispatched")
		select {
		case <-hub.presenceAudienceResults:
			t.Fatal("a retry worker was spawned")
		case <-time.After(2 * presenceAudienceRetryBackoff):
		}
	}

	t.Run("a superseded failure is dropped, not retried", func(t *testing.T) {
		hub := newHub(t)
		user := uuid.New()
		hub.presenceGeneration[user] = 9
		hub.presenceInFlight[user] = struct{}{}

		hub.applyPresenceAudience(presenceAudienceResult{
			userID: user, generation: 7, err: errors.New("connection pool exhausted"),
		})

		assertNoWorker(t, hub, user)
	})

	t.Run("an abandoned shutdown wait is not retried", func(t *testing.T) {
		hub := newHub(t)
		user := uuid.New()
		hub.presenceGeneration[user] = 3
		hub.presenceInFlight[user] = struct{}{}

		hub.applyPresenceAudience(presenceAudienceResult{
			userID: user, generation: 3, err: errPresenceHubStopping,
		})

		assertNoWorker(t, hub, user)
	})
}

// TestPresenceAudienceSuppressionIsCounted proves the ops counter fires on the
// path that drops a presence frame, and stays silent on the path that delivers
// one. Both halves matter: a counter that never fires is invisible, and one that
// fires on success would make the suppression rate meaningless.
//
// The counter exists because #1654's fail-closed branch is a PERMANENT loss for
// a terminal transition and was otherwise visible only in a log line, while the
// semaphore constant is deliberately not env-tunable on the grounds that an
// operator cannot see the budget. This is what makes it visible.
func TestPresenceAudienceSuppressionIsCounted(t *testing.T) {
	newCountedHub := func() (*Hub, *opsCounterSpy) {
		hub := NewHub(nil, nil)
		counter := &opsCounterSpy{}
		hub.opsCounter = counter
		return hub, counter
	}

	t.Run("a spent retry budget increments once", func(t *testing.T) {
		hub, counter := newCountedHub()
		user := uuid.New()
		hub.presenceGeneration[user] = 1
		hub.presenceInFlight[user] = struct{}{}

		hub.applyPresenceAudience(presenceAudienceResult{
			userID: user, status: statusOffline, generation: 1,
			attempt: presenceAudienceMaxAttempts - 1,
			err:     errors.New("connection pool exhausted"),
		})

		assert.Equal(t, 1, counter.count(opsmetrics.MetricPresenceAudienceSuppressedTotal),
			"a dropped terminal frame must be countable, not log-only")
	})

	t.Run("a retryable failure does not increment yet", func(t *testing.T) {
		hub, counter := newCountedHub()
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			return nil, errors.New("connection pool exhausted")
		}
		user := uuid.New()
		hub.presenceGeneration[user] = 1
		hub.presenceInFlight[user] = struct{}{}

		hub.applyPresenceAudience(presenceAudienceResult{
			userID: user, status: statusOffline, generation: 1,
			err: errors.New("connection pool exhausted"),
		})

		assert.Equal(t, 0, counter.count(opsmetrics.MetricPresenceAudienceSuppressedTotal),
			"a frame still being retried has not been suppressed; counting it here "+
				"would report up to presenceAudienceMaxAttempts losses for one transition")
	})

	t.Run("a successful fan-out does not increment", func(t *testing.T) {
		hub, counter := newCountedHub()
		user := uuid.New()
		hub.presenceGeneration[user] = 1
		hub.presenceInFlight[user] = struct{}{}

		hub.applyPresenceAudience(presenceAudienceResult{
			userID: user, status: statusOnline, generation: 1,
			audience: map[uuid.UUID]bool{},
		})

		assert.Equal(t, 0, counter.count(opsmetrics.MetricPresenceAudienceSuppressedTotal))
	})

	// The #2992 fence added a FOURTH drop path and it was the only uncounted one,
	// so the counter under-reported precisely the behaviour the fence introduced —
	// and that drop is the one an operator most needs to see, because a revocation
	// or handoff-bound suppression is a different condition from a database failure
	// and is invisible at any useful aggregate in the logs. Caught by CodeRabbit.
	t.Run("an unproven audience with a spent budget increments", func(t *testing.T) {
		hub, counter := newCountedHub()
		user := uuid.New()
		hub.presenceGeneration[user] = 1
		hub.presenceInFlight[user] = struct{}{}

		// Stamped, then revoked: unproven via the EPOCH arm, with no retries left,
		// so it reaches delivery and must be suppressed there.
		result := presenceAudienceResult{
			userID: user, status: statusOnline, generation: 1,
			audience:   map[uuid.UUID]bool{uuid.New(): true},
			authzEpoch: hub.presenceAuthzEpoch.Load(),
			computedAt: time.Now(),
			attempt:    uint8(presenceAudienceMaxAttempts),
		}
		hub.InvalidatePresenceAudiences()
		require.True(t, hub.presenceAudienceUnproven(result),
			"positive control: the result must actually BE unproven, or this arm "+
				"passes for the wrong reason")

		hub.applyPresenceAudience(result)

		assert.Equal(t, 1, counter.count(opsmetrics.MetricPresenceAudienceSuppressedTotal),
			"a fence suppression is a dropped frame like any other and must be counted")
	})
}

// TestPresenceFrontierDropsDeltaDispatchedBeforeRegistration locks the
// registration frontier, and its control arm is what proves the frontier is
// closing a REGRESSION rather than enforcing a pre-existing property.
//
// Moving the audience query off Run opened a window between a delta being
// dispatched and being delivered. A client registering inside that window
// receives a bootstrap snapshot carrying the sender's current state and then the
// older in-flight delta on top of it. The pre-#1654 synchronous path cannot
// produce this: it fans out before returning, so the viewer simply is not there
// yet and nothing is sent.
//
// Found by @red-team (A1); reproduced independently against this branch before
// the fix was applied — async delivered [online], the sync control delivered
// nothing.
func TestPresenceFrontierDropsDeltaDispatchedBeforeRegistration(t *testing.T) {
	collect := func(t *testing.T, client *Client) []string {
		t.Helper()
		var got []string
		for {
			select {
			case raw := <-client.Send:
				var frame map[string]interface{}
				require.NoError(t, json.Unmarshal(raw, &frame))
				if frame["type"] == "presence" {
					got = append(got, presenceFrameStatus(t, frame))
				}
			case <-time.After(150 * time.Millisecond):
				return got
			}
		}
	}

	t.Run("async delta predating registration is dropped", func(t *testing.T) {
		db := setupHubTestDB(t)
		hub := NewHub(db, setupHubTestRedis(t))
		sender := presenceTestUser(t, db)
		viewer := presenceTestUser(t, db)
		presenceTestFriendship(t, db, sender, viewer)

		entered, release := make(chan struct{}), make(chan struct{})
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			close(entered)
			<-release
			return map[uuid.UUID]bool{viewer: true}, nil
		}

		hub.broadcastPresenceToAll(sender, statusOnline, 1)
		<-entered // the worker is mid-query and the viewer does not exist yet

		viewerClient := &Client{ID: uuid.New(), UserID: viewer, Send: make(chan []byte, 20)}
		hub.registerClient(viewerClient) // stamps the frontier

		close(release)
		hub.applyPresenceAudience(<-hub.presenceAudienceResults)

		assert.Empty(t, collect(t, viewerClient),
			"a delta dispatched before this client registered must not land on top of "+
				"the snapshot it already received")
	})

	t.Run("a delta dispatched after registration still arrives", func(t *testing.T) {
		db := setupHubTestDB(t)
		hub := NewHub(db, setupHubTestRedis(t))
		sender := presenceTestUser(t, db)
		viewer := presenceTestUser(t, db)
		presenceTestFriendship(t, db, sender, viewer)
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			return map[uuid.UUID]bool{viewer: true}, nil
		}

		viewerClient := &Client{ID: uuid.New(), UserID: viewer, Send: make(chan []byte, 20)}
		hub.registerClient(viewerClient)

		hub.broadcastPresenceToAll(sender, statusOnline, 1)
		hub.applyPresenceAudience(<-hub.presenceAudienceResults)

		assert.Equal(t, []string{statusOnline}, collect(t, viewerClient),
			"the frontier must not swallow deltas newer than the client — that would "+
				"trade an ordering defect for a delivery one")
	})

	// The case the other three arms do NOT reach: above
	// presenceSnapshotConnectedLimit the snapshot omits senders fail-closed, and a
	// frontier that filtered unconditionally would drop their in-flight delta too,
	// leaving the viewer rendering them offline until some later transition.
	//
	// Found by Codex on the frontier commit itself. Uses the seam directly rather
	// than standing up 512 real clients: the property under test is "a sender the
	// snapshot omitted is exempt from the frontier", and the snapshot's own
	// truncation is covered by capturePresenceSnapshotSeed's tests.
	t.Run("a sender the snapshot omitted is exempt from the frontier", func(t *testing.T) {
		db := setupHubTestDB(t)
		hub := NewHub(db, setupHubTestRedis(t))
		sender := presenceTestUser(t, db)
		viewer := presenceTestUser(t, db)
		presenceTestFriendship(t, db, sender, viewer)

		entered, release := make(chan struct{}), make(chan struct{})
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			close(entered)
			<-release
			return map[uuid.UUID]bool{viewer: true}, nil
		}

		hub.broadcastPresenceToAll(sender, statusOnline, 1)
		<-entered

		viewerClient := &Client{ID: uuid.New(), UserID: viewer, Send: make(chan []byte, 20)}
		hub.registerClient(viewerClient)
		// A truncated snapshot that does NOT carry this sender — what a hub above
		// presenceSnapshotConnectedLimit produces.
		// A published snapshot that does NOT carry this sender — what truncation at
		// presenceSnapshotConnectedLimit, or an authorization filter, produces.
		viewerClient.setPresenceSnapshotCoverage(map[uuid.UUID]struct{}{sender: {}}, nil)

		close(release)
		hub.applyPresenceAudience(<-hub.presenceAudienceResults)

		assert.Equal(t, []string{statusOnline}, collect(t, viewerClient),
			"the snapshot omitted this sender, so dropping the delta would leave the "+
				"viewer rendering them offline until an unrelated transition")
	})

	t.Run("a sender the truncated snapshot DID carry is still filtered", func(t *testing.T) {
		db := setupHubTestDB(t)
		hub := NewHub(db, setupHubTestRedis(t))
		sender := presenceTestUser(t, db)
		viewer := presenceTestUser(t, db)
		presenceTestFriendship(t, db, sender, viewer)

		entered, release := make(chan struct{}), make(chan struct{})
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			close(entered)
			<-release
			return map[uuid.UUID]bool{viewer: true}, nil
		}

		hub.broadcastPresenceToAll(sender, statusOnline, 1)
		<-entered

		viewerClient := &Client{ID: uuid.New(), UserID: viewer, Send: make(chan []byte, 20)}
		hub.registerClient(viewerClient)
		viewerClient.setPresenceSnapshotCoverage(map[uuid.UUID]struct{}{uuid.New(): {}}, nil)

		close(release)
		hub.applyPresenceAudience(<-hub.presenceAudienceResults)

		assert.Empty(t, collect(t, viewerClient),
			"coverage must EXEMPT omitted senders, not disable the frontier wholesale")
	})

	t.Run("control: the sync path is never filtered", func(t *testing.T) {
		db := setupHubTestDB(t)
		hub := NewHub(db, setupHubTestRedis(t))
		sender := presenceTestUser(t, db)
		viewer := presenceTestUser(t, db)
		presenceTestFriendship(t, db, sender, viewer)
		hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
			return map[uuid.UUID]bool{viewer: true}, nil
		}

		viewerClient := &Client{ID: uuid.New(), UserID: viewer, Send: make(chan []byte, 20)}
		hub.registerClient(viewerClient)
		// Registering advances nothing, so a naive `<=` against a stale frontier
		// would swallow this. presenceDispatchSeqAlways is why it does not.
		hub.broadcastPresenceToAllSync(sender, statusOnline, 1)

		assert.Equal(t, []string{statusOnline}, collect(t, viewerClient),
			"shutdown fan-out must never be filtered by a frontier")
	})
}

// TestRevokedViewerDoesNotReceivePresenceAfterAudienceComputation is the #2992
// regression, in the shape @red-team's TestRedteam_B1_RevokedViewerStillReceivesFrame
// specified. It drives the real window: the audience is computed off the Run
// goroutine, authorization is revoked while the result sits in the channel, and the
// result is then applied on Run.
//
// WHAT EACH ARM PROVES, because they are not interchangeable:
//
//   - "control" — authorization intact, no signal. MUST deliver. Without it a green
//     revoked arm cannot distinguish "the fence suppressed the frame" from "no frame
//     was ever going to arrive". The first draft of this test had no control, filtered
//     on the wrong frame type, and passed while proving nothing.
//   - "revoked with signal" — the production shape. The raw SQL delete stands in for
//     the revoking write; InvalidatePresenceAudiences stands in for the post-commit
//     rail dispatch that follows it. The test issues the signal EXPLICITLY rather than
//     driving a handler, so it locks the FENCE, not the wiring —
//     TestDisconnectRichPresenceClientsInvalidatesAudiences locks the wiring.
//   - "revoked without signal" — the RESIDUAL, asserted as a leak on purpose. An apply
//     that beats the post-commit bump still delivers. Pinning it means the day someone
//     closes it properly, this arm fails and forces the residual to be re-read rather
//     than silently outliving its own documentation.
func TestRevokedViewerDoesNotReceivePresenceAfterAudienceComputation(t *testing.T) {
	t.Run("control/authorized, no signal", func(t *testing.T) {
		assert.Equal(t, []string{statusOnline}, revokedViewerCase(t, false, false),
			"CONTROL: with authorization intact this construction MUST deliver exactly "+
				"one frame — if it does not, neither arm below proves anything")
	})
	t.Run("revoked with signal", func(t *testing.T) {
		assert.Empty(t, revokedViewerCase(t, true, true),
			"a viewer whose authorization was revoked between audience computation and "+
				"enqueue must receive no presence frame for that sender (#2992, CWE-367)")
	})
	t.Run("residual/revoked before the signal lands", func(t *testing.T) {
		assert.Equal(t, []string{statusOnline}, revokedViewerCase(t, true, false),
			"DOCUMENTED RESIDUAL, not an aspiration: the revoking write commits and only "+
				"then dispatches, so an apply that beats the bump still delivers. If this "+
				"arm ever goes empty the residual has been closed — update the scope note "+
				"on InvalidatePresenceAudiences and #2992 rather than deleting this arm")
	})
}

// revokedViewerCase returns the statuses delivered to the viewer for the sender.
func revokedViewerCase(t *testing.T, revoke, signal bool) []string {
	t.Helper()
	hub, db := setupCustomTextHub(t)
	senderID := insertCTUser(t, db, "revoked_sender")
	viewerID := insertCTUser(t, db, "revoked_viewer")
	makeFriends(t, db, senderID, viewerID)

	viewer := &Client{ID: uuid.New(), UserID: viewerID, Send: make(chan []byte, 16), Hub: hub}
	hub.clients[viewer.ID] = viewer
	hub.userClients[viewerID] = map[uuid.UUID]bool{viewer.ID: true}
	// Registered and past bootstrap, so the registration frontier does not filter
	// for an unrelated reason and mask the result.
	viewer.beginBootstrap()
	require.True(t, hub.completeClientBootstrap(viewer, []byte(`{"type":"presence_snapshot"}`)))
	for len(viewer.Send) > 0 {
		<-viewer.Send
	}

	// Dispatch: the audience is computed off Run against the CURRENT graph.
	hub.broadcastPresenceToAll(senderID, statusOnline, 1)

	var result presenceAudienceResult
	select {
	case result = <-hub.presenceAudienceResults:
	case <-time.After(5 * time.Second):
		t.Fatal("audience computation did not complete")
	}
	require.NoError(t, result.err, "the audience query must succeed or the case proves nothing")
	require.True(t, result.audience[viewerID],
		"positive control: the viewer must be IN the computed audience, otherwise the "+
			"revocation below is not what suppresses the frame")

	// THE WINDOW.
	if revoke {
		_, err := db.Exec(`
			DELETE FROM friendships
			WHERE (requester_id = $1 AND addressee_id = $2)
			   OR (requester_id = $2 AND addressee_id = $1)
		`, senderID, viewerID)
		require.NoError(t, err)
	}
	if signal {
		hub.InvalidatePresenceAudiences()
	}

	hub.applyPresenceAudience(result)

	var delivered []string
	for len(viewer.Send) > 0 {
		var frame map[string]interface{}
		require.NoError(t, json.Unmarshal(<-viewer.Send, &frame))
		if frame["type"] != "presence" {
			continue
		}
		if data := presenceFrameData(t, frame); data["user_id"] == senderID.String() {
			delivered = append(delivered, presenceFrameStatus(t, frame))
		}
	}
	return delivered
}

// TestDisconnectRichPresenceClientsInvalidatesAudiences locks the WIRING that the
// fence depends on. The fence itself is inert unless something calls it, and this
// is the hub's single observation point for a committed revocation — the graph and
// membership rails both dispatch here. Deleting the call would leave every fence
// test above still green, because they signal explicitly.
func TestDisconnectRichPresenceClientsInvalidatesAudiences(t *testing.T) {
	hub := NewHub(nil, nil)
	before := hub.presenceAuthzEpoch.Load()
	require.NoError(t, hub.DisconnectRichPresenceClients(context.Background(),
		map[uuid.UUID]bool{uuid.New(): true}))
	assert.Greater(t, hub.presenceAuthzEpoch.Load(), before,
		"a committed revocation reaching the hub must invalidate in-flight audiences")

	// It must fire even when the recipient set contains nobody connected: the
	// audiences being invalidated belong to OTHER users, and gating the bump on
	// there being someone to disconnect would silence it in exactly the case where
	// the revoked viewer has already gone away.
	empty := hub.presenceAuthzEpoch.Load()
	require.NoError(t, hub.DisconnectRichPresenceClients(context.Background(),
		map[uuid.UUID]bool{}))
	assert.Greater(t, hub.presenceAuthzEpoch.Load(), empty,
		"an empty recipient set still means a revocation committed")

	// The ESCALATION arm is a SEPARATE hub method and was missed by the first
	// wiring pass: graphpresence's reconciler falls back to it when it cannot
	// determine the affected audience, so every escalated revocation was
	// invisible to the fence while the targeted path was covered. Gitar caught it
	// on PR #2975. Asserted here rather than in its own test so the two
	// observation points cannot drift apart unnoticed.
	esc := hub.presenceAuthzEpoch.Load()
	require.NoError(t, hub.DisconnectAllRichPresenceClients(context.Background()))
	assert.Greater(t, hub.presenceAuthzEpoch.Load(), esc,
		"the escalation disconnect fires when the audience is UNKNOWN, so an in-flight "+
			"audience is even less trustworthy here than on the targeted path")
}

// TestPresenceAudienceSuppressedWhenHandoffOutrunsItsBound covers the half of the
// fence that needs no signal at all. It is what bounds the residual: the widening
// #2975 introduced is an unbounded handoff under Run backlog, and a result whose
// handoff outran presenceAudienceMaxHandoff is treated as unproven whether or not
// any revocation was observed.
func TestPresenceAudienceSuppressedWhenHandoffOutrunsItsBound(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	senderID := insertCTUser(t, db, "handoff_sender")
	viewerID := insertCTUser(t, db, "handoff_viewer")
	makeFriends(t, db, senderID, viewerID)

	viewer := &Client{ID: uuid.New(), UserID: viewerID, Send: make(chan []byte, 16), Hub: hub}
	hub.clients[viewer.ID] = viewer
	hub.userClients[viewerID] = map[uuid.UUID]bool{viewer.ID: true}
	viewer.beginBootstrap()
	require.True(t, hub.completeClientBootstrap(viewer, []byte(`{"type":"presence_snapshot"}`)))
	for len(viewer.Send) > 0 {
		<-viewer.Send
	}

	hub.broadcastPresenceToAll(senderID, statusOnline, 1)
	var result presenceAudienceResult
	select {
	case result = <-hub.presenceAudienceResults:
	case <-time.After(5 * time.Second):
		t.Fatal("audience computation did not complete")
	}
	require.NoError(t, result.err)

	// Age the handoff past its bound and exhaust the retry budget, so the result
	// reaches delivery rather than being re-dispatched. Backdating rather than
	// sleeping keeps this deterministic — a test that slept would be asserting on
	// the scheduler.
	result.computedAt = result.computedAt.Add(-2 * presenceAudienceMaxHandoff)
	result.attempt = uint8(presenceAudienceMaxAttempts)
	hub.applyPresenceAudience(result)

	var delivered []string
	for len(viewer.Send) > 0 {
		var frame map[string]interface{}
		require.NoError(t, json.Unmarshal(<-viewer.Send, &frame))
		if frame["type"] == "presence" {
			delivered = append(delivered, presenceFrameStatus(t, frame))
		}
	}
	assert.Empty(t, delivered,
		"an exhausted retry budget must FAIL CLOSED — delivering here would make the "+
			"budget a fail-open timer, which is the inversion this assertion exists to catch")
}

// TestPresenceAudienceWithNoComputedAtSkipsOnlyTheHandoffArm pins the zero-value
// behaviour of the #2992 fence, INVERTED from its first cut.
//
// The first version asserted the opposite — that an unstamped result is unproven
// — on the reasoning that a forgotten stamp should stall presence rather than
// silently disable half the fence. The reasoning named the wrong consequence. An
// unproven result RE-DISPATCHES, so an unstamped one retries until its budget is
// gone; and because the retry path re-dispatches WITHOUT re-checking h.db, on a
// hub with no database it reached the query and panicked the process from a
// spawned goroutine. A stall was never on the menu.
//
// The corrected rule: an unstamped result cannot be AGED, so the handoff arm does
// not apply. The EPOCH arm still does, and that is the arm carrying the
// authorization property — the handoff bound only limits how long a PROVEN
// audience may sit before delivery. So this is not fail-open on the security
// property; it declines to evaluate a bound it has no input for.
func TestPresenceAudienceWithNoComputedAtSkipsOnlyTheHandoffArm(t *testing.T) {
	hub := NewHub(nil, nil)
	result := presenceAudienceResult{
		userID:   uuid.New(),
		audience: map[uuid.UUID]bool{uuid.New(): true},
	}
	assert.False(t, hub.presenceAudienceUnproven(result),
		"an unstamped result cannot be aged, so the handoff arm must not fire — "+
			"firing it turns every hand-built result into a retry")

	// The epoch arm is unaffected by the stamp: still the authorization fence.
	hub.InvalidatePresenceAudiences()
	assert.True(t, hub.presenceAudienceUnproven(result),
		"a revocation must still make an UNSTAMPED result unproven — this is what "+
			"makes the change above a narrowing of the age check and not a hole in "+
			"the authorization check")

	// And a stamped, current-epoch result within the bound stays proven.
	fresh := presenceAudienceResult{
		userID:     uuid.New(),
		audience:   map[uuid.UUID]bool{uuid.New(): true},
		authzEpoch: hub.presenceAuthzEpoch.Load(),
		computedAt: time.Now(),
	}
	assert.False(t, hub.presenceAudienceUnproven(fresh),
		"positive control: a fresh stamped result under the current epoch is proven")
}

// TestPresenceAudienceWorkerWithNoDatabaseFailsClosedInsteadOfPanicking locks the
// consequence that made the above urgent. runPresenceAudienceQuery executes on a
// SPAWNED GOROUTINE, so a nil dereference there panics the whole process rather
// than failing one request — and the dispatch-site h.db guards do not cover it,
// because retryPresenceAudience and redialPresenceAudience re-dispatch without
// re-checking.
func TestPresenceAudienceWorkerWithNoDatabaseFailsClosedInsteadOfPanicking(t *testing.T) {
	hub := NewHub(nil, nil)
	audience, err := hub.runPresenceAudienceQuery(uuid.New())
	require.ErrorIs(t, err, errPresenceNoDatabase,
		"a worker with no database must return an error, never dereference nil")
	assert.Nil(t, audience, "fail closed: no audience means no fan-out (#47)")

	assert.False(t, hub.retryPresenceAudience(presenceAudienceResult{
		userID: uuid.New(), err: errPresenceNoDatabase,
	}), "a missing database is not transient, so it must not be re-dispatched")
}

// TestPresenceAudienceRevokedDuringTheQueryIsUnproven covers the span every other
// fence test misses: a revocation that commits WHILE the audience query is running.
//
// The other arms bump the epoch after the result is already in hand, so they pass
// whether the worker reads the epoch before or after its query. That is what let the
// original ordering ship — the epoch was loaded AFTER the query, so a mid-query bump
// was already reflected in the value being stored, the comparison at apply matched,
// and an audience built from the pre-revocation graph was delivered as proven. The
// query is the longest span in the window, so this was the majority of the exposure.
// CodeRabbit caught it on PR #2997.
//
// The revocation is injected from INSIDE the computer seam, which is the only place
// that is genuinely concurrent with the query.
func TestPresenceAudienceRevokedDuringTheQueryIsUnproven(t *testing.T) {
	hub, db := setupCustomTextHub(t)
	senderID := insertCTUser(t, db, "midquery_sender")
	viewerID := insertCTUser(t, db, "midquery_viewer")

	viewer := &Client{ID: uuid.New(), UserID: viewerID, Send: make(chan []byte, 16), Hub: hub}
	hub.clients[viewer.ID] = viewer
	hub.userClients[viewerID] = map[uuid.UUID]bool{viewer.ID: true}
	viewer.beginBootstrap()
	require.True(t, hub.completeClientBootstrap(viewer, []byte(`{"type":"presence_snapshot"}`)))
	for len(viewer.Send) > 0 {
		<-viewer.Send
	}

	// The revocation lands mid-query: the returned audience is built from the
	// pre-revocation graph, exactly as a real query's snapshot would be.
	hub.presenceAudienceComputer = func(context.Context, uuid.UUID) (map[uuid.UUID]bool, error) {
		hub.InvalidatePresenceAudiences()
		return map[uuid.UUID]bool{viewerID: true}, nil
	}

	hub.broadcastPresenceToAll(senderID, statusOnline, 1)
	var result presenceAudienceResult
	select {
	case result = <-hub.presenceAudienceResults:
	case <-time.After(5 * time.Second):
		t.Fatal("audience computation did not complete")
	}
	require.NoError(t, result.err)
	require.True(t, result.audience[viewerID],
		"positive control: the pre-revocation audience must still name the viewer, or "+
			"the suppression below is not the fence doing its job")

	assert.True(t, hub.presenceAudienceUnproven(result),
		"a revocation concurrent with the query must leave the result UNPROVEN — this "+
			"is what fails when the epoch is read after the query instead of before it")

	// Exhaust the budget so the result reaches delivery rather than being re-dispatched.
	result.attempt = uint8(presenceAudienceMaxAttempts)
	hub.applyPresenceAudience(result)

	var delivered []string
	for len(viewer.Send) > 0 {
		var frame map[string]interface{}
		require.NoError(t, json.Unmarshal(<-viewer.Send, &frame))
		if frame["type"] == "presence" {
			delivered = append(delivered, presenceFrameStatus(t, frame))
		}
	}
	assert.Empty(t, delivered,
		"the viewer lost authorization while the query ran, so no frame may be delivered")
}
