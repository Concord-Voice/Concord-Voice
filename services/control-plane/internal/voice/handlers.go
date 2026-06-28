// Package voice provides handlers for voice channel state management.
// It exposes REST endpoints for voice join authorization and participant listing,
// and processes NATS events from the media plane to keep the DB and WS hub in sync.
package voice

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

// Handler handles voice-related requests.
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	cfg         *config.Config
	resolver    *rbac.Resolver
	nats        *natsclient.Client
	audit       *rbac.AuditWriter
	entCache    *entitlements.Cache
	serverTiers entitlements.ServerTierResolver
	tempGrant   *tempGrantManager
}

// HandlerDeps groups the dependencies required to construct a Handler.
//
// Audit may be nil (the move audit entry is then skipped — used by lightweight
// test constructions); production wiring passes the shared rbac.AuditWriter so
// hierarchy-crossing moves are recorded (#487 §6.1). EntCache is the shared
// entitlement-tier cache (#1296) used to resolve the joining user's media
// entitlements (#1300); production wiring passes the same instance the auth
// handler receives.
type HandlerDeps struct {
	DB          *sql.DB
	Log         *logger.Logger
	Hub         *websocket.Hub
	Cfg         *config.Config
	Resolver    *rbac.Resolver
	NATS        *natsclient.Client
	Audit       *rbac.AuditWriter
	EntCache    *entitlements.Cache
	ServerTiers entitlements.ServerTierResolver
}

// NewHandler creates a new voice handler.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		db:          deps.DB,
		log:         deps.Log,
		hub:         deps.Hub,
		cfg:         deps.Cfg,
		resolver:    deps.Resolver,
		nats:        deps.NATS,
		audit:       deps.Audit,
		entCache:    deps.EntCache,
		serverTiers: deps.ServerTiers,
		tempGrant:   newTempGrantManager(deps.DB, deps.Log, deps.Hub, deps.Resolver, deps.NATS),
	}
}

func (h *Handler) serverTier(ctx context.Context, serverID string) string {
	if h.serverTiers != nil {
		return h.serverTiers.GetServerTier(ctx, serverID)
	}
	return entitlements.ResolveServerTier(ctx, h.db, serverID)
}

// Participant represents a user currently in a voice channel.
type Participant struct {
	UserID          string `json:"user_id"`
	Username        string `json:"username"`
	DisplayName     string `json:"display_name,omitempty"`
	AvatarURL       string `json:"avatar_url,omitempty"`
	IsMuted         bool   `json:"is_muted"`
	IsDeafened      bool   `json:"is_deafened"`
	IsVideoOn       bool   `json:"is_video_on"`
	IsScreenSharing bool   `json:"is_screen_sharing"`
	JoinedAt        string `json:"joined_at"`
	ServerMuted     bool   `json:"server_muted"`
	ServerDeafened  bool   `json:"server_deafened"`
}

