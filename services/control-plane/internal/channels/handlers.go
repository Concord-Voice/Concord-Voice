// Package channels provides handlers for managing server channels.
package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/e2eekeys"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgInvalidServerID         = "Invalid server ID"
	errMsgInvalidChannelID        = "Invalid channel ID"
	errMsgInvalidRequestBody      = "Invalid request body"
	errMsgInsufficientPerms       = "insufficient permissions"
	errMsgForeignGroup            = "group_id does not belong to this server"
	errMsgNotMemberOfServer       = "Not a member of this server"
	errMsgChannelNotFound         = "Channel not found"
	errMsgChannelNotFoundOrDenied = "Channel not found or access denied"
	errMsgFailedFetchChannel      = "Failed to fetch channel"
	errMsgFailedFetchChannels     = "Failed to fetch channels"
	errMsgFailedCreateChannel     = "Failed to create channel"
	errMsgFailedUpdateChannel     = "Failed to update channel"
	errMsgFailedDeleteChannel     = "Failed to delete channel"
	errMsgFailedCheckMembership   = "Failed to check membership"
	errMsgFailedFetchKeys         = "Failed to fetch keys"
	errMsgFailedFetchUnreadCounts = "Failed to fetch unread counts"
	errMsgFailedMarkServerRead    = "Failed to mark server read"
	errMsgFailedMarkChannelRead   = "Failed to mark channel read"
	errMsgFailedResolveVisible    = "Failed to resolve visible channels"
	errMsgFailedFetchServerUnread = "Failed to fetch server unread status"
	errMsgNoEncryptionKey         = "No encryption key available yet"
	errMsgFailedDistributeKeys    = "Failed to distribute keys"
	// errMsgAuthRequired matches the middleware's generic auth-failure body so
	// an epoch-fence rejection inside a handler is indistinguishable from the
	// middleware's own rejection (#2201).
	errMsgAuthRequired            = "Authentication required"
	errMsgInvalidContextID        = "Invalid context ID"
	errMsgFailedProcessRewrap     = "Failed to process rewrap request"
	errMsgFailedEnrollRewrap      = "Failed to enroll rewrap request"
	errMsgContextNotFound         = "Context not found"
	errMsgContextNotFoundOrDenied = "Context not found or access denied"
	errMsgNotMemberOrParticipant  = "Not a member or participant"
	logMsgFailedCheckPermissions  = "Failed to check permissions"
)

// Handler handles channel-related requests
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	resolver    *rbac.Resolver
	redis       *redis.Client
	serverTiers entitlements.ServerTierResolver
}

// NewHandler creates a new channel handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, redis *redis.Client, serverTiers ...entitlements.ServerTierResolver) *Handler {
	var st entitlements.ServerTierResolver
	if len(serverTiers) > 0 {
		st = serverTiers[0]
	}
	return &Handler{
		db:          db,
		log:         log,
		hub:         hub,
		resolver:    resolver,
		redis:       redis,
		serverTiers: st,
	}
}

func (h *Handler) serverTier(ctx context.Context, serverID string) string {
	if h.serverTiers != nil {
		return h.serverTiers.GetServerTier(ctx, serverID)
	}
	return entitlements.ResolveServerTier(ctx, h.db, serverID)
}

// CreateChannelRequest represents a request to create a channel
type CreateChannelRequest struct {
	ServerID    string            `json:"server_id" binding:"required,uuid"`
	Name        string            `json:"name" binding:"required,min=3,max=100"`
	Type        string            `json:"type" binding:"required,oneof=text voice bulletin"`
	Emoji       *string           `json:"emoji,omitempty"`        // Optional custom emoji
	GroupID     *string           `json:"group_id,omitempty"`     // Channel group (category); nil = uncategorized
	WrappedKeys map[string]string `json:"wrapped_keys,omitempty"` // user_id → wrapped CSK (required for all channels)
	// WrappedKeyVersions carries the public_keys.key_version each initial-member
	// CSK was wrapped against (#2420; optional, fail-open when absent). Guards the
	// creator's initial wrap against a concurrent recipient key reset, same as the
	// distribution path.
	WrappedKeyVersions map[string]int `json:"wrapped_key_versions,omitempty"`
}

// UpdateChannelRequest represents a request to update a channel
type UpdateChannelRequest struct {
	Name             string  `json:"name" binding:"required,min=3,max=100"`
	Type             string  `json:"type" binding:"required,oneof=text voice bulletin"`
	Emoji            *string `json:"emoji,omitempty"`
	AudioQualityTier *string `json:"audio_quality_tier,omitempty"`
	GroupID          *string `json:"group_id"` // pointer: nil=unchanged, ""=uncategorized, "uuid"=set group
}

// Valid audio quality tier values
var validAudioQualityTiers = map[string]bool{
	"minimum": true, "low": true, "moderate": true, "standard": true, "high": true, "hifi": true, "studio": true,
}

// ListChannels returns all channels in a server that the user has permission to view.
func (h *Handler) ListChannels(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
		return
	}

	visibleIDs, err := h.resolver.GetVisibleChannelIDs(c.Request.Context(), serverID, userID)
	if err != nil {
		h.log.Error(errMsgFailedResolveVisible, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}

	visibleSet := make(map[string]bool, len(visibleIDs))
	for _, id := range visibleIDs {
		visibleSet[id] = true
	}

	channels, err := h.queryVisibleChannels(serverID, visibleSet)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}

	groups, err := h.queryChannelGroups(serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channels": channels, "channel_groups": groups})
}

func (h *Handler) queryVisibleChannels(serverID string, visibleSet map[string]bool) ([]models.Channel, error) {
	rows, err := h.db.Query(
		`SELECT id, server_id, name, type, description, emoji, audio_quality_tier, group_id, linked_voice_channel_id, sync_permissions, position, created_at, updated_at
		FROM channels
		WHERE server_id = $1
		ORDER BY position ASC, created_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query channels", "error", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var channels []models.Channel
	for rows.Next() {
		ch, scanErr := scanChannel(rows)
		if scanErr != nil {
			h.log.Error("Failed to scan channel", "error", scanErr)
			continue
		}
		if visibleSet[ch.ID] {
			channels = append(channels, ch)
		}
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating channels", "error", err)
		return nil, err
	}
	if channels == nil {
		channels = []models.Channel{}
	}
	return channels, nil
}

func scanChannel(rows *sql.Rows) (models.Channel, error) {
	var ch models.Channel
	err := rows.Scan(
		&ch.ID, &ch.ServerID, &ch.Name, &ch.Type, &ch.Description,
		&ch.Emoji, &ch.AudioQualityTier, &ch.GroupID,
		&ch.LinkedVoiceChannelID, &ch.SyncPermissions, &ch.Position,
		&ch.CreatedAt, &ch.UpdatedAt,
	)
	return ch, err
}

func (h *Handler) queryChannelGroups(serverID string) ([]models.ChannelGroup, error) {
	groupRows, err := h.db.Query(
		`SELECT id, server_id, name, position, created_at, updated_at
		 FROM channel_groups
		 WHERE server_id = $1
		 ORDER BY position ASC, created_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query channel groups", "error", err)
		return nil, err
	}
	defer func() { _ = groupRows.Close() }()

	var groups []models.ChannelGroup
	for groupRows.Next() {
		var g models.ChannelGroup
		if err := groupRows.Scan(&g.ID, &g.ServerID, &g.Name, &g.Position, &g.CreatedAt, &g.UpdatedAt); err != nil {
			h.log.Error("Failed to scan channel group", "error", err)
			continue
		}
		groups = append(groups, g)
	}
	if err := groupRows.Err(); err != nil {
		h.log.Error("Error iterating channel groups", "error", err)
		return nil, err
	}
	if groups == nil {
		groups = []models.ChannelGroup{}
	}
	return groups, nil
}

// CreateChannel creates a new channel in a server
// admitCreateChannelRequest runs CreateChannel's pre-transaction gates:
// PermManageChannels, the CV-CAN-010 foreign-group rejection (binding a new
// channel to another server's category would let the permission-sync cascade
// copy that server's category overrides in by group_id), and the
// E2EE-everywhere (#201) wrapped-keys requirement. On failure the HTTP
// response is written and false is returned.
func (h *Handler) admitCreateChannelRequest(c *gin.Context, req CreateChannelRequest, userID string) bool {
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), req.ServerID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return false
	}

	groupOK, groupErr := h.groupBelongsToServer(c.Request.Context(), req.GroupID, req.ServerID)
	if groupErr != nil {
		h.log.Error("Failed to validate channel group ownership", "error", groupErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return false
	}
	if !groupOK {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgForeignGroup})
		return false
	}

	if len(req.WrappedKeys) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Encrypted channels require wrapped keys for all members"})
		return false
	}
	return true
}

// CreateChannel creates a channel (plus a linked text channel for voice) and
// stores the E2EE-everywhere wrapped keys inside one epoch-guarded transaction.
func (h *Handler) CreateChannel(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if !h.admitCreateChannelRequest(c, req, userID) {
		return
	}

	// Start transaction for channel + keys
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	// #2201: the wrapped keys stored below are key-material-coupled state —
	// recheck the creator's credential epoch inside the tx (covers both
	// storeWrappedKeys and the linked-text-channel copy in this same tx).
	if guardErr := credepoch.GuardTx(c.Request.Context(), tx, userID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		// Discriminate an epoch-fence rejection (401) from a store/lock read
		// error (logged 500): the request was already authenticated, so a
		// transient DB failure is a server error, not "re-authenticate"
		// (Codex #2397 review).
		if errors.Is(guardErr, credepoch.ErrEpochMismatch) || errors.Is(guardErr, credepoch.ErrBlocked) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgAuthRequired})
			return
		}
		h.log.Error("credential-epoch guard read failed", "error", guardErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}

	nextPos := h.computeNextPosition(tx, req.ServerID, req.GroupID)
	channelID := uuid.New().String()

	channel, err := h.insertChannel(tx, channelID, req, nextPos)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}

	// Store wrapped channel keys (always required under E2EE-everywhere)
	if h.storeWrappedKeys(c.Request.Context(), tx, channelID, req.WrappedKeys, req.WrappedKeyVersions) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store encryption keys"})
		return
	}

	// Auto-create linked text channel for voice channels
	linkedTextChannel, ltcErr := h.maybeCreateLinkedTextChannel(c.Request.Context(), tx, req, channelID, nextPos)
	if ltcErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create linked text channel"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}

	h.log.Info("Channel created", "channel_id", channelID, "server_id", req.ServerID, "user_id", userID)

	h.broadcastChannelCreated(req.ServerID, channel, linkedTextChannel)

	// Return response: voice channels include the linked text channel
	response := gin.H{"channel": channel}
	if linkedTextChannel != nil {
		response["linked_text_channel"] = linkedTextChannel
	}
	c.JSON(http.StatusCreated, response)
}

