package channels

import (
	"database/sql"
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	errInvalidServerID       = "Invalid server ID"
	errInvalidRequestBody    = "Invalid request body"
	errFailedCheckPerms      = "Failed to check permissions"
	errInsufficientPerms     = "insufficient permissions"
	errFailedCreateGroup     = "Failed to create channel group"
	errFailedUpdateGroup     = "Failed to update channel group"
	errFailedDeleteGroup     = "Failed to delete channel group"
	errFailedReorderChannels = "Failed to reorder channels"
)

// CreateChannelGroupRequest represents a request to create a channel group
type CreateChannelGroupRequest struct {
	Name string `json:"name" binding:"required,min=1,max=100"`
}

// UpdateChannelGroupRequest represents a request to update a channel group
type UpdateChannelGroupRequest struct {
	Name     *string `json:"name,omitempty"`
	Position *int    `json:"position,omitempty"`
}

// ReorderChannelsRequest represents a bulk reorder/move of channels
type ReorderChannelsRequest struct {
	Channels []ChannelPosition `json:"channels" binding:"required"`
}

// ChannelPosition specifies a channel's group and position
type ChannelPosition struct {
	ChannelID string  `json:"channel_id" binding:"required,uuid"`
	GroupID   *string `json:"group_id"` // nil = uncategorized
	Position  int     `json:"position"`
}

// ListChannelGroups returns all channel groups in a server
func (h *Handler) ListChannelGroups(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidServerID})
		return
	}

	// Check membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil || !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	rows, err := h.db.Query(
		`SELECT id, server_id, name, position, created_at, updated_at
		 FROM channel_groups
		 WHERE server_id = $1
		 ORDER BY position ASC, created_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query channel groups", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel groups"})
		return
	}
	defer func() { _ = rows.Close() }()

	groups := []models.ChannelGroup{}
	for rows.Next() {
		var g models.ChannelGroup
		if err := rows.Scan(&g.ID, &g.ServerID, &g.Name, &g.Position, &g.CreatedAt, &g.UpdatedAt); err != nil {
			h.log.Error("Failed to scan channel group", "error", err)
			continue
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating channel groups", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel groups"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channel_groups": groups})
}

// CreateChannelGroup creates a new channel group in a server
func (h *Handler) CreateChannelGroup(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidServerID})
		return
	}

	var req CreateChannelGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidRequestBody})
		return
	}

	// Check permission to manage channels
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(errFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateGroup})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return
	}

	// Auto-assign next position
	var maxPos int
	_ = h.db.QueryRow(
		`SELECT COALESCE(MAX(position), -1) FROM channel_groups WHERE server_id = $1`,
		serverID,
	).Scan(&maxPos)

	groupID := uuid.New().String()
	var group models.ChannelGroup
	err = h.db.QueryRow(
		`INSERT INTO channel_groups (id, server_id, name, position, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, NOW(), NOW())
		 RETURNING id, server_id, name, position, created_at, updated_at`,
		groupID, serverID, req.Name, maxPos+1,
	).Scan(&group.ID, &group.ServerID, &group.Name, &group.Position, &group.CreatedAt, &group.UpdatedAt)
	if err != nil {
		h.log.Error(errFailedCreateGroup, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateGroup})
		return
	}

	h.log.Info("Channel group created", "group_id", groupID, "server_id", serverID, "user_id", userID)

	if h.hub != nil {
		if serverUUID, err := uuid.Parse(serverID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_group_created",
				Data: map[string]interface{}{
					"channel_group": map[string]interface{}{
						"id":         group.ID,
						"server_id":  group.ServerID,
						"name":       group.Name,
						"position":   group.Position,
						"created_at": group.CreatedAt,
						"updated_at": group.UpdatedAt,
					},
				},
			})
		}
	}

	c.JSON(http.StatusCreated, gin.H{"channel_group": group})
}

// UpdateChannelGroup updates a channel group's name or position
func (h *Handler) UpdateChannelGroup(c *gin.Context) {
	userID := c.GetString("user_id")
	groupID := c.Param("group_id")

	if _, err := uuid.Parse(groupID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid group ID"})
		return
	}

	var req UpdateChannelGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidRequestBody})
		return
	}

	// Get group's server and check role
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channel_groups WHERE id = $1`, groupID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel group not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch group", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedUpdateGroup})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(errFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedUpdateGroup})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return
	}

	// Build dynamic update
	var group models.ChannelGroup
	err = h.db.QueryRow(
		`UPDATE channel_groups
		 SET name = COALESCE($1, name),
		     position = COALESCE($2, position),
		     updated_at = NOW()
		 WHERE id = $3
		 RETURNING id, server_id, name, position, created_at, updated_at`,
		req.Name, req.Position, groupID,
	).Scan(&group.ID, &group.ServerID, &group.Name, &group.Position, &group.CreatedAt, &group.UpdatedAt)
	if err != nil {
		h.log.Error(errFailedUpdateGroup, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedUpdateGroup})
		return
	}

	h.log.Info("Channel group updated", "group_id", groupID, "user_id", userID)

	if h.hub != nil {
		if serverUUID, err := uuid.Parse(serverID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_group_updated",
				Data: map[string]interface{}{
					"channel_group": map[string]interface{}{
						"id":         group.ID,
						"server_id":  group.ServerID,
						"name":       group.Name,
						"position":   group.Position,
						"updated_at": group.UpdatedAt,
					},
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"channel_group": group})
}