// GetParticipants returns all users currently in a voice channel.
// GET /channels/:id/voice/participants
func (h *Handler) GetParticipants(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Verify the user has access to this channel's server
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	} else if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}

	// Fetch voice participants with user details and server enforcement flags
	rows, err := h.db.Query(`
		SELECT vp.user_id, u.username, COALESCE(u.display_name, ''), COALESCE(u.avatar_url, ''),
		       vp.is_muted, vp.is_deafened, vp.is_video_on, vp.is_screen_sharing, vp.joined_at,
		       sm.server_muted, sm.server_deafened
		FROM voice_participants vp
		INNER JOIN users u ON u.id = vp.user_id
		INNER JOIN channels c ON c.id = vp.channel_id
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = vp.user_id
		WHERE vp.channel_id = $1
		ORDER BY vp.joined_at ASC
	`, channelID)
	if err != nil {
		h.log.Error("Failed to query voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}
	defer func() { _ = rows.Close() }()

	participants := []Participant{}
	for rows.Next() {
		var p Participant
		if err := rows.Scan(
			&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL,
			&p.IsMuted, &p.IsDeafened, &p.IsVideoOn, &p.IsScreenSharing, &p.JoinedAt,
			&p.ServerMuted, &p.ServerDeafened,
		); err != nil {
			h.log.Error("Failed to scan voice participant", "error", err)
			continue
		}
		participants = append(participants, p)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}

	c.JSON(http.StatusOK, gin.H{"participants": participants})
}

// AuthorizeJoin checks that a user can join a voice channel and returns
// media plane connection details.
// POST /channels/:id/voice/join
func (h *Handler) AuthorizeJoin(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Fetch channel details + verify membership in one query
	var channelName, channelType, serverID string
	var audioQualityTier *string
	err := h.db.QueryRow(`
		SELECT c.id, c.name, c.type, c.server_id, c.audio_quality_tier
		FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&channelID, &channelName, &channelType, &serverID, &audioQualityTier)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel for voice join", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}

	if channelType != "voice" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Not a voice channel"})
		return
	}

	// Check PermJoinVoice permission (with channelID for SBAC overrides)
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermJoinVoice)
	if permErr != nil {
		h.log.Error("Failed to check voice permissions", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Get effective permissions for this channel (media plane uses this to enforce
	// PermSpeak, PermScreenShare, PermMuteMembers, PermDeafenMembers, PermMoveMembers)
	effectivePerms, permResolveErr := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, channelID)
	if permResolveErr != nil {
		h.log.Error("Failed to resolve effective voice permissions", "error", permResolveErr, "user_id", userID, "channel_id", channelID, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}

	// Query server-enforced mute/deafen flags for this member
	var serverMuted, serverDeafened bool
	if err := h.db.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, userID).Scan(&serverMuted, &serverDeafened); err != nil {
		h.log.Error("Failed to query server enforcement flags", "error", err, "user_id", userID, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}

	// Resolve channel-aware media entitlements: the per-channel audio standard
	// (admin-set) uplifts every member, bounded by the server's Mach tier. The
	// joining user's tier is resolved server-side from the AUTHENTICATED user_id;
	// the server-tier resolver is the #1521 seam (Groundspeed today, Mach when
	// #1556 ships); MediaForChannel fails closed (Personal/free) on any unknown
	// value. See [internal]rules/media-plane.md "Per-channel audio standard".
	channelTier := ""
	if audioQualityTier != nil {
		channelTier = *audioQualityTier
	}
	mediaEnt := entitlements.MediaForChannel(
		h.entCache.GetTier(c.Request.Context(), userID),
		h.serverTier(c.Request.Context(), serverID),
		channelTier,
	)

	h.log.Info("Voice join authorized", "user_id", userID, "channel_id", channelID, "server_id", serverID, "media_tier", mediaEnt.Tier)

	c.JSON(http.StatusOK, gin.H{
		"allowed":            true,
		"media_server_url":   h.cfg.MediaPlaneURL,
		"ice_servers":        h.cfg.ICEServers(userID),
		"permissions":        strconv.FormatInt(int64(effectivePerms), 10),
		"server_muted":       serverMuted,
		"server_deafened":    serverDeafened,
		"media_entitlements": mediaEnt,
		"channel": gin.H{
			"id":                 channelID,
			"name":               channelName,
			"server_id":          serverID,
			"audio_quality_tier": audioQualityTier,
		},
	})
}

// AuthorizeVoiceAction checks whether a user has permission to perform a voice
// moderation action (mute, deafen, move) on a target user. The media plane
// should call this endpoint before executing moderation commands.
// POST /channels/:id/voice/authorize-action
func (h *Handler) AuthorizeVoiceAction(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req struct {
		Action       string `json:"action" binding:"required,oneof=mute deafen move"`
		TargetUserID string `json:"target_user_id" binding:"required,uuid"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Get server ID from channel
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	}
	if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}

	// Map action to required permission
	var perm rbac.Permission
	switch req.Action {
	case "mute":
		perm = rbac.PermMuteMembers
	case "deafen":
		perm = rbac.PermDeafenMembers
	case "move":
		perm = rbac.PermMoveMembers
	}

	// Check permission
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, perm)
	if permErr != nil {
		h.log.Error("Failed to check voice moderation permission", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Verify target user is a member of this server
	var targetIsMember bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, req.TargetUserID).Scan(&targetIsMember); err != nil {
		h.log.Error("Failed to check target membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}
	if !targetIsMember {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotMember})
		return
	}

	// Hierarchy check: cannot moderate members with equal or higher role
	if h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, req.TargetUserID) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"allowed":    true,
		"action":     req.Action,
		"target_id":  req.TargetUserID,
		"channel_id": channelID,
	})
}