// computeNextPosition returns the next position for a channel within a group (or uncategorized).
func (h *Handler) computeNextPosition(tx *sql.Tx, serverID string, groupID *string) int {
	var maxPos int
	if groupID != nil {
		_ = tx.QueryRow(
			`SELECT COALESCE(MAX(position), -1) FROM channels WHERE server_id = $1 AND group_id = $2`,
			serverID, *groupID,
		).Scan(&maxPos)
	} else {
		_ = tx.QueryRow(
			`SELECT COALESCE(MAX(position), -1) FROM channels WHERE server_id = $1 AND group_id IS NULL`,
			serverID,
		).Scan(&maxPos)
	}
	return maxPos + 1
}

// insertChannel creates the primary channel row within a transaction.
func (h *Handler) insertChannel(tx *sql.Tx, channelID string, req CreateChannelRequest, position int) (models.Channel, error) {
	insertQuery := `
		INSERT INTO channels (id, server_id, name, type, emoji, group_id, position, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var channel models.Channel
	channel.ID = channelID
	channel.ServerID = req.ServerID
	channel.Name = req.Name
	channel.Type = req.Type
	channel.Emoji = req.Emoji
	channel.GroupID = req.GroupID
	channel.Position = position

	err := tx.QueryRow(insertQuery, channelID, req.ServerID, req.Name, req.Type, req.Emoji, req.GroupID, position).Scan(
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create channel", "error", err)
		return channel, err
	}
	return channel, nil
}

// storeWrappedKeys inserts wrapped E2EE keys for a channel within a transaction.
// wrappedKeyRecipientStale runs the #2420 recipient-freshness guard for one
// initial-member wrap (channel creation). When the creator supplied the
// wrapped-against public-key version, it verifies (FOR SHARE, serializing
// against a concurrent ReplaceMyKeys/RecoveryResetAccount) that the recipient
// has not rotated it; a stale recipient is skipped (stale=true) after an
// idempotent self-heal enqueue so the servicer re-wraps to the new key. Absent
// version → fail-open (stale=false), preserving old-client behavior.
func wrappedKeyRecipientStale(ctx context.Context, tx *sql.Tx, channelID, memberUserID string, wrappedKeyVersions map[string]int) (bool, error) {
	wrappedVersion, ok := wrappedKeyVersions[memberUserID]
	if !ok {
		return false, nil
	}
	fresh, err := recipientKeyFresh(ctx, tx, memberUserID, wrappedVersion)
	if err != nil {
		return false, err
	}
	if fresh {
		return false, nil
	}
	if eErr := enqueueChannelKeyRequest(ctx, tx, channelID, memberUserID); eErr != nil {
		return false, fmt.Errorf("enqueue self-heal: %w", eErr)
	}
	return true, nil
}

func (h *Handler) storeWrappedKeys(ctx context.Context, tx *sql.Tx, channelID string, wrappedKeys map[string]string, wrappedKeyVersions map[string]int) error {
	keyInsert := `
		INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, 1)
	`
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue // skip invalid UUIDs
		}
		stale, sErr := wrappedKeyRecipientStale(ctx, tx, channelID, memberUserID, wrappedKeyVersions)
		if sErr != nil {
			return sErr
		}
		if stale {
			continue // skip the stale wrap; the enqueued self-heal re-wraps it
		}
		if _, err := tx.ExecContext(ctx, keyInsert, channelID, memberUserID, wrappedKey); err != nil {
			h.log.Error("Failed to store channel key", "error", err, "user_id", memberUserID)
			return err
		}
	}
	return nil
}

// maybeCreateLinkedTextChannel creates a linked text channel for voice channels, or returns nil for other types.
func (h *Handler) maybeCreateLinkedTextChannel(ctx context.Context, tx *sql.Tx, req CreateChannelRequest, voiceChannelID string, nextPos int) (*models.Channel, error) {
	if req.Type != "voice" {
		return nil, nil
	}
	return h.createLinkedTextChannel(ctx, tx, req, voiceChannelID, nextPos+1)
}

// createLinkedTextChannel creates a linked text channel for a voice channel.
func (h *Handler) createLinkedTextChannel(ctx context.Context, tx *sql.Tx, req CreateChannelRequest, voiceChannelID string, position int) (*models.Channel, error) {
	linkedTextID := uuid.New().String()
	linkedInsert := `
		INSERT INTO channels (id, server_id, name, type, group_id, linked_voice_channel_id, position, created_at, updated_at)
		VALUES ($1, $2, $3, 'text', $4, $5, $6, NOW(), NOW())
		RETURNING created_at, updated_at
	`
	var ltc models.Channel
	ltc.ID = linkedTextID
	ltc.ServerID = req.ServerID
	ltc.Name = req.Name
	ltc.Type = "text"
	ltc.GroupID = req.GroupID
	ltc.LinkedVoiceChannelID = &voiceChannelID
	ltc.Position = position

	err := tx.QueryRow(linkedInsert, linkedTextID, req.ServerID, req.Name, req.GroupID, voiceChannelID, position).Scan(
		&ltc.CreatedAt,
		&ltc.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create linked text channel", "error", err)
		return nil, err
	}

	// Copy wrapped keys for the linked text channel too (non-fatal)
	h.storeWrappedKeysNonFatal(ctx, tx, linkedTextID, req.WrappedKeys, req.WrappedKeyVersions)

	return &ltc, nil
}

// storeWrappedKeysNonFatal stores wrapped keys but does not fail on error (keys can be distributed later).
func (h *Handler) storeWrappedKeysNonFatal(ctx context.Context, tx *sql.Tx, channelID string, wrappedKeys map[string]string, wrappedKeyVersions map[string]int) {
	keyInsert := `
		INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, 1)
	`
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}
		// #2420: apply the same recipient-freshness guard to the linked-text copy
		// so a stale member is skipped consistently across both channels. A
		// freshness-read error is non-fatal here (like an insert error) — skip and
		// log; the key can be distributed later.
		stale, sErr := wrappedKeyRecipientStale(ctx, tx, channelID, memberUserID, wrappedKeyVersions)
		if sErr != nil {
			h.log.Error("Failed recipient-freshness check for linked text channel key", "error", sErr, "user_id", memberUserID)
			continue
		}
		if stale {
			continue
		}
		if _, err := tx.ExecContext(ctx, keyInsert, channelID, memberUserID, wrappedKey); err != nil {
			h.log.Error("Failed to store linked text channel key", "error", err, "user_id", memberUserID)
		}
	}
}

// broadcastChannelCreated sends channel_created events to server subscribers.
func (h *Handler) broadcastChannelCreated(serverID string, channel models.Channel, linkedTextChannel *models.Channel) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "channel_created",
		Data: map[string]interface{}{
			"channel": channelToMap(channel),
		},
	})

	if linkedTextChannel != nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "channel_created",
			Data: map[string]interface{}{
				"channel": channelToMap(*linkedTextChannel),
			},
		})
	}
}

// channelToMap converts a Channel model to a map for broadcast payloads.
func channelToMap(ch models.Channel) map[string]interface{} {
	m := map[string]interface{}{
		"id":         ch.ID,
		"server_id":  ch.ServerID,
		"name":       ch.Name,
		"type":       ch.Type,
		"emoji":      ch.Emoji,
		"group_id":   ch.GroupID,
		"position":   ch.Position,
		"created_at": ch.CreatedAt,
		"updated_at": ch.UpdatedAt,
	}
	if ch.LinkedVoiceChannelID != nil {
		m["linked_voice_channel_id"] = ch.LinkedVoiceChannelID
	}
	return m
}

// viewPermForType maps a channel type to the view permission bit that gates it.
// It mirrors the CASE WHEN c.type = 'voice' logic in Resolver.GetVisibleChannelIDs /
// GetAllVisibleChannelIDs, keeping the type→permission mapping in one place so the
// per-channel gates in GetChannel and MarkChannelRead cannot silently drift from
// each other (or from the resolver) if a new visible channel type is introduced.
func viewPermForType(channelType string) rbac.Permission {
	if channelType == "voice" {
		return rbac.PermViewVoiceChannels
	}
	return rbac.PermViewTextChannels
}

// GetChannel returns a specific channel
func (h *Handler) GetChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Get channel and check if user is a member of the server
	query := `
		SELECT c.id, c.server_id, c.name, c.type, c.description, c.emoji, c.audio_quality_tier, c.group_id, c.linked_voice_channel_id, c.sync_permissions, c.position, c.created_at, c.updated_at
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`

	var channel models.Channel
	err := h.db.QueryRow(query, channelID, userID).Scan(
		&channel.ID,
		&channel.ServerID,
		&channel.Name,
		&channel.Type,
		&channel.Description,
		&channel.Emoji,
		&channel.AudioQualityTier,
		&channel.GroupID,
		&channel.LinkedVoiceChannelID,
		&channel.SyncPermissions,
		&channel.Position,
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFoundOrDenied})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannel})
		return
	}

	// CV-CAN-001: server membership alone is insufficient — a member denied
	// channel visibility must not read hidden channel metadata by UUID. Require
	// the type-appropriate view permission (mirrors ListChannels' visibility);
	// deny with the same not-found response as a non-member to avoid an
	// existence oracle.
	viewPerm := viewPermForType(channel.Type)
	canView, permErr := h.resolver.HasPermission(c.Request.Context(), channel.ServerID, userID, channelID, viewPerm)
	if permErr != nil {
		h.log.Error("Failed to check channel view permission", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannel})
		return
	}
	if !canView {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFoundOrDenied})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

// validateUpdateChannelGroupOwnership verifies req's target group_id belongs to
// serverID (CV-CAN-011). On a lookup error or a foreign group it writes the HTTP
// error response and returns false.
func (h *Handler) validateUpdateChannelGroupOwnership(c *gin.Context, req UpdateChannelRequest, serverID string) bool {
	groupOK, groupErr := h.groupBelongsToServer(c.Request.Context(), req.GroupID, serverID)
	if groupErr != nil {
		h.log.Error("Failed to validate channel group ownership", "error", groupErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return false
	}
	if !groupOK {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgForeignGroup})
		return false
	}
	return true
}

// UpdateChannel updates a channel's details
func (h *Handler) UpdateChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req UpdateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	serverID, err := h.lookupChannelServerID(channelID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// CV-CAN-011: reject moving this channel under a category owned by another
	// server before permission sync treats it as the source of overrides.
	if !h.validateUpdateChannelGroupOwnership(c, req, serverID) {
		return
	}

	if req.AudioQualityTier != nil && *req.AudioQualityTier != "" {
		if !validAudioQualityTiers[*req.AudioQualityTier] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid audio quality tier"})
			return
		}
		// Bound the channel standard to the server's audio ceiling (#179):
		// Groundspeed → Standard, any Mach → Studio. The server-tier resolver is
		// the #1521 seam (Groundspeed today). Authoritative server-side guard —
		// the client slider lock is UX only.
		if !entitlements.AudioTierAllowedForServer(*req.AudioQualityTier,
			h.serverTier(c.Request.Context(), serverID)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Audio quality tier exceeds this server's tier"})
			return
		}
	}

	channel, err := h.executeChannelUpdate(channelID, req)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}

	h.log.Info("Channel updated", "channel_id", channelID, "user_id", userID)
	h.broadcastChannelUpdated(channel)
	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

func (h *Handler) lookupChannelServerID(channelID string) (string, error) {
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err != nil && err != sql.ErrNoRows {
		h.log.Error("Failed to look up channel", "error", err)
	}
	return serverID, err
}

// groupBelongsToServer reports whether a channel's group_id is safe to set on a
// channel in serverID. A nil/empty group_id (uncategorized) is always allowed;
// otherwise the referenced channel_groups row MUST belong to the same server.
// This blocks binding or moving a channel under a category owned by ANOTHER
// server — the permission-sync cascade keys on group_id with no server
// predicate, so a cross-server binding would copy the foreign category's
// overrides into this channel (CV-CAN-010/011/012). Returns (false, nil) when
// the group is missing, malformed, or cross-server, (true, nil) when allowed,
// and a non-nil error only on a DB failure. The composite (group_id, server_id)
// FK added in migration 000082 is the structural backstop for this check.
func (h *Handler) groupBelongsToServer(ctx context.Context, groupID *string, serverID string) (bool, error) {
	if groupID == nil || *groupID == "" {
		return true, nil
	}
	// A malformed (non-UUID) group_id is a client input error, not a server
	// fault. Reject it as a bad binding (400 at the caller) instead of letting
	// the Postgres uuid cast fail the query and surface as a 500.
	if _, err := uuid.Parse(*groupID); err != nil {
		return false, nil //nolint:nilerr // malformed group_id is a client input error (400 at caller), not a server fault
	}
	// Match server ownership inside Postgres so both ids are compared as
	// canonical uuid values. Comparing the DB's canonical server_id against the
	// raw request string in Go would falsely reject an equivalent but
	// differently-cased serverID (e.g. uppercase from the client).
	var sameServer bool
	err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM channel_groups WHERE id = $1 AND server_id = $2)`,
		*groupID, serverID,
	).Scan(&sameServer)
	if err != nil {
		return false, err
	}
	return sameServer, nil
}

