// Package channels provides handlers for managing server channels.
package channels

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/e2eekeys"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgInvalidServerID         = "Invalid server ID"
	errMsgInvalidChannelID        = "Invalid channel ID"
	errMsgInvalidRequestBody      = "Invalid request body"
	errMsgInsufficientPerms       = "insufficient permissions"
	errMsgNotMemberOfServer       = "Not a member of this server"
	errMsgChannelNotFound         = "Channel not found"
	errMsgFailedFetchChannels     = "Failed to fetch channels"
	errMsgFailedCreateChannel     = "Failed to create channel"
	errMsgFailedUpdateChannel     = "Failed to update channel"
	errMsgFailedDeleteChannel     = "Failed to delete channel"
	errMsgFailedCheckMembership   = "Failed to check membership"
	errMsgFailedFetchKeys         = "Failed to fetch keys"
	errMsgFailedFetchUnreadCounts = "Failed to fetch unread counts"
	errMsgFailedMarkServerRead    = "Failed to mark server read"
	errMsgNoEncryptionKey         = "No encryption key available yet"
	errMsgFailedDistributeKeys    = "Failed to distribute keys"
	errMsgInvalidContextID        = "Invalid context ID"
	errMsgFailedProcessRewrap     = "Failed to process rewrap request"
	errMsgFailedEnrollRewrap      = "Failed to enroll rewrap request"
	errMsgContextNotFound         = "Context not found"
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
		h.log.Error("Failed to resolve visible channels", "error", err)
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
func (h *Handler) CreateChannel(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Check permission to manage channels
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), req.ServerID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Under E2EE-everywhere (#201) wrapped keys are always required.
	if len(req.WrappedKeys) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Encrypted channels require wrapped keys for all members"})
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

	nextPos := h.computeNextPosition(tx, req.ServerID, req.GroupID)
	channelID := uuid.New().String()

	channel, err := h.insertChannel(tx, channelID, req, nextPos)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}

	// Store wrapped channel keys (always required under E2EE-everywhere)
	if h.storeWrappedKeys(tx, channelID, req.WrappedKeys) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store encryption keys"})
		return
	}

	// Auto-create linked text channel for voice channels
	linkedTextChannel, ltcErr := h.maybeCreateLinkedTextChannel(tx, req, channelID, nextPos)
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
func (h *Handler) storeWrappedKeys(tx *sql.Tx, channelID string, wrappedKeys map[string]string) error {
	keyInsert := `
		INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, 1)
	`
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue // skip invalid UUIDs
		}
		if _, err := tx.Exec(keyInsert, channelID, memberUserID, wrappedKey); err != nil {
			h.log.Error("Failed to store channel key", "error", err, "user_id", memberUserID)
			return err
		}
	}
	return nil
}

// maybeCreateLinkedTextChannel creates a linked text channel for voice channels, or returns nil for other types.
func (h *Handler) maybeCreateLinkedTextChannel(tx *sql.Tx, req CreateChannelRequest, voiceChannelID string, nextPos int) (*models.Channel, error) {
	if req.Type != "voice" {
		return nil, nil
	}
	return h.createLinkedTextChannel(tx, req, voiceChannelID, nextPos+1)
}

// createLinkedTextChannel creates a linked text channel for a voice channel.
func (h *Handler) createLinkedTextChannel(tx *sql.Tx, req CreateChannelRequest, voiceChannelID string, position int) (*models.Channel, error) {
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
	h.storeWrappedKeysNonFatal(tx, linkedTextID, req.WrappedKeys)

	return &ltc, nil
}

// storeWrappedKeysNonFatal stores wrapped keys but does not fail on error (keys can be distributed later).
func (h *Handler) storeWrappedKeysNonFatal(tx *sql.Tx, channelID string, wrappedKeys map[string]string) {
	keyInsert := `
		INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, 1)
	`
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}
		if _, err := tx.Exec(keyInsert, channelID, memberUserID, wrappedKey); err != nil {
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
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channel": channel})
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

// GetUnreadCounts returns unread message counts for all channels in a server.
// For each channel, counts messages created after the user's last_read_at.
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

	// For each channel in the server, count messages newer than last_read_at.
	// If no read state exists, fall back to the user's join date so pre-existing
	// messages are not counted as unread for first-time members.
	// Uses JOINs instead of correlated subqueries for better query planning.
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
		GROUP BY ch.id
	`

	rows, err := h.db.Query(query, serverID, userID)
	if err != nil {
		h.log.Error("Failed to query unread counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
		return
	}
	defer func() { _ = rows.Close() }()

	type unreadEntry struct {
		ChannelID   string `json:"channel_id"`
		UnreadCount int    `json:"unread_count"`
	}
	unreads := []unreadEntry{}
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
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query server unread status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch server unread status"})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch server unread status"})
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

	// Verify user is a member of the channel's server
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		)`,
		channelID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check channel membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark channel read"})
		return
	}
	if !isMember {
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark channel read"})
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

	// Upsert read state for every channel in the server
	_, err = h.db.Exec(
		`INSERT INTO channel_read_states (user_id, channel_id, last_read_at)
		 SELECT $1, id, NOW() FROM channels WHERE server_id = $2
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET last_read_at = NOW()`,
		userID, serverID,
	)
	if err != nil {
		h.log.Error("Failed to mark server read", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkServerRead})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "All channels marked as read"})
}