// --- Server-enforced voice moderation ---

const (
	errMsgInvalidChannelID       = "Invalid channel ID"
	errMsgInvalidServerID        = "Invalid server ID"
	errMsgInvalidUserID          = "Invalid user ID"
	errMsgNotMember              = "Not a member of this server"
	errMsgFetchParticipants      = "Failed to fetch participants"
	errMsgFailedAuthorize        = "Failed to authorize"
	errMsgFailedRevokeTempAccess = "Failed to revoke temporary access"
	errMsgFailedAuthAction       = "Failed to authorize action"
	errMsgInsufficientPerms      = "Insufficient permissions"
	errMsgFailedCheckPerms       = "Failed to check permissions"
	errMsgFailedCheckMember      = "Failed to check membership"
	errMsgTargetNotMember        = "Target user is not a member of this server"
	errMsgHierarchyViolation     = "Cannot moderate a member with equal or higher role position"
	errMsgTargetNotInVoice       = "Target user is not in a voice channel"
	errMsgCannotTargetSelf       = "Cannot target yourself"
)

// voiceModContext holds the validated state from authorizeVoiceMod.
type voiceModContext struct {
	serverID string
	targetID string
}

// authorizeVoiceMod validates params, checks membership, permission, and optionally hierarchy.
// For user-level actions (requireHierarchy=false), it also enforces the self-target guard.
// Returns nil and sends the HTTP error response if any check fails.
func (h *Handler) authorizeVoiceMod(c *gin.Context, perm rbac.Permission, requireHierarchy bool) *voiceModContext {
	actorID := c.GetString("user_id")
	serverID := c.Param("id")
	targetID := c.Param("userId")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return nil
	}
	if _, err := uuid.Parse(targetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return nil
	}

	// Always verify actor is a member of the server
	if !h.checkMembership(c, serverID, actorID) {
		return nil
	}

	if !requireHierarchy {
		// User-level: self-target guard
		if actorID == targetID {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCannotTargetSelf})
			return nil
		}
	}

	// Check permission
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, actorID, "", perm)
	if err != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", err, "permission", perm)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
		return nil
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return nil
	}

	if requireHierarchy && h.resolver.CheckHierarchy(c.Request.Context(), serverID, actorID, targetID) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
		return nil
	}

	return &voiceModContext{serverID: serverID, targetID: targetID}
}

// checkMembership verifies that userID is a member of serverID.
// Returns false and sends the HTTP error response if the check fails.
func (h *Handler) checkMembership(c *gin.Context, serverID, userID string) bool {
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID).Scan(&exists); err != nil {
		h.log.Error(errMsgFailedCheckMember, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckMember})
		return false
	}
	if !exists {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return false
	}
	return true
}