func resolveGroupIDParam(groupID *string) interface{} {
	if groupID == nil {
		return nil
	}
	if *groupID == "" {
		return nil
	}
	return *groupID
}

func (h *Handler) executeChannelUpdate(channelID string, req UpdateChannelRequest) (models.Channel, error) {
	var channel models.Channel
	channel.ID = channelID
	channel.Name = req.Name
	channel.Type = req.Type

	var err error
	if req.GroupID != nil {
		err = h.db.QueryRow(
			`UPDATE channels
			SET name = $1, type = $2, emoji = $3, audio_quality_tier = $4, group_id = $5, updated_at = NOW()
			WHERE id = $6
			RETURNING server_id, emoji, audio_quality_tier, group_id, linked_voice_channel_id, sync_permissions, position, created_at, updated_at`,
			req.Name, req.Type, req.Emoji, req.AudioQualityTier, resolveGroupIDParam(req.GroupID), channelID,
		).Scan(
			&channel.ServerID, &channel.Emoji, &channel.AudioQualityTier,
			&channel.GroupID, &channel.LinkedVoiceChannelID, &channel.SyncPermissions,
			&channel.Position, &channel.CreatedAt, &channel.UpdatedAt,
		)
	} else {
		err = h.db.QueryRow(
			`UPDATE channels
			SET name = $1, type = $2, emoji = $3, audio_quality_tier = $4, updated_at = NOW()
			WHERE id = $5
			RETURNING server_id, emoji, audio_quality_tier, group_id, linked_voice_channel_id, sync_permissions, position, created_at, updated_at`,
			req.Name, req.Type, req.Emoji, req.AudioQualityTier, channelID,
		).Scan(
			&channel.ServerID, &channel.Emoji, &channel.AudioQualityTier,
			&channel.GroupID, &channel.LinkedVoiceChannelID, &channel.SyncPermissions,
			&channel.Position, &channel.CreatedAt, &channel.UpdatedAt,
		)
	}
	if err != nil && err != sql.ErrNoRows {
		h.log.Error("Failed to update channel", "error", err)
	}
	return channel, err
}

func (h *Handler) broadcastChannelUpdated(channel models.Channel) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(channel.ServerID)
	if err != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "channel_updated",
		Data: map[string]interface{}{
			"channel_id":         channel.ID,
			"server_id":          channel.ServerID,
			"name":               channel.Name,
			"type":               channel.Type,
			"emoji":              channel.Emoji,
			"audio_quality_tier": channel.AudioQualityTier,
			"group_id":           channel.GroupID,
		},
	})
}

// DeleteChannel deletes a channel (owner/admin only)
func (h *Handler) DeleteChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Get channel's server ID
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to look up channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteChannel})
		return
	}

	// Check permission to manage channels
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteChannel})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Delete channel
	deleteQuery := `DELETE FROM channels WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, channelID)
	if err != nil {
		h.log.Error("Failed to delete channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteChannel})
		return
	}

	h.log.Info("Channel deleted", "channel_id", channelID, "user_id", userID)

	// Broadcast deletion to server subscribers so frontends can clean up
	if h.hub != nil {
		if serverUUID, err := uuid.Parse(serverID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_deleted",
				Data: map[string]interface{}{
					"channel_id": channelID,
					"server_id":  serverID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Channel deleted successfully"})
}

// GetUnreadCounts returns per-channel unread message counts for the channels the
// caller can view in a server.
func (h *Handler) GetUnreadCounts(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
		return
	}

	// CV-CAN-002: restrict unread counts to channels the caller can view — server
	// membership alone must not enumerate hidden channel IDs or their activity.
	visibleIDs, visErr := h.resolver.GetVisibleChannelIDs(c.Request.Context(), serverID, userID)
	if visErr != nil {
		h.log.Error(errMsgFailedResolveVisible, "error", visErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
		return
	}

	type unreadEntry struct {
		ChannelID   string `json:"channel_id"`
		UnreadCount int    `json:"unread_count"`
	}
	unreads := []unreadEntry{}
	if len(visibleIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"unreads": unreads})
		return
	}

	// For each VISIBLE channel in the server, count messages newer than
	// last_read_at. If no read state exists, fall back to the user's join date so
	// pre-existing messages are not counted as unread for first-time members.
	// Uses JOINs instead of correlated subqueries for better query planning.
	//
	// CV-CAN-002: the ANY($3) predicate scopes aggregation to visible channels in
	// SQL, so a view-denied member cannot force the aggregate over hidden
	// channels' message history (the rows are never scanned, not just dropped).
	query := `
		SELECT ch.id,
			COUNT(m.id)::int AS unread_count
		FROM channels ch
		CROSS JOIN (
			SELECT joined_at FROM server_members WHERE server_id = $1 AND user_id = $2
		) sm
		LEFT JOIN channel_read_states crs
			ON crs.channel_id = ch.id AND crs.user_id = $2
		LEFT JOIN messages m
			ON m.channel_id = ch.id
			AND m.user_id != $2
			AND m.created_at > COALESCE(crs.last_read_at, sm.joined_at)
		WHERE ch.server_id = $1
		  AND ch.id = ANY($3::uuid[])
		GROUP BY ch.id
	`

	rows, err := h.db.Query(query, serverID, userID, pq.Array(visibleIDs))
	if err != nil {
		h.log.Error("Failed to query unread counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var entry unreadEntry
		if err := rows.Scan(&entry.ChannelID, &entry.UnreadCount); err != nil {
			h.log.Error("Failed to scan unread count", "error", err)
			continue
		}
		unreads = append(unreads, entry)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating unread counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
		return
	}

	c.JSON(http.StatusOK, gin.H{"unreads": unreads})
}

// GetServerUnreadStatus returns a list of server IDs where the user has unread messages.
// Used to show unread dots on server icons without fetching per-channel counts for every server.
func (h *Handler) GetServerUnreadStatus(c *gin.Context) {
	userID := c.GetString("user_id")

	// CV-CAN-002: only count unread in channels the caller can view, so a hidden
	// channel's activity does not raise a server's unread flag. Resolved in a
	// single query across all of the user's servers to avoid a per-server N+1.
	visibleIDs, visErr := h.resolver.GetAllVisibleChannelIDs(c.Request.Context(), userID)
	if visErr != nil {
		h.log.Error(errMsgFailedResolveVisible, "error", visErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchServerUnread})
		return
	}
	if len(visibleIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"server_ids": []string{}})
		return
	}

	// Uses a LEFT JOIN to channel_read_states instead of a correlated subquery
	// so the planner can use a hash/merge join instead of nested-loop per row.
	query := `
		SELECT DISTINCT ch.server_id
		FROM channels ch
		INNER JOIN server_members sm
			ON ch.server_id = sm.server_id AND sm.user_id = $1
		LEFT JOIN channel_read_states crs
			ON crs.channel_id = ch.id AND crs.user_id = $1
		INNER JOIN messages m
			ON m.channel_id = ch.id
			AND m.user_id != $1
			AND m.created_at > COALESCE(crs.last_read_at, sm.joined_at)
		WHERE ch.id = ANY($2::uuid[])
	`

	rows, err := h.db.Query(query, userID, pq.Array(visibleIDs))
	if err != nil {
		h.log.Error("Failed to query server unread status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchServerUnread})
		return
	}
	defer func() { _ = rows.Close() }()

	serverIDs := []string{}
	for rows.Next() {
		var serverID string
		if err := rows.Scan(&serverID); err != nil {
			h.log.Error("Failed to scan server ID", "error", err)
			continue
		}
		serverIDs = append(serverIDs, serverID)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating server unread status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchServerUnread})
		return
	}

	c.JSON(http.StatusOK, gin.H{"server_ids": serverIDs})
}

// MarkChannelRead updates the user's last_read_at for a channel (upsert).
func (h *Handler) MarkChannelRead(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Verify user is a member of the channel's server, and load the channel type
	// so per-channel view visibility can be enforced. Restricting the row to the
	// caller's servers means a non-member (or a non-existent channel) is
	// indistinguishable — both yield sql.ErrNoRows.
	var serverID, channelType string
	err := h.db.QueryRow(
		`SELECT c.server_id, c.type
		 FROM channels c
		 INNER JOIN server_members sm ON c.server_id = sm.server_id
		 WHERE c.id = $1 AND sm.user_id = $2`,
		channelID, userID,
	).Scan(&serverID, &channelType)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
		return
	} else if err != nil {
		h.log.Error("Failed to check channel membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkChannelRead})
		return
	}

	// CV-CAN-002: server membership alone is insufficient — a member denied
	// channel visibility must not write read state for a hidden channel (this
	// mirrors the bulk MarkServerRead gating and closes the existence-oracle /
	// read-state-write vector for the per-channel path). Require the
	// type-appropriate view permission; deny with the same response a non-member
	// receives so a hidden channel cannot be distinguished.
	viewPerm := viewPermForType(channelType)
	canView, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, viewPerm)
	if permErr != nil {
		h.log.Error("Failed to check channel view permission", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkChannelRead})
		return
	}
	if !canView {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
		return
	}

	// Upsert read state
	_, err = h.db.Exec(
		`INSERT INTO channel_read_states (user_id, channel_id, last_read_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET last_read_at = NOW()`,
		userID, channelID,
	)
	if err != nil {
		h.log.Error("Failed to upsert read state", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkChannelRead})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Channel marked as read"})
}

// MarkServerRead marks all channels in a server as read for the user.
func (h *Handler) MarkServerRead(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkServerRead})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
		return
	}

	// CV-CAN-002: only write read state for channels the caller can view — do not
	// upsert read state for hidden channels the member is denied visibility on.
	visibleIDs, visErr := h.resolver.GetVisibleChannelIDs(c.Request.Context(), serverID, userID)
	if visErr != nil {
		h.log.Error(errMsgFailedResolveVisible, "error", visErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkServerRead})
		return
	}
	if len(visibleIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "All channels marked as read"})
		return
	}

	// Upsert read state for every VISIBLE channel in the server
	_, err = h.db.Exec(
		`INSERT INTO channel_read_states (user_id, channel_id, last_read_at)
		 SELECT $1, id, NOW() FROM channels WHERE server_id = $2 AND id = ANY($3::uuid[])
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET last_read_at = NOW()`,
		userID, serverID, pq.Array(visibleIDs),
	)
	if err != nil {
		h.log.Error("Failed to mark server read", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkServerRead})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "All channels marked as read"})
}

// GetChannelKeys returns the caller's wrapped channel key for an E2EE channel.
// channelKeyAccess reports whether channelID is a channel the user is a member
// of (isMember) and, if so, whether they hold the type-appropriate channel VIEW
// permission (canView). It is the authorization primitive for E2EE channel-key
// access (CV-CAN-005): server membership alone must not grant retrieval of a
// hidden channel's wrapped key material, enroll the member for its key
// distribution, or push a rotated key to them. When no RBAC resolver is
// configured (tests) it falls back to membership-only (canView == isMember).
// Returns isMember == false when channelID is not a channel or the user is not a
// member (callers treat that as "route to the DM branch / deny").
// sanitizeID strips CR/LF and other control characters from an id/label before
// it is logged (CWE-117 log-forging defense). Applied uniformly to logged
// user-derived strings — even structurally-safe uuids — per observability.md /
// #1645. Package-local to avoid importing the websocket helper.
func sanitizeID(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func (h *Handler) channelKeyAccess(ctx context.Context, channelID, userID string) (isMember, canView bool, err error) {
	var serverID, channelType string
	qErr := h.db.QueryRowContext(ctx, `
		SELECT c.server_id, c.type FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID, &channelType)
	if qErr == sql.ErrNoRows {
		return false, false, nil
	}
	if qErr != nil {
		return false, false, qErr
	}
	if h.resolver == nil {
		return true, true, nil
	}
	viewPerm := rbac.PermViewTextChannels
	if channelType == "voice" {
		viewPerm = rbac.PermViewVoiceChannels
	}
	allowed, permErr := h.resolver.HasPermission(ctx, serverID, userID, channelID, viewPerm)
	if permErr != nil {
		return true, false, permErr
	}
	return true, allowed, nil
}

