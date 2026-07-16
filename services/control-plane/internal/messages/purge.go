package messages

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
)

const (
	errMsgEnumerateChannels = "Failed to enumerate channels"
	errMsgPurgeFailed       = "Purge failed"
)

// purgeRequest is the shared body for the channel and server purge endpoints (#1352).
// Range is recent-ward ("1h".."90d") or "all"; TargetUserID narrows to one author
// (requires ManageAllMessages — an actor with only ManageOwnMessages is forced to self).
type purgeRequest struct {
	Range        string  `json:"range" binding:"required"`
	TargetUserID *string `json:"target_user_id"`
}

// PurgeChannel handles DELETE /channels/:id/messages — bulk-delete a channel's
// messages, scoped by time range and optional author (#1352).
//
// Authorization (OWASP A01 — resolved ONCE, before any mutation):
//   - PermManageAllMessages → may purge any/all authors in the channel.
//   - PermManageOwnMessages only → forced to target_user = self.
//   - Neither → 403, zero rows deleted, no audit row.
func (h *Handler) PurgeChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	var req purgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	rangeFrom, err := purge.ParseRange(req.Range)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid range"})
		return
	}

	var serverID string
	if err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found"})
		return
	}

	author, ok := h.resolvePurgeAuthor(c, serverID, userID, channelID, req.TargetUserID)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions to purge this channel"})
		return
	}

	plan := purge.Plan{
		ContextType: purge.ContextChannel,
		ContextID:   channelID,
		ServerID:    &serverID,
		ActorID:     userID,
		Target:      req.TargetUserID,
		Reason:      "manual",
		RangeFrom:   rangeFrom,
		Deletes: []purge.DeleteSpec{{
			MessagesTable:    "messages",
			ScopeColumn:      "channel_id",
			ScopeID:          channelID,
			AttachmentsTable: "message_attachments",
			Author:           author,
		}},
	}
	res, err := h.purgeEngine.Run(c.Request.Context(), plan)
	if err != nil {
		h.log.Error("Channel purge failed", "error", err, "channel_id", channelID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return
	}

	h.log.Info("Channel purged", "channel_id", channelID, "actor", userID, "deleted", res.DeletedCount)
	h.emitChannelPurged(channelID, userID, res.DeletedCount, req.Range)
	// hidden_count is structurally 0 for server contexts — returned so the response
	// shape matches spec §4 { deleted_count, hidden_count } across ALL contexts.
	c.JSON(http.StatusOK, gin.H{"deleted_count": res.DeletedCount, "hidden_count": 0})
}