// findVoiceChannel queries the voice channel a target is connected to in a server.
// Returns the channel ID (empty if not in voice) and any real DB error.
func (h *Handler) findVoiceChannel(serverID, targetID string) (string, error) {
	var channelID string
	err := h.db.QueryRow(`
		SELECT vp.channel_id FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE c.server_id = $1 AND vp.user_id = $2
	`, serverID, targetID).Scan(&channelID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return channelID, nil
}

// publishEnforcement publishes a NATS enforcement message if the target is in voice.
func (h *Handler) publishEnforcement(subject, channelID, targetID, action string) {
	if channelID == "" || h.nats == nil {
		return
	}
	if pubErr := h.nats.Publish(subject, map[string]interface{}{
		"channelId": channelID, "userId": targetID, "action": action,
	}); pubErr != nil {
		h.log.Error("Failed to publish NATS enforcement", "error", pubErr, "subject", subject, "action", action)
	}
}

// broadcastVoiceStateUpdate sends a voice_state_update WS event to all server members.
func (h *Handler) broadcastVoiceStateUpdate(serverID, targetID, channelID, action string) {
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"action":     action,
			"user_id":    targetID,
			"server_id":  serverID,
			"channel_id": channelID,
		},
	})
}

// enforcementParams groups the per-action strings for applyServerEnforcement.
type enforcementParams struct {
	query       string // SQL UPDATE statement
	natsSubject string // NATS subject to publish on
	natsAction  string // action field in NATS payload
	wsAction    string // action field in WS broadcast
	successMsg  string // HTTP 200 message
	failMsg     string // HTTP 500 / log message
}

// applyServerEnforcement executes the SQL update, finds voice channel, publishes NATS, broadcasts WS.
func (h *Handler) applyServerEnforcement(c *gin.Context, ctx *voiceModContext, p enforcementParams) {
	result, err := h.db.Exec(p.query, ctx.serverID, ctx.targetID)
	if err != nil {
		h.log.Error(p.failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		h.log.Error("Failed to check rows affected", "error", raErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotMember})
		return
	}
	channelID, findErr := h.findVoiceChannel(ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("Failed to find voice channel for enforcement", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
	}
	h.publishEnforcement(p.natsSubject, channelID, ctx.targetID, p.natsAction)
	h.broadcastVoiceStateUpdate(ctx.serverID, ctx.targetID, channelID, p.wsAction)
	c.JSON(http.StatusOK, gin.H{"message": p.successMsg})
}

// userLevelAction sends a real-time user-level enforcement command via NATS.
func (h *Handler) userLevelAction(c *gin.Context, perm rbac.Permission, natsSubject, natsAction, successMsg, failMsg string) {
	ctx := h.authorizeVoiceMod(c, perm, false)
	if ctx == nil {
		return
	}

	channelID, findErr := h.findVoiceChannel(ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("Failed to find voice channel", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
	}
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotInVoice})
		return
	}

	if h.nats != nil {
		if pubErr := h.nats.Publish(natsSubject, map[string]interface{}{
			"channelId": channelID, "userId": ctx.targetID, "action": natsAction,
		}); pubErr != nil {
			h.log.Error(failMsg, "error", pubErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": failMsg})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": successMsg})
}

// ServerMute applies a persistent server-level mute to a member.
// POST /servers/:id/voice/:userId/mute
func (h *Handler) ServerMute(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMuteMembers, true)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query:       `UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`,
		natsSubject: "voice.enforce.mute", natsAction: "mute", wsAction: "server_muted",
		successMsg: "Member server-muted", failMsg: "Failed to mute member",
	})
}

// ServerUnmute removes a persistent server-level mute from a member.
// DELETE /servers/:id/voice/:userId/mute
func (h *Handler) ServerUnmute(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMuteMembers, true)
	if ctx == nil {
		return
	}

	// Check if target is server_deafened — cannot unmute without undeafening first
	var serverDeafened bool
	if err := h.db.QueryRow(`SELECT server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`,
		ctx.serverID, ctx.targetID).Scan(&serverDeafened); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotMember})
			return
		}
		h.log.Error("Failed to check deafen state", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unmute member"})
		return
	}
	if serverDeafened {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot unmute a server-deafened member; undeafen first"})
		return
	}

	// Remove server mute
	if _, err := h.db.Exec(`UPDATE server_members SET server_muted = false WHERE server_id = $1 AND user_id = $2`,
		ctx.serverID, ctx.targetID); err != nil {
		h.log.Error("Failed to server-unmute member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unmute member"})
		return
	}

	channelID, findErr := h.findVoiceChannel(ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("Failed to find voice channel for unmute enforcement", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
	}
	h.publishEnforcement("voice.enforce.mute", channelID, ctx.targetID, "unmute")
	h.broadcastVoiceStateUpdate(ctx.serverID, ctx.targetID, channelID, "server_unmuted")

	c.JSON(http.StatusOK, gin.H{"message": "Member server-unmuted"})
}