// GetChannelKeys returns the caller's own wrapped channel key (optionally a
// specific ?version=N) for an E2EE channel they can view.
func (h *Handler) GetChannelKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// CV-CAN-005: channel-key fetch requires channel VIEW, not just server
	// membership — a hidden-channel member must not retrieve wrapped CSK material.
	// A non-member and a no-view member get the same 403 (no existence oracle).
	isMember, canView, err := h.channelKeyAccess(c.Request.Context(), channelID, userID)
	if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel keys"})
		return
	}
	if !isMember || !canView {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
		return
	}

	// Support ?version=N to fetch a specific key version (for decrypting old messages)
	var key models.ChannelKey
	if versionStr := c.Query("version"); versionStr != "" {
		var version int
		if _, scanErr := fmt.Sscanf(versionStr, "%d", &version); scanErr == nil && version > 0 {
			err = h.db.QueryRow(
				`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
				 FROM channel_keys
				 WHERE channel_id = $1 AND user_id = $2 AND key_version = $3`,
				channelID, userID, version,
			).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid version parameter"})
			return
		}
	} else {
		// Default: latest version
		err = h.db.QueryRow(
			`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
			 FROM channel_keys
			 WHERE channel_id = $1 AND user_id = $2
			 ORDER BY key_version DESC LIMIT 1`,
			channelID, userID,
		).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
	}
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgNoEncryptionKey, "pending": true})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key})
}

// DistributeChannelKeysRequest represents wrapped keys for new members
type DistributeChannelKeysRequest struct {
	WrappedKeys map[string]string `json:"wrapped_keys" binding:"required"` // user_id → wrapped CSK
	KeyVersion  *int              `json:"key_version,omitempty"`           // Explicit epoch for rotation (must be > current max)
	// WrappedKeyVersions carries the public_keys.key_version each CSK was wrapped
	// against (#2420). Optional and fail-open: a recipient with no entry keeps the
	// legacy bare insert. Shared by the channel and DM distribution paths (the DM
	// handler binds this same struct). Serves the recipient-freshness guard.
	WrappedKeyVersions map[string]int `json:"wrapped_key_versions,omitempty"`
}

// DistributeChannelKeys stores wrapped channel keys for new members (key distribution).
// Uses first-response-wins: if a key already exists for a (channel, user), returns 409.
func (h *Handler) DistributeChannelKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req DistributeChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if err := h.verifyChannelEncrypted(c.Request.Context(), channelID, userID); err != nil {
		h.respondKeyDistError(c, err)
		return
	}

	if !h.callerHasChannelKey(channelID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You must have the channel key to distribute keys"})
		return
	}

	targetKeyVersion := h.resolveTargetKeyVersion(channelID, req.KeyVersion)

	distributed, duplicates, skippedErrors, skippedStale, distErr := h.distributeChannelKeysToMembers(c.Request.Context(), userID, middleware.TokenCredentialEpoch(c), channelID, req.WrappedKeys, req.WrappedKeyVersions, targetKeyVersion)
	if distErr != nil {
		h.respondKeyDistributionError(c, distErr, channelID)
		return
	}

	h.log.Info("Channel keys distributed",
		"channel_id", channelID, "by_user", userID,
		"distributed", distributed, "duplicates", duplicates, "skipped", skippedErrors,
		"skipped_stale", skippedStale)

	// CV-CAN-005: distribution fails CLOSED — a channelKeyAccess resolver error
	// skips the target rather than leaking a key. Skipped targets are enrolled
	// into pending_key_requests for peer retry, but returning 200 here would let
	// the caller (e.g. rotateChannelKey, which only checks res.ok and then
	// invalidates its cache) treat a degraded rotation as fully successful and
	// leave skipped members on a stale epoch with no synchronous signal. Return
	// 503 so res.ok is false and the caller can retry the rotation; the counts
	// stay in the body for observability.
	if skippedErrors > 0 {
		h.log.Warn("key distribution: targets skipped due to view-check errors (degraded rotation)",
			"channel_id", sanitizeID(channelID), "by_user", sanitizeID(userID),
			"skipped_errors", skippedErrors, "distributed", distributed)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":         "Some targets could not be verified and were skipped; retry",
			"distributed":   distributed,
			"duplicates":    duplicates,
			"skipped":       skippedErrors,
			"skipped_stale": skippedStale,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"distributed":   distributed,
		"duplicates":    duplicates,
		"skipped":       skippedErrors,
		"skipped_stale": skippedStale,
	})
}

type keyDistError struct {
	status  int
	message string
}

func (e *keyDistError) Error() string { return e.message }

func (h *Handler) verifyChannelEncrypted(ctx context.Context, channelID, userID string) error {
	// All channels are encrypted under E2EE-everywhere (#201). CV-CAN-005: the
	// distributor must currently hold channel VIEW, not merely server membership
	// plus a (possibly stale) key. Without this a hidden-channel member who kept
	// an old key row could still POST wraps/rotations for visible targets (and
	// DistributeUnifiedKeys delegates here after a membership-only probe, so the
	// same gap applies there). Fail closed on a resolver error — do not fall back
	// to membership-only on the write path.
	isMember, canView, err := h.channelKeyAccess(ctx, channelID, userID)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		return &keyDistError{http.StatusInternalServerError, errMsgFailedDistributeKeys}
	}
	if !isMember || !canView {
		return &keyDistError{http.StatusForbidden, "Not a member of this channel's server"}
	}
	return nil
}

func (h *Handler) respondKeyDistError(c *gin.Context, err error) {
	if kde, ok := err.(*keyDistError); ok {
		c.JSON(kde.status, gin.H{"error": kde.message})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDistributeKeys})
}

func (h *Handler) callerHasChannelKey(channelID, userID string) bool {
	var hasKey bool
	_ = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_keys WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&hasKey)
	return hasKey
}

func (h *Handler) resolveTargetKeyVersion(channelID string, explicitVersion *int) int {
	if explicitVersion != nil && *explicitVersion > 0 {
		return *explicitVersion
	}
	var v int
	_ = h.db.QueryRow(
		`SELECT COALESCE(MAX(key_version), 1) FROM channel_keys WHERE channel_id = $1`,
		channelID,
	).Scan(&v)
	return v
}