// PurgeServer handles DELETE /servers/:id/messages — bulk-delete across a server's
// channels (#1352).
//
// SECURITY (review finding M1): authorization is re-resolved PER CHANNEL, never once
// at server scope. computeEffectivePermissions skips applyChannelOverrides when
// channelID is "", so a single server-scope check would bypass channel_permission_overrides
// rows that DENY ManageAllMessages (or view) on specific channels — and irreversibly
// delete messages the actor is explicitly denied access to. Channels where the actor
// lacks permission are SKIPPED; if no channel is purgeable the request 403s with no
// audit row (the guard runs before Engine.Run, which writes the audit).
func (h *Handler) PurgeServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	var req purgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	rangeFrom, err := purge.ParseRange(req.Range)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid range"})
		return
	}

	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT id FROM channels WHERE server_id = $1`, serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgEnumerateChannels})
		return
	}
	defer func() { _ = rows.Close() }()

	var channelIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgEnumerateChannels})
			return
		}
		channelIDs = append(channelIDs, id)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgEnumerateChannels})
		return
	}

	var deletes []purge.DeleteSpec
	for _, chID := range channelIDs {
		author, ok := h.resolvePurgeAuthor(c, serverID, userID, chID, req.TargetUserID)
		if !ok {
			continue // denied in this channel — skip it, never delete here
		}
		deletes = append(deletes, purge.DeleteSpec{
			MessagesTable:    "messages",
			ScopeColumn:      "channel_id",
			ScopeID:          chID,
			AttachmentsTable: "message_attachments",
			Author:           author,
		})
	}
	if len(deletes) == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions to purge this server"})
		return
	}

	plan := purge.Plan{
		ContextType: purge.ContextServer,
		ContextID:   serverID,
		ServerID:    &serverID,
		ActorID:     userID,
		Target:      req.TargetUserID,
		Reason:      "manual",
		RangeFrom:   rangeFrom,
		Deletes:     deletes,
	}
	res, err := h.purgeEngine.Run(c.Request.Context(), plan)
	if err != nil {
		h.log.Error("Server purge failed", "error", err, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return
	}

	h.log.Info("Server purged", "server_id", serverID, "actor", userID,
		"channels", len(deletes), "deleted", res.DeletedCount)
	// One event per affected channel (spec §11). The engine returns only an aggregate
	// count, so the per-channel event carries 0; clients treat the event as an
	// invalidation signal and refetch (next-fetch is the correctness backstop).
	for _, ds := range deletes {
		h.emitChannelPurged(ds.ScopeID, userID, 0, req.Range)
	}
	c.JSON(http.StatusOK, gin.H{"deleted_count": res.DeletedCount, "hidden_count": 0})
}

// resolvePurgeAuthor resolves the author filter for one channel per the RBAC matrix:
// View → required first; ManageAll → requested target (or nil = all authors);
// ManageOwn only → self, and ONLY when no other author was requested; neither → not
// authorized. Membership resolution is inside HasPermission (ErrNotMember → false).
// Resolver errors fail CLOSED (denied).
func (h *Handler) resolvePurgeAuthor(c *gin.Context, serverID, userID, channelID string, target *string) (*string, bool) {
	// You cannot purge what you cannot see. Without this, a channel override that
	// denies PermViewTextChannels but leaves ManageAllMessages intact would let an
	// actor irreversibly wipe a private channel they cannot even open. Purge needs no
	// message ID, so unlike DeleteMessage it reaches every message in the channel by
	// scope alone — the view gate is what bounds that reach.
	canView, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermViewTextChannels)
	if err != nil {
		h.log.Error("Purge authz resolve failed", "error", err, "channel_id", channelID)
		return nil, false
	}
	if !canView {
		return nil, false
	}

	canAll, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageAllMessages)
	if err != nil {
		h.log.Error("Purge authz resolve failed", "error", err, "channel_id", channelID)
		return nil, false
	}
	if canAll {
		return target, true
	}
	canOwn, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageOwnMessages)
	if err != nil || !canOwn {
		return nil, false
	}
	// ManageOwn only: may purge their own messages, and ONLY their own.
	//
	// An explicit target that is not self is a permission error — NOT license to purge
	// the actor's messages instead. Silently redirecting an irreversible bulk delete
	// onto a different subject and returning 200 destroys the wrong data and reports
	// success; it would also write an audit row naming the requested target while
	// deleting the actor's own messages (false Art.17 evidence against a third party).
	// A nil target means "my own messages" and is the legitimate ManageOwn request.
	if target != nil && *target != userID {
		return nil, false
	}
	self := userID
	return &self, true
}

// emitChannelPurged broadcasts the bulk-purge event to channel subscribers.
// Payload carries counts and context only — never message content (spec §11).
func (h *Handler) emitChannelPurged(channelID, actorID string, count int, rng string) {
	if h.hub == nil {
		return
	}
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}
	h.hub.BroadcastToChannelAuthorized(channelUUID, websocket.OutgoingMessage{
		Type: "channel_purged",
		Data: map[string]interface{}{
			"channel_id":    channelID,
			"purged_by":     actorID,
			"deleted_count": count,
			"range":         rng,
		},
	})
}