// ServerDeafen applies a persistent server-level deafen (implies mute) to a member.
// POST /servers/:id/voice/:userId/deafen
func (h *Handler) ServerDeafen(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermDeafenMembers, true)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query:       `UPDATE server_members SET server_muted = true, server_deafened = true WHERE server_id = $1 AND user_id = $2`,
		natsSubject: "voice.enforce.deafen", natsAction: "deafen", wsAction: "server_deafened",
		successMsg: "Member server-deafened", failMsg: "Failed to deafen member",
	})
}

// ServerUndeafen removes a persistent server-level deafen (and mute) from a member.
// DELETE /servers/:id/voice/:userId/deafen
func (h *Handler) ServerUndeafen(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermDeafenMembers, true)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query:       `UPDATE server_members SET server_deafened = false, server_muted = false WHERE server_id = $1 AND user_id = $2`,
		natsSubject: "voice.enforce.deafen", natsAction: "undeafen", wsAction: "server_undeafened",
		successMsg: "Member server-undeafened", failMsg: "Failed to undeafen member",
	})
}

// UserMute sends a real-time user-level mute command to the media plane.
// No persistent DB state — requires the target to be in voice.
// POST /servers/:id/voice/:userId/user-mute
func (h *Handler) UserMute(c *gin.Context) {
	h.userLevelAction(c, rbac.PermMuteMembers, "voice.user_mute", "mute", "User mute command sent", "Failed to mute user")
}

// UserDeafen sends a real-time user-level deafen command to the media plane.
// No persistent DB state — requires the target to be in voice.
// POST /servers/:id/voice/:userId/user-deafen
func (h *Handler) UserDeafen(c *gin.Context) {
	h.userLevelAction(c, rbac.PermDeafenMembers, "voice.user_deafen", "deafen", "User deafen command sent", "Failed to deafen user")
}

// --- Force-disconnect (#487 P3) ---

// ServerDisconnect force-disconnects a member from whatever voice channel they
// are currently in within the server. Unlike /move (the single ADR-0023
// hierarchy exception), disconnect RESPECTS hierarchy: a moderator cannot
// disconnect a member with an equal-or-higher role position. It requires the
// Move Members permission and the target must currently be in a voice channel
// in this server (else 409). The action publishes voice.enforce.disconnect so
// the media plane closes the peer's transports; the resulting voice.left NATS
// event drives any temp-grant cleanup automatically (revokeTempGrantIfHeld), so
// this handler does NOT duplicate that cleanup.
//
// POST /servers/:id/voice/:userId/disconnect
func (h *Handler) ServerDisconnect(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMoveMembers, true)
	if ctx == nil {
		return
	}

	channelID, findErr := h.findVoiceChannel(ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("disconnect: find current voice channel", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to disconnect member"})
		return
	}
	if channelID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgTargetNotInVoice})
		return
	}

	// Publish the force-disconnect command. The media plane closes the peer's
	// transports and emits voice.left, which (via the NATSSubscriber) updates DB
	// state and triggers revokeTempGrantIfHeld — no duplicate cleanup here.
	h.publishEnforcement(natsSubjectEnforceDisconnect, channelID, ctx.targetID, "disconnect")

	c.JSON(http.StatusOK, gin.H{"disconnected": true})
}

// --- Moderator temp-grant revoke (#487 Scope C) ---

const errMsgInvalidChannelIDBody = "Invalid channel_id"