// distributionTargetAdmitted applies the CV-CAN-005 gate for one distribution
// target: do not distribute a channel key to a target lacking channel VIEW (or
// that is not a member). Fail CLOSED — skip on a definite deny AND on a
// resolver error. A persistent resolver failure must not silently degrade this
// security gate back to membership-only. Distribution is retryable and
// eventually consistent: a target skipped on a transient error is enrolled
// into the peer-fulfillment queue (idempotent) so a viewer re-delivers the key
// once the resolver recovers; fulfillment re-checks VIEW, so this cannot
// re-open CV-CAN-005 for a genuinely no-view user.
func (h *Handler) distributionTargetAdmitted(ctx context.Context, channelID, memberUserID string) (admitted, skippedOnError bool) {
	_, canView, vErr := h.channelKeyAccess(ctx, channelID, memberUserID)
	if vErr != nil {
		if _, enrollErr := h.enrollPending("channel", channelID, memberUserID); enrollErr != nil {
			h.log.Error("key distribution: failed to enroll skipped target for retry",
				"error", enrollErr, "channel_id", sanitizeID(channelID), "user_id", sanitizeID(memberUserID))
		}
		h.log.Error("key distribution: view check failed; skipping target (fail closed, enrolled for retry)", "error", vErr, "user_id", sanitizeID(memberUserID))
		return false, true
	}
	return canView, false
}

// distributionOutcome classifies one target of a channel-key distribution.
type distributionOutcome int

const (
	distributionSkipped distributionOutcome = iota
	distributionSkippedOnError
	distributionDuplicate
	distributionDelivered
	// distributionSkippedStale: the recipient's public key rotated after the
	// distributor wrapped this CSK (#2420). The insert is refused and a self-heal
	// re-request is enqueued; no key_delivered notification fires.
	distributionSkippedStale
)

// recipientKeyFresh reports whether the recipient's current public-key version
// still equals the version the distributor wrapped the CSK against (#2420). The
// FOR SHARE lock is load-bearing: it conflicts with a concurrent key-reset's
// FOR NO KEY UPDATE on the same public_keys row (key_version is not a key
// column), so the compare-then-insert can no longer straddle the reset — under
// READ COMMITTED a bare snapshot SELECT would NOT serialize. A missing row means
// the recipient has no key to wrap to, so treat it as not-fresh. The caller must
// hold the distribution transaction.
func recipientKeyFresh(ctx context.Context, tx *sql.Tx, userID string, wrappedVersion int) (bool, error) {
	var current int
	err := tx.QueryRowContext(ctx,
		`SELECT key_version FROM public_keys
		 WHERE user_id = $1 ORDER BY key_version DESC LIMIT 1
		 FOR SHARE`, userID).Scan(&current)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("recipient key freshness read: %w", err)
	}
	return current == wrappedVersion, nil
}

// enqueueChannelKeyRequest re-requests distribution for a recipient skipped as
// stale (#2420 self-heal). Idempotent against the existing retry loop; ON
// CONFLICT DO NOTHING never raises a statement error, so it cannot poison the
// batch transaction.
func enqueueChannelKeyRequest(ctx context.Context, tx *sql.Tx, channelID, userID string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO pending_key_requests (channel_id, user_id)
		 VALUES ($1, $2) ON CONFLICT (channel_id, user_id) DO NOTHING`,
		channelID, userID)
	return err
}

// distributeOneChannelKey processes one distribution target inside the caller's
// epoch-guarded transaction: the CV-CAN-005 admission gate, the #2420
// recipient-freshness guard, and the idempotent insert (the caller validates the
// member UUID). The freshness guard runs only when the distributor supplied the
// wrapped-against public-key version (fail-open for old clients) and serializes
// against a concurrent ReplaceMyKeys/RecoveryResetAccount so a stale wrapped key
// cannot re-create a row the reset just purged. A statement error poisons the
// whole PG transaction (25P02), so insert failures fail the batch via the error.
func (h *Handler) distributeOneChannelKey(ctx context.Context, tx *sql.Tx, channelID, memberUserID, wrappedKey string, wrappedKeyVersions map[string]int, keyVersion int) (distributionOutcome, error) {
	admitted, skippedOnError := h.distributionTargetAdmitted(ctx, channelID, memberUserID)
	if skippedOnError {
		return distributionSkippedOnError, nil
	}
	if !admitted {
		return distributionSkipped, nil
	}

	if wrappedVersion, ok := wrappedKeyVersions[memberUserID]; ok {
		fresh, fErr := recipientKeyFresh(ctx, tx, memberUserID, wrappedVersion)
		if fErr != nil {
			return distributionSkipped, fErr
		}
		if !fresh {
			if eErr := enqueueChannelKeyRequest(ctx, tx, channelID, memberUserID); eErr != nil {
				return distributionSkipped, fmt.Errorf("enqueue self-heal: %w", eErr)
			}
			return distributionSkippedStale, nil
		}
	}

	inserted, insErr := insertWrappedChannelKeyTx(ctx, tx, channelID, memberUserID, wrappedKey, keyVersion)
	if insErr != nil {
		h.log.Error("Failed to store key for member", "error", insErr, "user_id", memberUserID)
		return distributionSkipped, fmt.Errorf("store key: %w", insErr)
	}
	if !inserted {
		return distributionDuplicate, nil
	}
	return distributionDelivered, nil
}

// insertWrappedChannelKeyTx inserts one wrapped key inside the caller's
// epoch-guarded transaction. inserted=false with a nil error means the
// (channel, user, version) row already existed.
func insertWrappedChannelKeyTx(ctx context.Context, tx *sql.Tx, channelID, memberUserID, wrappedKey string, keyVersion int) (inserted bool, err error) {
	result, err := tx.ExecContext(ctx,
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (channel_id, user_id, key_version) DO NOTHING`,
		channelID, memberUserID, wrappedKey, keyVersion,
	)
	if err != nil {
		return false, err
	}
	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		// #2201 review: a RowsAffected error means we can't tell whether the
		// wrapped-key row landed. Fail the batch (caller → 500) rather than
		// silently classify it not-delivered and skip the delivered notification.
		return false, fmt.Errorf("rows affected: %w", raErr)
	}
	return rowsAffected > 0, nil
}

// respondKeyDistributionError maps a distribution failure onto the wire (#2201):
// epoch-fence rejections get the middleware-identical generic 401; anything
// else (tx begin/commit, statement failures) is a 500.
func (h *Handler) respondKeyDistributionError(c *gin.Context, distErr error, contextID string) {
	if errors.Is(distErr, credepoch.ErrEpochMismatch) || errors.Is(distErr, credepoch.ErrBlocked) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgAuthRequired})
		return
	}
	h.log.Error("Key distribution failed", "error", distErr, "context_id", sanitizeID(contextID))
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDistributeKeys})
}

// distributeChannelKeysToMembers writes wrapped keys inside ONE transaction
// guarded by the distributing actor's credential epoch (#2201): a distributor
// admitted before a destructive key reset cannot recreate wrapped-key rows
// after it (GuardTx FOR SHARE serializes against the reset's user-row lock).
// key_delivered notifications fire only after commit so a notified client
// never fetches an uncommitted row. A guard/transaction failure returns an
// error (the caller maps epoch errors to 401); per-member insert errors keep
// their existing skip semantics.
func (h *Handler) distributeChannelKeysToMembers(ctx context.Context, actorID, tokenEpoch, channelID string, wrappedKeys map[string]string, wrappedKeyVersions map[string]int, keyVersion int) (distributed, duplicates, skippedErrors, skippedStale int, err error) {
	// #2201 review: run on the request context so a client disconnect cancels a
	// GuardTx FOR SHARE lock-wait (which blocks against a destructive reset's
	// FOR NO KEY UPDATE) instead of pinning a pooled connection with no deadline.
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("begin distribution tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback distribution tx", "error", rbErr)
		}
	}()
	if guardErr := credepoch.GuardTx(ctx, tx, actorID, tokenEpoch); guardErr != nil {
		return 0, 0, 0, 0, guardErr
	}
	var tally channelDistributionTally
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}
		outcome, distErr := h.distributeOneChannelKey(ctx, tx, channelID, memberUserID, wrappedKey, wrappedKeyVersions, keyVersion)
		if distErr != nil {
			return 0, 0, 0, 0, distErr
		}
		tally.record(outcome, memberUserID)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("commit distribution tx: %w", err)
	}
	for _, memberUserID := range tally.delivered {
		// Pending-request cleanup is best-effort POST-commit: the key row is
		// already durable, and a failed DELETE inside the tx would poison the
		// whole batch (25P02) for what is only retry-safe housekeeping.
		_, _ = h.db.Exec(
			`DELETE FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
			channelID, memberUserID,
		)
		h.notifyKeyDelivered(channelID, memberUserID)
	}
	return tally.distributed, tally.duplicates, tally.skippedErrors, tally.skippedStale, nil
}

// channelDistributionTally accumulates per-target outcomes for a channel-key
// distribution. Split out (with record) to keep distributeChannelKeysToMembers
// under the cognitive-complexity ceiling.
type channelDistributionTally struct {
	distributed, duplicates, skippedErrors, skippedStale int
	delivered                                            []string
}

func (t *channelDistributionTally) record(outcome distributionOutcome, memberUserID string) {
	switch outcome {
	case distributionDelivered:
		t.delivered = append(t.delivered, memberUserID)
		t.distributed++
	case distributionDuplicate:
		t.duplicates++
	case distributionSkippedOnError:
		t.skippedErrors++
	case distributionSkippedStale:
		// Guarded out (#2420): recipient rotated after wrap. Counted apart from
		// skippedErrors — this is a deterministic skip (retrying the SAME stale
		// wrap won't help; the in-tx self-heal enqueue re-requests a fresh wrap),
		// so it must NOT trip the transient-retry 503 the caller raises for
		// skippedErrors. No key_delivered notification fires.
		t.skippedStale++
	case distributionSkipped:
	}
}

func (h *Handler) notifyKeyDelivered(contextID, memberUserID string) {
	if h.hub == nil {
		return
	}
	recipientUUID, err := uuid.Parse(memberUserID)
	if err != nil {
		return
	}
	h.hub.BroadcastToUser(recipientUUID, websocket.OutgoingMessage{
		Type: "key_delivered",
		Data: map[string]interface{}{
			"channel_id": contextID,
			"user_id":    memberUserID,
		},
	})
}

// GetPendingKeyRequests returns pending key requests for channels the caller can service.
// pendingKeyRequest is a single pending E2EE key request row (channel or DM)
// returned by GetPendingKeyRequests.
type pendingKeyRequest struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	CreatedAt string `json:"created_at"`
}

// GetPendingKeyRequests lists pending channel-key requests the caller may
// service, filtered fail-closed to channels the caller can view and requesters
// who still hold view permission (CV-CAN-005).
// GET /api/v1/e2ee/pending-keys
func (h *Handler) GetPendingKeyRequests(c *gin.Context) {
	userID := c.GetString("user_id")

	// Return pending requests for channels where the caller already has a key
	query := `
		SELECT pkr.id, pkr.channel_id, pkr.user_id, pkr.created_at
		FROM pending_key_requests pkr
		INNER JOIN channel_keys ck ON pkr.channel_id = ck.channel_id AND ck.user_id = $1
		ORDER BY pkr.created_at ASC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query pending key requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch pending key requests"})
		return
	}
	// Drain the cursor fully before running the per-request VIEW filter below.
	// The filter calls channelKeyAccess (a DB query + RBAC resolver lookup) per
	// row, so filtering inline would hold this cursor's pooled connection open
	// for the duration of N round-trips (CV-CAN-005 review). Collect first, then
	// close, then filter.
	candidates := make([]pendingKeyRequest, 0)
	for rows.Next() {
		var req pendingKeyRequest
		if err := rows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.CreatedAt); err != nil {
			h.log.Error("Failed to scan pending request", "error", err)
			continue
		}
		candidates = append(candidates, req)
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil {
		h.log.Error("Error iterating pending requests", "error", rowsErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch pending key requests"})
		return
	}

	requests := h.filterVisiblePendingRequests(c.Request.Context(), userID, candidates)
	requests = h.appendDMPendingRequests(userID, requests)

	c.JSON(http.StatusOK, gin.H{"pending_requests": requests})
}

// filterVisiblePendingRequests drops channel pending requests the caller may no
// longer VIEW. The source query selected channels solely because the CALLER
// holds a channel_keys row; CV-CAN-005 requires that a caller who received a key
// and was later denied VIEW is neither shown nor prompted to fulfill a hidden
// channel's pending queue. Gate the caller's own VIEW per channel (memoised —
// many pending requests share a channel), then also drop requests whose
// requester no longer holds VIEW (defence in depth). Fail CLOSED on a resolver
// error so a persistent failure cannot re-expose a hidden channel's queue.
func (h *Handler) filterVisiblePendingRequests(ctx context.Context, callerID string, candidates []pendingKeyRequest) []pendingKeyRequest {
	requests := []pendingKeyRequest{}
	callerCanView := make(map[string]bool, len(candidates))
	for _, req := range candidates {
		canView, known := callerCanView[req.ChannelID]
		if !known {
			_, cv, vErr := h.channelKeyAccess(ctx, req.ChannelID, callerID)
			canView = vErr == nil && cv
			callerCanView[req.ChannelID] = canView
		}
		if !canView {
			continue
		}
		if _, cv, vErr := h.channelKeyAccess(ctx, req.ChannelID, req.UserID); vErr != nil || !cv {
			continue
		}
		requests = append(requests, req)
	}
	return requests
}

// appendDMPendingRequests appends the caller's DM pending key requests to
// requests. DM membership is scoped by the dm_channel_keys join, so no extra
// VIEW gate applies here.
func (h *Handler) appendDMPendingRequests(userID string, requests []pendingKeyRequest) []pendingKeyRequest {
	dmQuery := `
		SELECT dpkr.id, dpkr.conversation_id, dpkr.user_id, dpkr.created_at
		FROM dm_pending_key_requests dpkr
		INNER JOIN dm_channel_keys dck ON dpkr.conversation_id = dck.conversation_id AND dck.user_id = $1
		WHERE dpkr.user_id != $1
		ORDER BY dpkr.created_at ASC
	`
	dmRows, dmErr := h.db.Query(dmQuery, userID)
	if dmErr != nil {
		return requests
	}
	defer func() { _ = dmRows.Close() }()
	for dmRows.Next() {
		var req pendingKeyRequest
		if err := dmRows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.CreatedAt); err != nil {
			continue
		}
		requests = append(requests, req)
	}
	return requests
}