// GetChannelKeys returns the caller's wrapped channel key for an E2EE channel.
func (h *Handler) GetChannelKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Check membership (all channels are encrypted under E2EE-everywhere #201).
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		)`,
		channelID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel keys"})
		return
	}
	if !isMember {
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

	if err := h.verifyChannelEncrypted(channelID, userID); err != nil {
		h.respondKeyDistError(c, err)
		return
	}

	if !h.callerHasChannelKey(channelID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You must have the channel key to distribute keys"})
		return
	}

	targetKeyVersion := h.resolveTargetKeyVersion(channelID, req.KeyVersion)

	distributed, duplicates := h.distributeChannelKeysToMembers(channelID, req.WrappedKeys, targetKeyVersion)

	h.log.Info("Channel keys distributed",
		"channel_id", channelID, "by_user", userID,
		"distributed", distributed, "duplicates", duplicates)

	c.JSON(http.StatusOK, gin.H{
		"distributed": distributed,
		"duplicates":  duplicates,
	})
}

type keyDistError struct {
	status  int
	message string
}

func (e *keyDistError) Error() string { return e.message }

func (h *Handler) verifyChannelEncrypted(channelID, userID string) error {
	// All channels are encrypted under E2EE-everywhere (#201); only membership check remains.
	var exists bool
	err := h.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		)`,
		channelID, userID,
	).Scan(&exists)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		return &keyDistError{http.StatusInternalServerError, errMsgFailedDistributeKeys}
	}
	if !exists {
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

func (h *Handler) distributeChannelKeysToMembers(channelID string, wrappedKeys map[string]string, keyVersion int) (distributed, duplicates int) {
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}

		result, err := h.db.Exec(
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (channel_id, user_id, key_version) DO NOTHING`,
			channelID, memberUserID, wrappedKey, keyVersion,
		)
		if err != nil {
			h.log.Error("Failed to store key for member", "error", err, "user_id", memberUserID)
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			duplicates++
			continue
		}

		_, _ = h.db.Exec(
			`DELETE FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
			channelID, memberUserID,
		)

		h.notifyKeyDelivered(channelID, memberUserID)
		distributed++
	}
	return distributed, duplicates
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
	defer func() { _ = rows.Close() }()

	type pendingRequest struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		UserID    string `json:"user_id"`
		CreatedAt string `json:"created_at"`
	}
	requests := []pendingRequest{}
	for rows.Next() {
		var req pendingRequest
		if err := rows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.CreatedAt); err != nil {
			h.log.Error("Failed to scan pending request", "error", err)
			continue
		}
		requests = append(requests, req)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating pending requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch pending key requests"})
		return
	}

	// Also fetch DM pending key requests
	dmQuery := `
		SELECT dpkr.id, dpkr.conversation_id, dpkr.user_id, dpkr.created_at
		FROM dm_pending_key_requests dpkr
		INNER JOIN dm_channel_keys dck ON dpkr.conversation_id = dck.conversation_id AND dck.user_id = $1
		WHERE dpkr.user_id != $1
		ORDER BY dpkr.created_at ASC
	`
	dmRows, dmErr := h.db.Query(dmQuery, userID)
	if dmErr == nil {
		defer func() { _ = dmRows.Close() }()
		for dmRows.Next() {
			var req pendingRequest
			if err := dmRows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.CreatedAt); err != nil {
				continue
			}
			requests = append(requests, req)
		}
	}

	c.JSON(http.StatusOK, gin.H{"pending_requests": requests})
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

	// Under E2EE-everywhere (#201) all channels are encrypted; only existence + membership matters.
	var channelExists bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
			WHERE c.id = $1
		)
	`, contextID, userID).Scan(&channelExists)

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
	if channelExists {
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

	// Resolve channel vs DM
	var isChannel bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
			WHERE c.id = $1
		)
	`, contextID, userID).Scan(&isChannel)
	if err != nil {
		h.log.Error("re_wrap_request: channel check failed",
			"kind", "re_wrap_check_db_error",
			"context_id", contextID,
			"user_id", userID,
			"error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedProcessRewrap})
		return
	}

	if isChannel {
		h.enrollChannelRewrap(c, contextID, userID)
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
			Error: "Context not found or access denied",
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

	var isChannel bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
			WHERE c.id = $1
		)
	`, contextID, userID).Scan(&isChannel)
	if err != nil {
		h.log.Error("Failed to check channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDistributeKeys})
		return
	}

	if isChannel {
		c.Params = append(c.Params, gin.Param{Key: "id", Value: contextID})
		h.DistributeChannelKeys(c)
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
		c.JSON(http.StatusNotFound, gin.H{"error": "Context not found or access denied"})
		return
	}

	var req DistributeChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	distributed := h.distributeDMKeys(contextID, req.WrappedKeys, req.KeyVersion)
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

func (h *Handler) distributeDMKeys(conversationID string, wrappedKeys map[string]string, explicitVersion *int) int {
	keyVersion := h.resolveTargetKeyVersionDM(conversationID, explicitVersion)

	distributed := 0
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}

		result, err := h.db.Exec(`
			INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (conversation_id, user_id, key_version) DO NOTHING
		`, conversationID, memberUserID, wrappedKey, keyVersion)
		if err != nil {
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			continue
		}

		_, _ = h.db.Exec(
			`DELETE FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
			conversationID, memberUserID,
		)
		h.notifyKeyDelivered(conversationID, memberUserID)
		distributed++
	}
	return distributed
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