// tempAccessRevokeRequest is the RevokeTempAccess request body.
type tempAccessRevokeRequest struct {
	ChannelID string `json:"channel_id" binding:"required,uuid"`
}

// RevokeTempAccess lets a moderator revoke a move-granted temporary SBAC grant
// while the target is still in the VC (#487 Scope C edge case). It converges on
// the single revokeTemporaryChannelAccess path (delete temp override, purge the
// user's channel_keys + pending requests, rotate the channel CSK,
// force-disconnect the live peer, broadcast channel_access_revoked). Authorize
// with Move Members + hierarchy (this is a moderation action, NOT the move
// exception). If no temporary grant exists for (user, channel) the revoke is a
// no-op and returns 200 {revoked:false}.
//
// DELETE /servers/:id/voice/:userId/temp-access  body {channel_id}
func (h *Handler) RevokeTempAccess(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMoveMembers, true)
	if ctx == nil {
		return
	}

	var req tempAccessRevokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelIDBody})
		return
	}

	reqCtx := c.Request.Context()

	// Scope the body channel_id to the path server: authorizeVoiceMod authorized the
	// actor for :id only, so a temp grant in a DIFFERENT server must not be revocable
	// here (cross-server IDOR guard — Gitar finding). Reuses the ServerMove scope helper;
	// temp grants are only ever issued on voice channels, so isVoiceChannelInServer is exact.
	inServer, scopeErr := h.isVoiceChannelInServer(reqCtx, req.ChannelID, ctx.serverID)
	if scopeErr != nil {
		h.log.Error("temp-access revoke: channel-scope check", "error", scopeErr, "channel_id", req.ChannelID, "server_id", ctx.serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeTempAccess})
		return
	}
	if !inServer {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotVoiceInSrv})
		return
	}

	// Probe first so the response can report whether a temp grant actually existed.
	held, probeErr := h.tempGrant.hasTemporaryGrant(reqCtx, req.ChannelID, ctx.targetID)
	if probeErr != nil {
		h.log.Error("temp-access revoke: hasTemporaryGrant probe", "error", probeErr, "channel_id", req.ChannelID, "target_id", ctx.targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeTempAccess})
		return
	}
	if !held {
		// No temp grant (permanent grant or none) → no-op, never touch a permanent grant.
		c.JSON(http.StatusOK, gin.H{"revoked": false})
		return
	}

	actorID := c.GetString("user_id")
	if err := h.tempGrant.revokeTemporaryChannelAccess(reqCtx, ctx.serverID, req.ChannelID, ctx.targetID, actorID); err != nil {
		h.log.Error("temp-access revoke", "error", err, "channel_id", req.ChannelID, "target_id", ctx.targetID, "server_id", ctx.serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeTempAccess})
		return
	}

	c.JSON(http.StatusOK, gin.H{"revoked": true})
}

// --- Move (#487 Scope B) ---

const (
	errMsgInvalidTargetChannel = "Invalid target_channel_id"
	errMsgTargetNotVoiceInSrv  = "target is not a voice channel in this server"
	errMsgAlreadyInTarget      = "user is already in the target channel"
	errMsgMovePrep             = "Failed to prepare move"
	auditActionVoiceMoved      = "voice_member_moved"
)

// moveRequest is the ServerMove request body.
type moveRequest struct {
	TargetChannelID string `json:"target_channel_id" binding:"required,uuid"`
}