// GetUnifiedKeys resolves a context_id to either a server channel or DM conversation
// and returns the caller's wrapped key.
//
// Side effect: on the 404 NO_KEY_YET path (key row missing), auto-enrolls the
// caller into pending_key_requests / dm_pending_key_requests so peers can
// fulfill via DistributeUnifiedKeys (#1023). Idempotent via ON CONFLICT DO NOTHING.
//
// GET /e2ee/keys/:context_id
func (h *Handler) GetUnifiedKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	contextID := c.Param("context_id")

	if _, err := uuid.Parse(contextID); err != nil {
		c.JSON(http.StatusBadRequest, e2eekeys.ErrorResponse{
			Error: errMsgInvalidContextID,
			Code:  e2eekeys.CodeInvalidRequest,
			Kind:  e2eekeys.KindUnknown,
		})
		return
	}

	// Under E2EE-everywhere (#201) all channels are encrypted; membership +
	// channel VIEW (CV-CAN-005) gate channel-key access. A member without VIEW is
	// routed to the DM branch, which returns not-found — so a hidden channel is
	// indistinguishable from a non-existent context (no existence oracle).
	isMember, canView, err := h.channelKeyAccess(c.Request.Context(), contextID, userID)
	if err != nil {
		h.log.Error("e2ee key fetch: channel check failed",
			"kind", "channel_check_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, e2eekeys.ErrorResponse{
			Error: errMsgFailedFetchKeys,
			Code:  e2eekeys.CodeInternalError,
			Kind:  e2eekeys.KindUnknown,
		})
		return
	}
	if isMember && canView {
		h.getChannelKeyResponse(c, contextID, userID)
		return
	}

	h.getDMKeyResponse(c, contextID, userID)
}

// enrollPending inserts an idempotent (context, user) row into the pending
// table corresponding to the kind. Returns true if a new row was inserted,
// false if the insert was a duplicate (silent enrollment).
//
// Uses pre-written parameterized SQL strings selected by a switch on the
// kind argument — no fmt.Sprintf, no string concatenation. Per
// [internal]rules/backend.md, SQL statements must be parameterized with
// $1, $2; this helper preserves that rule while still presenting a single
// call shape to enrollChannelRewrap / enrollDMRewrap and the
// getChannelKeyResponse / getDMKeyResponse auto-enroll paths.
//
// Used by RequestRewrap (explicit POST /rewrap path) and by
// getChannelKeyResponse / getDMKeyResponse (auto-enroll on 404 path).
// Logging is delegated to callers — they have different log contexts.
func (h *Handler) enrollPending(kind, contextID, userID string) (inserted bool, err error) {
	var query string
	switch kind {
	case "channel":
		query = `INSERT INTO pending_key_requests (channel_id, user_id)
		         VALUES ($1, $2)
		         ON CONFLICT (channel_id, user_id) DO NOTHING`
	case "dm":
		query = `INSERT INTO dm_pending_key_requests (conversation_id, user_id)
		         VALUES ($1, $2)
		         ON CONFLICT (conversation_id, user_id) DO NOTHING`
	default:
		return false, fmt.Errorf("enrollPending: unknown kind %q", kind)
	}
	result, execErr := h.db.Exec(query, contextID, userID)
	if execErr != nil {
		return false, execErr
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

// RequestRewrap enrolls the caller into the peer-fulfillment queue for a
// missing channel/DM key. Idempotent: ON CONFLICT DO NOTHING.
// POST /api/v1/e2ee/keys/:context_id/rewrap
//
// Security (per [internal]rules/e2ee.md):
//   - Takes NO request body. Server uses existing peer-fulfillment flow
//     which relies on ALREADY-STORED pubkeys; no client-supplied pubkey
//     is ever consumed.
//   - RBAC before any DB write: server member (channel) or DM participant.
//   - Rate-limited per-user at the route layer (10/min).
//   - Logs context_id, user_id, action only. No key material.
//
// The channel vs DM branches are extracted into enrollChannelRewrap and
// enrollDMRewrap helpers to keep cognitive complexity under the SonarQube
// threshold of 15 (S3776) and eliminate Block B duplication between the
// two enrollment paths.
func (h *Handler) RequestRewrap(c *gin.Context) {
	userID := c.GetString("user_id")
	contextID := c.Param("context_id")

	if _, err := uuid.Parse(contextID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidContextID})
		return
	}

	// Resolve channel vs DM. CV-CAN-005: channel-key rewrap enrollment requires
	// channel VIEW, not just membership — a hidden-channel member must not enroll
	// for its key distribution. A no-view member routes to the DM branch, which
	// returns not-found (no existence oracle).
	isChannel, canView, err := h.channelKeyAccess(c.Request.Context(), contextID, userID)
	if err != nil {
		h.log.Error("re_wrap_request: channel check failed",
			"kind", "re_wrap_check_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedProcessRewrap})
		return
	}

	if isChannel && canView {
		h.enrollChannelRewrap(c, contextID, userID)
		return
	}

	// CV-CAN-005: a member without channel VIEW must be indistinguishable from a
	// caller probing a non-existent context. Falling through to enrollDMRewrap
	// would reach respondNotMemberOrUnknown, which returns 403 for an existing
	// (hidden) channel but 404 for an unknown context — an existence oracle. Emit
	// the same 404 an unknown context yields instead of leaking existence via 403.
	if isChannel {
		h.log.Info("re_wrap_request: no-view channel member",
			"kind", "re_wrap_no_view",
			"context_id", sanitizeID(contextID),
			"user_id", sanitizeID(userID))
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgContextNotFound})
		return
	}

	h.enrollDMRewrap(c, contextID, userID)
}

// enrollChannelRewrap handles the channel half of RequestRewrap: inserts a row
// into pending_key_requests (idempotent via ON CONFLICT DO NOTHING) and emits
// the structured log + 202 response.
//
// Extracted from RequestRewrap to keep cognitive complexity under the SonarQube
// S3776 threshold of 15 and eliminate code duplication with enrollDMRewrap.
func (h *Handler) enrollChannelRewrap(c *gin.Context, contextID, userID string) {
	inserted, enrollErr := h.enrollPending("channel", contextID, userID)
	if enrollErr != nil {
		h.log.Error("re_wrap_request: channel enrollment insert failed",
			"kind", "re_wrap_insert_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", enrollErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedEnrollRewrap})
		return
	}
	h.emitEnrollResult(c, contextID, userID, "channel", inserted)
}