// DeleteChannelGroup deletes a channel group. Channels in this group get group_id = NULL.
func (h *Handler) DeleteChannelGroup(c *gin.Context) {
	userID := c.GetString("user_id")
	groupID := c.Param("group_id")

	if _, err := uuid.Parse(groupID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid group ID"})
		return
	}

	// Get group's server and check role
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channel_groups WHERE id = $1`, groupID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel group not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch group", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(errFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return
	}

	// Delete group (channels get group_id = NULL via ON DELETE SET NULL)
	_, err = h.db.Exec(`DELETE FROM channel_groups WHERE id = $1`, groupID)
	if err != nil {
		h.log.Error(errFailedDeleteGroup, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
		return
	}

	h.log.Info("Channel group deleted", "group_id", groupID, "user_id", userID)

	if h.hub != nil {
		if serverUUID, err := uuid.Parse(serverID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_group_deleted",
				Data: map[string]interface{}{
					"group_id":  groupID,
					"server_id": serverID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Channel group deleted"})
}

// closeRows closes a result set, logging any close error. Callers defer it so
// the "check every error" rule is honored without adding a conditional (and its
// cognitive complexity) to a deferred closure at each call site. The what arg
// labels the originating query in the log line.
func (h *Handler) closeRows(rows *sql.Rows, what string) {
	if err := rows.Close(); err != nil {
		h.log.Error("Failed to close rows", "query", what, "error", err)
	}
}

// validateReorderGroupOwnership verifies every target group_id in the reorder
// request belongs to serverID (CV-CAN-012). On a lookup error it writes a 500;
// on a missing, malformed, or cross-server group it writes a 400. The distinct
// non-null group_ids are resolved in a single round-trip so a bulk drag-and-drop
// that moves many channels (often into the same category) does not fan out into
// N sequential lookups.
func (h *Handler) validateReorderGroupOwnership(c *gin.Context, req ReorderChannelsRequest, serverID string) bool {
	// Collect the distinct target group_ids. nil/empty is uncategorized and
	// always allowed (mirrors groupBelongsToServer); dedupe so moving several
	// channels into the same category costs one lookup, not one per channel.
	wanted := make(map[string]struct{})
	for _, cp := range req.Channels {
		if cp.GroupID == nil || *cp.GroupID == "" {
			continue
		}
		// A malformed (non-UUID) group_id is a client input error, not a server
		// fault: reject it as a bad binding (400) before it reaches the
		// uuid-typed query, which would otherwise fail as a 500.
		if _, err := uuid.Parse(*cp.GroupID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsgForeignGroup})
			return false
		}
		wanted[*cp.GroupID] = struct{}{}
	}
	if len(wanted) == 0 {
		return true
	}

	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}

	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT id::text FROM channel_groups WHERE id = ANY($1::uuid[]) AND server_id = $2`,
		pq.Array(ids), serverID,
	)
	if err != nil {
		h.log.Error("Failed to validate reorder group ownership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return false
	}
	defer h.closeRows(rows, "reorder group ownership")

	// id is the primary key, so each distinct requested id matches at most one
	// row, and the server_id predicate means every returned row is same-server.
	found := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.log.Error("Failed to scan reorder group ownership", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
			return false
		}
		found++
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Failed to read reorder group ownership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return false
	}

	// A shortfall means at least one requested group is missing or cross-server.
	if found != len(wanted) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgForeignGroup})
		return false
	}
	return true
}

// ReorderChannels bulk-updates channel positions and group assignments.
// Used for drag-and-drop reordering between groups.
func (h *Handler) ReorderChannels(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidServerID})
		return
	}

	var req ReorderChannelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidRequestBody})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(errFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return
	}

	// CV-CAN-012: every non-null target group_id must belong to this server — a
	// bulk reorder must not assign a channel to a category from another server
	// (the permission-sync cascade keys on group_id with no server predicate).
	if !h.validateReorderGroupOwnership(c, req, serverID) {
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	for _, cp := range req.Channels {
		_, err := tx.Exec(
			`UPDATE channels SET group_id = $1, position = $2, updated_at = NOW()
			 WHERE id = $3 AND server_id = $4`,
			cp.GroupID, cp.Position, cp.ChannelID, serverID,
		)
		if err != nil {
			h.log.Error("Failed to update channel position", "error", err, "channel_id", cp.ChannelID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit reorder", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return
	}

	h.log.Info("Channels reordered", "server_id", serverID, "user_id", userID, "count", len(req.Channels))

	h.broadcastChannelsReordered(serverID, req.Channels)

	c.JSON(http.StatusOK, gin.H{"message": "Channels reordered"})
}

// broadcastChannelsReordered emits a channels_reordered event to the server's
// subscribers (best-effort; no-op when the hub is unset or serverID is unparseable).
func (h *Handler) broadcastChannelsReordered(serverID string, channels []ChannelPosition) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "channels_reordered",
		Data: map[string]interface{}{
			"server_id": serverID,
			"channels":  channels,
		},
	})
}