// ServerMove relocates a user to another voice channel in the same server (#487
// Scope B). It is the SINGLE sanctioned requireHierarchy=false voice action:
//
//	DELIBERATE HIERARCHY EXCEPTION (ADR-0023): MOVE_MEMBERS bypasses CheckHierarchy
//	so a designated organizer can move anyone — including higher roles and the owner
//	— between same-server voice channels. Worst case is annoyance, not privilege
//	escalation (no data access, no kick/ban, server-scoped, target must already be
//	in voice). Hierarchy-crossing moves are audit-logged. The rbac-reviewer must
//	treat any OTHER requireHierarchy=false voice action as a finding. See #487.
//
// POST /servers/:id/voice/:userId/move  body {target_channel_id}
func (h *Handler) ServerMove(c *gin.Context) {
	actorID := c.GetString("user_id")
	serverID := c.Param("id")
	targetID := c.Param("userId")

	req, ok := h.parseMoveRequest(c, serverID, targetID)
	if !ok {
		return
	}

	selfMove := actorID == targetID

	// Self-move: any user may relocate themselves; no permission, no hierarchy,
	// no temp grant (they can already see channels they're allowed to join).
	if !selfMove && !h.authorizeMove(c, serverID, actorID) {
		return
	}

	fromChannelID, ok := h.resolveMoveSource(c, serverID, targetID, req.TargetChannelID)
	if !ok {
		return
	}

	if !selfMove && !h.prepareModeratedMove(c, serverID, actorID, targetID, fromChannelID, req.TargetChannelID) {
		return
	}

	h.signalMove(serverID, targetID, fromChannelID, req.TargetChannelID)
	c.JSON(http.StatusOK, gin.H{"moved": true})
}

// parseMoveRequest validates the path params + JSON body of a move request. It
// writes the 400 response and returns ok=false on any malformed input.
func (h *Handler) parseMoveRequest(c *gin.Context, serverID, targetID string) (moveRequest, bool) {
	var req moveRequest
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return req, false
	}
	if _, err := uuid.Parse(targetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return req, false
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidTargetChannel})
		return req, false
	}
	return req, true
}

// authorizeMove checks server membership + PermMoveMembers for a moderated
// (non-self) move. It writes the appropriate error response and returns false
// when the actor is not authorized.
//
//	DELIBERATE HIERARCHY EXCEPTION (ADR-0023): requireHierarchy=false — the perm
//	check below is NOT followed by a CheckHierarchy gate, so a designated organizer
//	can move anyone (including higher roles and the owner) between same-server voice
//	channels. This is the SINGLE sanctioned requireHierarchy=false voice action;
//	the rbac-reviewer must treat any OTHER requireHierarchy=false voice action as a
//	finding. Hierarchy-crossing moves are audit-logged (see prepareModeratedMove).
//	See #487 and the ServerMove doc comment above.
func (h *Handler) authorizeMove(c *gin.Context, serverID, actorID string) bool {
	if !h.checkMembership(c, serverID, actorID) {
		return false
	}
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, actorID, "", rbac.PermMoveMembers)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr, "permission", rbac.PermMoveMembers)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return false
	}
	return true
}

// resolveMoveSource validates the target channel (voice type, in this server) and
// resolves the user's current voice channel. It writes the error response and
// returns ok=false on any validation failure (400/409/500). On success it returns
// the user's current ("from") channel id.
func (h *Handler) resolveMoveSource(c *gin.Context, serverID, targetID, targetChannelID string) (string, bool) {
	reqCtx := c.Request.Context()

	// Validate the target channel: voice type AND in this server.
	isVoice, vErr := h.isVoiceChannelInServer(reqCtx, targetChannelID, serverID)
	if vErr != nil {
		h.log.Error("move: target channel lookup", "error", vErr, "target_channel_id", targetChannelID, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return "", false
	}
	if !isVoice {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotVoiceInSrv})
		return "", false
	}

	// The target user must currently be in a voice channel in this server.
	fromChannelID, findErr := h.findVoiceChannel(serverID, targetID)
	if findErr != nil {
		h.log.Error("move: find current voice channel", "error", findErr, "server_id", serverID, "target_id", targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return "", false
	}
	if fromChannelID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgTargetNotInVoice})
		return "", false
	}
	if fromChannelID == targetChannelID {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgAlreadyInTarget})
		return "", false
	}
	return fromChannelID, true
}