// enrollDMRewrap handles the DM half of RequestRewrap: verifies participation,
// distinguishes unknown-context (404) from non-participant (403), and inserts
// into dm_pending_key_requests on success.
//
// Extracted from RequestRewrap to keep cognitive complexity under the SonarQube
// S3776 threshold of 15 and eliminate code duplication with enrollChannelRewrap.
func (h *Handler) enrollDMRewrap(c *gin.Context, contextID, userID string) {
	var isDMParticipant bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM dm_conversations dc
			INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
			WHERE dc.id = $1
		)
	`, contextID, userID).Scan(&isDMParticipant)
	if err != nil {
		h.log.Error("re_wrap_request: dm check failed",
			"kind", "re_wrap_check_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedProcessRewrap})
		return
	}

	if !isDMParticipant {
		h.respondNotMemberOrUnknown(c, contextID, userID)
		return
	}

	inserted, enrollErr := h.enrollPending("dm", contextID, userID)
	if enrollErr != nil {
		h.log.Error("re_wrap_request: dm enrollment insert failed",
			"kind", "re_wrap_insert_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", enrollErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedEnrollRewrap})
		return
	}
	h.emitEnrollResult(c, contextID, userID, "dm", inserted)
}

// emitEnrollResult writes the structured log line corresponding to whether
// the insert happened (re_wrap_enrolled vs re_wrap_already_enrolled) and
// sends the 202 response. Extracted to deduplicate the symmetric tails of
// enrollChannelRewrap and enrollDMRewrap (SonarQube duplication threshold).
func (h *Handler) emitEnrollResult(c *gin.Context, contextID, userID, contextKind string, inserted bool) {
	if inserted {
		h.log.Info("re_wrap_enrolled",
			"kind", "re_wrap_enrolled",
			"context_id", contextID,
			"user_id", userID,
			"context_kind", contextKind)
	} else {
		h.log.Info("re_wrap_already_enrolled",
			"kind", "re_wrap_already_enrolled",
			"context_id", contextID,
			"user_id", userID,
			"context_kind", contextKind)
	}
	c.JSON(http.StatusAccepted, gin.H{"enrolled": true, "kind": contextKind})
}

// respondNotMemberOrUnknown distinguishes "context doesn't exist" (404) from
// "caller isn't a member or participant" (403) when neither the channel-member
// nor DM-participant checks matched. Extracted so enrollDMRewrap stays under
// the cognitive-complexity threshold (S3776).
func (h *Handler) respondNotMemberOrUnknown(c *gin.Context, contextID, userID string) {
	var contextExists bool
	ceErr := h.db.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1)
		OR EXISTS(SELECT 1 FROM dm_conversations WHERE id = $1)
	`, contextID).Scan(&contextExists)
	if ceErr != nil {
		// error field included for incident triage; per observability.md this is not key material.
		h.log.Error("re_wrap_request: context existence check failed",
			"kind", "re_wrap_check_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", ceErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedProcessRewrap})
		return
	}
	if !contextExists {
		h.log.Info("re_wrap_request: unknown context",
			"kind", "re_wrap_unknown_context",
			"context_id", contextID,
			"user_id", userID)
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgContextNotFound})
		return
	}
	h.log.Info("re_wrap_request: not member or participant",
		"kind", "re_wrap_not_member",
		"context_id", contextID,
		"user_id", userID)
	c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOrParticipant})
}

func (h *Handler) getChannelKeyResponse(c *gin.Context, contextID, userID string) {
	key, err := h.fetchChannelKey(contextID, userID, c.Query("version"))
	if err == errInvalidVersion {
		c.JSON(http.StatusBadRequest, e2eekeys.ErrorResponse{
			Error: "Invalid version parameter",
			Code:  e2eekeys.CodeInvalidRequest,
			Kind:  e2eekeys.KindChannel,
		})
		return
	}
	if err == sql.ErrNoRows {
		// Auto-enroll caller into pending_key_requests (#1023 — missing-wrap recovery).
		// Idempotent via ON CONFLICT DO NOTHING. Complement to POST /rewrap
		// (RequestRewrap): gives immediate enrollment without requiring a second
		// round-trip from the client.
		inserted, enrollErr := h.enrollPending("channel", contextID, userID)
		if enrollErr != nil {
			// Log but don't fail the request — the 404+pending response is the
			// contract; auto-enroll is a defense-in-depth side effect.
			h.log.Error("auto-enroll pending channel insert failed",
				"kind", "auto_enroll_insert_db_error",
				"context_id", contextID,
				"user_id", userID,
				"error", enrollErr)
		} else if inserted {
			h.log.Info("auto-enrolled pending channel request",
				"kind", "enroll_pending_channel",
				"context_id", contextID,
				"user_id", userID)
		}

		h.log.Info("e2ee key fetch: no channel key row",
			"kind", "no_channel_key_row",
			"context_id", contextID,
			"user_id", userID,
			"version", c.Query("version"))
		c.JSON(http.StatusNotFound, e2eekeys.ErrorResponse{
			Error:   errMsgNoEncryptionKey,
			Code:    e2eekeys.CodeNoKeyYet,
			Kind:    e2eekeys.KindChannel,
			Pending: true,
		})
		return
	}
	if err != nil {
		h.log.Error("e2ee key fetch: channel key query failed",
			"kind", "channel_key_fetch_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, e2eekeys.ErrorResponse{
			Error: errMsgFailedFetchKeys,
			Code:  e2eekeys.CodeInternalError,
			Kind:  e2eekeys.KindUnknown,
		})
		return
	}

	c.JSON(http.StatusOK, e2eekeys.KeyResponse{
		Key: e2eekeys.KeyPayload{
			WrappedKey: key.WrappedKey,
			KeyVersion: key.KeyVersion,
		},
		Kind: e2eekeys.KindChannel,
	})
}

var errInvalidVersion = fmt.Errorf("invalid version parameter")

func (h *Handler) fetchChannelKey(channelID, userID, versionStr string) (models.ChannelKey, error) {
	var key models.ChannelKey
	if versionStr != "" {
		var version int
		if _, scanErr := fmt.Sscanf(versionStr, "%d", &version); scanErr != nil || version <= 0 {
			return key, errInvalidVersion
		}
		err := h.db.QueryRow(
			`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
			 FROM channel_keys
			 WHERE channel_id = $1 AND user_id = $2 AND key_version = $3`,
			channelID, userID, version,
		).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
		return key, err
	}
	err := h.db.QueryRow(
		`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
		 FROM channel_keys
		 WHERE channel_id = $1 AND user_id = $2
		 ORDER BY key_version DESC LIMIT 1`,
		channelID, userID,
	).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
	return key, err
}

type dmKey struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	UserID         string `json:"user_id"`
	WrappedKey     string `json:"wrapped_key"`
	KeyVersion     int    `json:"key_version"`
	CreatedAt      string `json:"created_at"`
}

func (h *Handler) getDMKeyResponse(c *gin.Context, contextID, userID string) {
	// Under E2EE-everywhere (#201) all DMs are encrypted; check membership/existence only.
	var exists bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM dm_conversations dc
			INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
			WHERE dc.id = $1
		)
	`, contextID, userID).Scan(&exists)

	if err != nil {
		h.log.Error("e2ee key fetch: DM check failed",
			"kind", "dm_check_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, e2eekeys.ErrorResponse{
			Error: errMsgFailedFetchKeys,
			Code:  e2eekeys.CodeInternalError,
			Kind:  e2eekeys.KindUnknown,
		})
		return
	}
	if !exists {
		h.log.Info("e2ee key fetch: context not found or user not authorized",
			"kind", "context_not_found_or_forbidden",
			"context_id", contextID,
			"user_id", userID)
		c.JSON(http.StatusNotFound, e2eekeys.ErrorResponse{
			Error: errMsgContextNotFoundOrDenied,
			Code:  e2eekeys.CodeNotMember,
			Kind:  e2eekeys.KindUnknown,
		})
		return
	}

	var key dmKey
	err = h.db.QueryRow(`
		SELECT id, conversation_id, user_id, wrapped_key, key_version, created_at
		FROM dm_channel_keys
		WHERE conversation_id = $1 AND user_id = $2
		ORDER BY key_version DESC LIMIT 1
	`, contextID, userID).Scan(&key.ID, &key.ConversationID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)

	if err == sql.ErrNoRows {
		// Auto-enroll caller into dm_pending_key_requests (#1023).
		// Mirror of getChannelKeyResponse auto-enroll path.
		inserted, enrollErr := h.enrollPending("dm", contextID, userID)
		if enrollErr != nil {
			h.log.Error("auto-enroll pending dm insert failed",
				"kind", "auto_enroll_insert_db_error",
				"context_id", contextID,
				"user_id", userID,
				"error", enrollErr)
		} else if inserted {
			h.log.Info("auto-enrolled pending dm request",
				"kind", "enroll_pending_dm",
				"context_id", contextID,
				"user_id", userID)
		}

		h.log.Info("e2ee key fetch: no DM key row",
			"kind", "no_dm_key_row",
			"context_id", contextID,
			"user_id", userID)
		c.JSON(http.StatusNotFound, e2eekeys.ErrorResponse{
			Error:   errMsgNoEncryptionKey,
			Code:    e2eekeys.CodeNoKeyYet,
			Kind:    e2eekeys.KindDM,
			Pending: true,
		})
		return
	}
	if err != nil {
		h.log.Error("e2ee key fetch: DM key query failed",
			"kind", "dm_key_fetch_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, e2eekeys.ErrorResponse{
			Error: errMsgFailedFetchKeys,
			Code:  e2eekeys.CodeInternalError,
			Kind:  e2eekeys.KindUnknown,
		})
		return
	}

	// Epoch revocation check — if the caller's current key_version appears
	// in dm_key_revocations as revoked_epoch, return REVOKED_EPOCH so the
	// client triggers a rekey flow instead of trying to use stale wrap bytes.
	// Per [internal]rules/e2ee.md: epoch numbers do NOT appear in the response.
	var revokedExists bool
	err = h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM dm_key_revocations
			WHERE conversation_id = $1 AND revoked_epoch = $2
		)
	`, contextID, key.KeyVersion).Scan(&revokedExists)
	if err != nil {
		h.log.Error("e2ee key fetch: dm revocation check failed",
			"kind", "dm_revocation_check_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, e2eekeys.ErrorResponse{
			Error: errMsgFailedFetchKeys,
			Code:  e2eekeys.CodeInternalError,
			Kind:  e2eekeys.KindUnknown,
		})
		return
	}
	if revokedExists {
		h.log.Info("e2ee key fetch: dm epoch revoked",
			"kind", "dm_epoch_revoked",
			"context_id", contextID,
			"user_id", userID)
		c.JSON(http.StatusNotFound, e2eekeys.ErrorResponse{
			Error: "Key epoch has been revoked; rekey required",
			Code:  e2eekeys.CodeRevokedEpoch,
			Kind:  e2eekeys.KindDM,
		})
		return
	}

	c.JSON(http.StatusOK, e2eekeys.KeyResponse{
		Key: e2eekeys.KeyPayload{
			WrappedKey: key.WrappedKey,
			KeyVersion: key.KeyVersion,
		},
		Kind: e2eekeys.KindDM,
	})
}

// DistributeUnifiedKeys resolves a context_id and distributes wrapped keys.
// POST /e2ee/keys/:context_id
func (h *Handler) DistributeUnifiedKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	contextID := c.Param("context_id")

	if _, err := uuid.Parse(contextID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidContextID})
		return
	}

	// Don't parse the request body yet — route to channel or DM first.
	// DistributeChannelKeys will parse the body itself; parsing here would
	// consume the one-shot io.ReadCloser, causing a double-read 400 on delegation.

	// CV-CAN-005: gate the channel branch on channel VIEW, not mere server
	// membership. Delegating to DistributeChannelKeys for a member who lacks VIEW
	// would return 403, while an unknown context falls through to the DM branch
	// below and returns 404 — a hidden-channel existence oracle. Mirror the
	// unified GET/rewrap paths: a no-view member gets the same 404 as an unknown
	// context. channelKeyAccess fails closed on a resolver error (returns err),
	// matching the 500 those paths emit.
	isMember, canView, err := h.channelKeyAccess(c.Request.Context(), contextID, userID)
	if err != nil {
		h.log.Error("unified key distribution: channel check failed",
			"kind", "distribute_channel_check_db_error",
			"context_id", sanitizeID(contextID),
			"user_id", sanitizeID(userID),
			"error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDistributeKeys})
		return
	}

	if isMember && canView {
		c.Params = append(c.Params, gin.Param{Key: "id", Value: contextID})
		h.DistributeChannelKeys(c)
		return
	}

	if isMember {
		// Server member without channel VIEW: do not reveal the hidden channel's
		// existence via a 403. Emit the same not-found the DM branch yields for an
		// unknown context.
		h.log.Info("unified key distribution: no-view channel member",
			"kind", "distribute_no_view",
			"context_id", sanitizeID(contextID),
			"user_id", sanitizeID(userID))
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgContextNotFoundOrDenied})
		return
	}

	var isDM bool
	err = h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM dm_conversations dc
			INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
			WHERE dc.id = $1
		)
	`, contextID, userID).Scan(&isDM)
	if err != nil || !isDM {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgContextNotFoundOrDenied})
		return
	}

	var req DistributeChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	distributed, distErr := h.distributeDMKeys(c.Request.Context(), userID, middleware.TokenCredentialEpoch(c), contextID, req.WrappedKeys, req.WrappedKeyVersions, req.KeyVersion)
	if distErr != nil {
		h.respondKeyDistributionError(c, distErr, contextID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"distributed": distributed, "context_type": "dm"})
}

// resolveTargetKeyVersionDM mirrors resolveTargetKeyVersion for DM conversations.
// Returns the explicit version if the caller provided one > 0 (rotation path);
// otherwise returns the EXISTING max version so peer-fulfilled wraps of the
// cached CSK get tagged at the same epoch as established participants. For a
// brand-new conversation with no key rows, returns 1.
//
// CRITICAL: this MUST NOT compute MAX+1 on the fallback path. Stamping a peer
// fulfillment at a new version would break history decryption — the recovering
// user would get a row tagged at a version no historical message references.
// See PR #1080 / issue #1023.
func (h *Handler) resolveTargetKeyVersionDM(conversationID string, explicitVersion *int) int {
	if explicitVersion != nil && *explicitVersion > 0 {
		return *explicitVersion
	}
	var v int
	_ = h.db.QueryRow(
		`SELECT COALESCE(MAX(key_version), 1) FROM dm_channel_keys WHERE conversation_id = $1`,
		conversationID,
	).Scan(&v)
	if v == 0 {
		// COALESCE returns 1 when MAX is NULL (no rows), but defend against
		// Scan leaving v at its zero value on driver error — keep the
		// invariant "version >= 1" by clamping.
		v = 1
	}
	return v
}

// distributeDMKeys mirrors distributeChannelKeysToMembers' guarded-transaction
// shape (#2201): one tx, actor-epoch GuardTx first, post-commit notifications.
// insertWrappedDMKeyTx mirrors insertWrappedChannelKeyTx for the unified-DM
// branch: inserted=false means an idempotent duplicate; any error (statement OR
// RowsAffected — the latter leaves delivery unknowable, Codex #2397 review)
// fails the batch.
func insertWrappedDMKeyTx(ctx context.Context, tx *sql.Tx, conversationID, memberUserID, wrappedKey string, keyVersion int) (inserted bool, err error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (conversation_id, user_id, key_version) DO NOTHING
	`, conversationID, memberUserID, wrappedKey, keyVersion)
	if err != nil {
		return false, fmt.Errorf("store dm key: %w", err)
	}
	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		return false, fmt.Errorf("dm key rows affected: %w", raErr)
	}
	return rowsAffected > 0, nil
}

// enqueueDMKeyRequest is the DM analogue of enqueueChannelKeyRequest (#2420
// self-heal): idempotent re-request for a recipient skipped as stale.
func enqueueDMKeyRequest(ctx context.Context, tx *sql.Tx, conversationID, userID string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO dm_pending_key_requests (conversation_id, user_id)
		 VALUES ($1, $2) ON CONFLICT (conversation_id, user_id) DO NOTHING`,
		conversationID, userID)
	return err
}

// distributeOneDMKey processes one DM distribution target inside the caller's
// epoch-guarded transaction: the #2420 recipient-freshness guard (fail-open when
// the distributor supplied no version) and the idempotent insert. A stale
// recipient is skipped (inserted=false) after a self-heal enqueue; any statement
// error fails the batch (25P02).
func distributeOneDMKey(ctx context.Context, tx *sql.Tx, conversationID, memberUserID, wrappedKey string, wrappedKeyVersions map[string]int, keyVersion int) (inserted bool, err error) {
	if wrappedVersion, ok := wrappedKeyVersions[memberUserID]; ok {
		fresh, fErr := recipientKeyFresh(ctx, tx, memberUserID, wrappedVersion)
		if fErr != nil {
			return false, fErr
		}
		if !fresh {
			if eErr := enqueueDMKeyRequest(ctx, tx, conversationID, memberUserID); eErr != nil {
				return false, fmt.Errorf("enqueue dm self-heal: %w", eErr)
			}
			return false, nil
		}
	}
	return insertWrappedDMKeyTx(ctx, tx, conversationID, memberUserID, wrappedKey, keyVersion)
}

func (h *Handler) distributeDMKeys(ctx context.Context, actorID, tokenEpoch, conversationID string, wrappedKeys map[string]string, wrappedKeyVersions map[string]int, explicitVersion *int) (int, error) {
	keyVersion := h.resolveTargetKeyVersionDM(conversationID, explicitVersion)

	// #2201 review: request context, same rationale as distributeChannelKeysToMembers.
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin dm distribution tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback dm distribution tx", "error", rbErr)
		}
	}()
	if guardErr := credepoch.GuardTx(ctx, tx, actorID, tokenEpoch); guardErr != nil {
		return 0, guardErr
	}

	distributed := 0
	var delivered []string
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}
		inserted, insErr := distributeOneDMKey(ctx, tx, conversationID, memberUserID, wrappedKey, wrappedKeyVersions, keyVersion)
		if insErr != nil {
			return 0, insErr
		}
		if !inserted {
			continue
		}
		delivered = append(delivered, memberUserID)
		distributed++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit dm distribution tx: %w", err)
	}
	for _, memberUserID := range delivered {
		// Best-effort POST-commit cleanup — see distributeChannelKeysToMembers.
		_, _ = h.db.Exec(
			`DELETE FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
			conversationID, memberUserID,
		)
		h.notifyKeyDelivered(conversationID, memberUserID)
	}
	return distributed, nil
}

// RotateKey handles manual seal & rotate for server channel E2EE.
// POST /channels/:id/rotate-key
func (h *Handler) RotateKey(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Look up channel's server (all channels are encrypted under E2EE-everywhere #201).
	var serverID string
	err := h.db.QueryRow(
		`SELECT server_id FROM channels WHERE id = $1`, channelID,
	).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to look up channel for rotation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
		return
	}

	// Check permission to manage crypto rotation
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageCryptoRotation)
	if err != nil {
		h.log.Error("Failed to check permissions for rotation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Per-resource rate limit: 10 rotations per 24h per channel.
	// Subscription-tiered limits deferred to issue #603.
	rateLimitKey := fmt.Sprintf("ratelimit:channel_rotate:%s", channelID)
	if blocked, retryAfter := middleware.IsRateLimited(c.Request.Context(), h.redis, rateLimitKey, 10, 24*time.Hour); blocked {
		middleware.RespondRateLimited(c, retryAfter, 10)
		return
	}

	var maxVersion int
	_ = h.db.QueryRow(`SELECT COALESCE(MAX(key_version), 0) FROM channel_keys WHERE channel_id = $1`, channelID).Scan(&maxVersion)

	h.log.Info("Channel key rotation requested", "channel_id", channelID, "user_id", userID, "current_version", maxVersion)

	// Broadcast key_rotation to all server subscribers
	if h.hub != nil {
		if serverUUID, parseErr := uuid.Parse(serverID); parseErr == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "key_rotation",
				Data: map[string]interface{}{
					"channel_id":      channelID,
					"server_id":       serverID,
					"triggered_by":    userID,
					"new_key_version": maxVersion + 1,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         "Key rotation initiated",
		"new_key_version": maxVersion + 1,
	})
}

// ValidateEpochs checks if any of the client's cached key epochs have been revoked.
// Called on reconnect to catch missed key_revocation WebSocket events.
// POST /api/channels/validate-epochs
func (h *Handler) ValidateEpochs(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		Epochs map[string]int `json:"epochs" binding:"required"` // channel_id → current cached epoch
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	type revocationInfo struct {
		ChannelID      string `json:"channel_id"`
		RevokedEpoch   int    `json:"revoked_epoch"`
		SuccessorEpoch int    `json:"successor_epoch"`
		Reason         string `json:"reason"`
	}

	var revocations []revocationInfo

	for channelID, clientEpoch := range req.Epochs {
		if _, parseErr := uuid.Parse(channelID); parseErr != nil {
			continue
		}

		// Verify the user has access to this channel (member of server or DM participant)
		var hasAccess bool
		_ = h.db.QueryRow(
			`SELECT EXISTS(
				SELECT 1 FROM channels c
				INNER JOIN server_members sm ON c.server_id = sm.server_id
				WHERE c.id = $1 AND sm.user_id = $2
			)`,
			channelID, userID,
		).Scan(&hasAccess)
		if !hasAccess {
			continue
		}

		// Check if the client's epoch has been revoked
		var revokedEpoch, successorEpoch int
		var reason string
		err := h.db.QueryRow(
			`SELECT revoked_epoch, successor_epoch, reason
			 FROM key_revocations
			 WHERE channel_id = $1 AND revoked_epoch = $2`,
			channelID, clientEpoch,
		).Scan(&revokedEpoch, &successorEpoch, &reason)
		if err == nil {
			revocations = append(revocations, revocationInfo{
				ChannelID:      channelID,
				RevokedEpoch:   revokedEpoch,
				SuccessorEpoch: successorEpoch,
				Reason:         reason,
			})
		}
	}

	if revocations == nil {
		revocations = []revocationInfo{}
	}

	c.JSON(http.StatusOK, gin.H{"revocations": revocations})
}