// prepareModeratedMove performs the moderated-move side effects before signaling:
// audit a hierarchy-crossing move and grant temporary destination access if the
// target cannot already join. It writes the error response and returns false on a
// failed join-permission check or grant. Self-moves never reach here.
func (h *Handler) prepareModeratedMove(c *gin.Context, serverID, actorID, targetID, fromChannelID, targetChannelID string) bool {
	reqCtx := c.Request.Context()

	// GRANT BEFORE SIGNAL (ordering load-bearing — the client's subsequent
	// AuthorizeJoin checks PermJoinVoice). Only grant if the target cannot
	// already join the destination (avoids polluting overrides for users who
	// already have role-based access). grantTemporaryChannelAccess never
	// downgrades a permanent grant.
	canJoin, joinErr := h.resolver.HasPermission(reqCtx, serverID, targetID, targetChannelID, rbac.PermJoinVoice)
	if joinErr != nil {
		h.log.Error("move: target join-permission check", "error", joinErr, "target_id", targetID, "target_channel_id", targetChannelID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return false
	}
	if !canJoin {
		if grantErr := h.tempGrant.grantTemporaryChannelAccess(reqCtx, serverID, targetChannelID, targetID); grantErr != nil {
			h.log.Error("move: temp grant", "error", grantErr, "target_id", targetID, "target_channel_id", targetChannelID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
			return false
		}
	}

	// Audit a move that crosses hierarchy (target outranks-or-equals actor). Done
	// AFTER the grant succeeds (finding #6) so an authorized-but-failed-to-prepare
	// move does NOT emit a voice_member_moved audit row for a move that never
	// happened.
	h.auditMoveIfCrossesHierarchy(reqCtx, serverID, actorID, targetID, fromChannelID, targetChannelID)

	return true
}

// signalMove directs the moved client to leave+rejoin (D2 client-cooperative).
// The voice.left/voice.joined NATS events then refresh every sidebar.
func (h *Handler) signalMove(serverID, targetID, fromChannelID, targetChannelID string) {
	targetUUID, parseErr := uuid.Parse(targetID)
	if parseErr != nil {
		h.log.Error("move: invalid target UUID for directed broadcast", "error", parseErr, "target_id", targetID)
		return
	}
	h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
		Type: "voice_move",
		Data: map[string]interface{}{
			"user_id":         targetID,
			"from_channel_id": fromChannelID,
			"to_channel_id":   targetChannelID,
			"server_id":       serverID,
		},
	})
}

// isVoiceChannelInServer reports whether channelID is a voice channel belonging to
// serverID. channelID is already UUID-validated by the request binding
// (target_channel_id binding:"required,uuid"), so no parse guard is needed here.
func (h *Handler) isVoiceChannelInServer(ctx context.Context, channelID, serverID string) (bool, error) {
	var exists bool
	err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1 AND server_id = $2 AND type = 'voice')`,
		channelID, serverID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// auditMoveIfCrossesHierarchy writes an audit_log entry when a move crosses role
// hierarchy — i.e., the target outranks or equals the actor (CheckHierarchy returns
// non-nil). Best-effort: a failed audit write is logged but does NOT block the move
// (the move is already authorized by PermMoveMembers). Self-moves never reach here.
func (h *Handler) auditMoveIfCrossesHierarchy(ctx context.Context, serverID, actorID, targetID, fromChannelID, toChannelID string) {
	if h.resolver.CheckHierarchy(ctx, serverID, actorID, targetID) == nil {
		return // actor outranks target → ordinary move, no audit needed
	}
	if h.audit == nil {
		return
	}
	actor := actorID
	target := targetID
	if err := h.audit.Log(ctx, serverID, &actor, auditActionVoiceMoved, "member", &target, map[string]interface{}{
		"from_channel_id":   fromChannelID,
		"to_channel_id":     toChannelID,
		"hierarchy_crossed": true,
	}); err != nil {
		h.log.Error("move: audit log", "error", err, "server_id", serverID, "target_id", targetID)
	}
}
