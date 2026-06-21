package rbac

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgInvalidServerID         = "Invalid server ID"
	errMsgInvalidRoleID           = "Invalid role ID"
	errMsgInvalidChannelID        = "Invalid channel ID"
	errMsgInvalidCategoryID       = "Invalid category ID"
	errMsgInvalidRequestBody      = "Invalid request body"
	errMsgInsufficientPermissions = "Insufficient permissions"
	errMsgFailedCheckPermissions  = "Failed to check permissions"
	errMsgRoleNotFound            = "Role not found"
	errMsgChannelNotFound         = "Channel not found"
	errMsgCategoryNotFound        = "Category not found"
	errMsgFailedCreateRole        = "Failed to create role"
	errMsgFailedUpdateRole        = "Failed to update role"
	errMsgFailedDeleteRole        = "Failed to delete role"
	errMsgFailedReorderRoles      = "Failed to reorder roles"
	errMsgFailedAssignRole        = "Failed to assign role"
	errMsgFailedUnassignRole      = "Failed to unassign role"
	errMsgFailedFetchOverrides    = "Failed to fetch overrides"
	errMsgFailedSaveOverride      = "Failed to save override"
	errMsgFailedDeleteOverride    = "Failed to delete override"
	errMsgFailedFetchPermissions  = "Failed to fetch permissions"
	errMsgFailedUpdateSync        = "Failed to update sync"
	errMsgCannotGrantPerms        = "Cannot grant permissions you do not have"
	errMsgFailedGetServerOwner    = "Failed to get server owner"
	errMsgFailedGetActorPosition  = "Failed to get actor role position"
	errMsgFailedQueryRole         = "Failed to query role"
	errMsgFailedQueryChannel      = "Failed to query channel"
	errMsgFailedQueryCategory     = "Failed to query category"
	errMsgFailedGetActorPerms     = "Failed to get actor permissions"
)

// Handler handles RBAC-related HTTP requests
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	redis    *redis.Client
	hub      *websocket.Hub
	resolver *Resolver
	cache    *PermissionCache
	audit    *AuditWriter
}

// NewHandler creates a new RBAC handler
func NewHandler(
	db *sql.DB,
	log *logger.Logger,
	redis *redis.Client,
	hub *websocket.Hub,
	resolver *Resolver,
	cache *PermissionCache,
	audit *AuditWriter,
) *Handler {
	return &Handler{
		db:       db,
		log:      log,
		redis:    redis,
		hub:      hub,
		resolver: resolver,
		cache:    cache,
		audit:    audit,
	}
}

// Role represents a server role
type Role struct {
	ID                string  `json:"id"`
	ServerID          string  `json:"server_id"`
	Name              string  `json:"name"`
	Color             *string `json:"color,omitempty"`
	Emoji             *string `json:"emoji,omitempty"`
	Position          int     `json:"position"`
	Permissions       int64   `json:"permissions,string"`
	IsDefault         bool    `json:"is_default"`
	IsManaged         bool    `json:"is_managed"`
	Mentionable       bool    `json:"mentionable"`
	DisplaySeparately bool    `json:"display_separately"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// CreateRoleRequest represents a request to create a new role
type CreateRoleRequest struct {
	Name              string  `json:"name" binding:"required,min=1,max=100"`
	Color             *string `json:"color,omitempty"`
	Emoji             *string `json:"emoji,omitempty"`
	Permissions       int64   `json:"permissions,string"`
	Mentionable       bool    `json:"mentionable"`
	DisplaySeparately bool    `json:"display_separately"`
}

// UpdateRoleRequest represents a request to update an existing role
type UpdateRoleRequest struct {
	Name              *string `json:"name,omitempty"`
	Color             *string `json:"color,omitempty"`
	Emoji             *string `json:"emoji,omitempty"`
	Permissions       *int64  `json:"permissions,string,omitempty"`
	Mentionable       *bool   `json:"mentionable,omitempty"`
	DisplaySeparately *bool   `json:"display_separately,omitempty"`
}

// ReorderRolesRequest represents a request to reorder roles (changes position values)
type ReorderRolesRequest struct {
	RoleIDs []string `json:"role_ids" binding:"required"`
}

// ListRoles returns all roles for a server
func (h *Handler) ListRoles(c *gin.Context) {
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	query := `
		SELECT id, server_id, name, color, emoji, position, permissions,
		       is_default, is_managed, mentionable, display_separately, created_at, updated_at
		FROM roles
		WHERE server_id = $1
		ORDER BY position DESC
	`

	rows, err := h.db.Query(query, serverID)
	if err != nil {
		h.log.Error("Failed to query roles", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch roles"})
		return
	}
	defer rows.Close() //nolint:errcheck

	roles := []Role{}
	for rows.Next() {
		var role Role
		if err := rows.Scan(
			&role.ID, &role.ServerID, &role.Name, &role.Color, &role.Emoji,
			&role.Position, &role.Permissions, &role.IsDefault, &role.IsManaged,
			&role.Mentionable, &role.DisplaySeparately, &role.CreatedAt, &role.UpdatedAt,
		); err != nil {
			h.log.Error("Failed to scan role", "error", err)
			continue
		}
		roles = append(roles, role)
	}

	c.JSON(http.StatusOK, gin.H{"roles": roles})
}

// CreateRole creates a new role in a server
func (h *Handler) CreateRole(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var req CreateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Privilege escalation check: new role permissions must be a subset of actor's permissions
	actorPerms, err := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, "")
	if err != nil {
		h.log.Error(errMsgFailedGetActorPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateRole})
		return
	}
	if Permission(req.Permissions)&^actorPerms != 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgCannotGrantPerms})
		return
	}

	// Validate color format if provided
	if req.Color != nil && len(*req.Color) > 0 {
		if (*req.Color)[0] != '#' || len(*req.Color) != 7 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid color format (expected #RRGGBB)"})
			return
		}
	}

	// Create role — place above @all (position 0) by assigning max(position) + 1
	roleID := uuid.New().String()

	var nextPosition int
	err = h.db.QueryRow(`SELECT COALESCE(MAX(position), 0) + 1 FROM roles WHERE server_id = $1`, serverID).Scan(&nextPosition)
	if err != nil {
		h.log.Error("Failed to get next role position", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateRole})
		return
	}

	query := `
		INSERT INTO roles (id, server_id, name, color, emoji, position, permissions, mentionable, display_separately)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at, updated_at
	`

	var role Role
	role.ID = roleID
	role.ServerID = serverID
	role.Name = req.Name
	role.Color = req.Color
	role.Emoji = req.Emoji
	role.Position = nextPosition
	role.Permissions = req.Permissions
	role.Mentionable = req.Mentionable
	role.DisplaySeparately = req.DisplaySeparately

	err = h.db.QueryRow(
		query, roleID, serverID, req.Name, req.Color, req.Emoji,
		nextPosition, req.Permissions, req.Mentionable, req.DisplaySeparately,
	).Scan(&role.CreatedAt, &role.UpdatedAt)

	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "A role with that name already exists in this server"})
			return
		}
		h.log.Error("Failed to create role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateRole})
		return
	}

	// Invalidate permission cache for entire server
	_ = h.cache.InvalidateServer(c.Request.Context(), serverID)

	// Audit log
	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "role_created", "role", &roleID,
			map[string]interface{}{"role_name": req.Name, "permissions": req.Permissions})
	}

	// Broadcast role_created event
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "role_created",
		Data: map[string]interface{}{
			"server_id": serverID,
			"role":      role,
		},
	})

	h.log.Info("Role created", "role_id", roleID, "server_id", serverID, "name", req.Name)
	c.JSON(http.StatusCreated, gin.H{"role": role})
}

// UpdateRole updates an existing role
func (h *Handler) UpdateRole(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	roleID := c.Param("role_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(roleID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRoleID})
		return
	}

	var req UpdateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	rolePosition, err := h.validateRoleModifiable(c, roleID, serverID)
	if err != nil {
		return // Response already written
	}

	isOwner, err := h.checkRoleOwnerAndHierarchy(c, serverID, userID, rolePosition)
	if err != nil {
		return // Response already written
	}

	if h.checkPermissionEscalation(c, serverID, userID, isOwner, req.Permissions) {
		return
	}

	updates, args, argIdx := h.buildRoleUpdateClauses(req, roleID, serverID)
	if h.validateRoleColor(c, req.Color, &updates, &args, &argIdx) {
		return
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	query := "UPDATE roles SET " + strings.Join(updates, ", ") +
		", updated_at = NOW() WHERE id = $1 AND server_id = $2 RETURNING id, server_id, name, color, emoji, position, permissions, is_default, is_managed, mentionable, display_separately, created_at, updated_at"

	var role Role
	err = h.db.QueryRow(query, args...).Scan(
		&role.ID, &role.ServerID, &role.Name, &role.Color, &role.Emoji,
		&role.Position, &role.Permissions, &role.IsDefault, &role.IsManaged,
		&role.Mentionable, &role.DisplaySeparately, &role.CreatedAt, &role.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return
	}
	if err != nil {
		h.log.Error("Failed to update role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return
	}

	_ = h.cache.InvalidateServer(c.Request.Context(), serverID)
	h.auditRoleUpdate(c, serverID, userID, roleID, req)
	h.broadcastRoleUpdated(serverID, roleID, role)

	h.log.Info("Role updated", "role_id", roleID, "server_id", serverID)
	c.JSON(http.StatusOK, gin.H{"role": role})
}

// validateRoleModifiable checks that the role exists, belongs to the server, and is not managed.
// Returns the role position, or writes an error response and returns an error.
func (h *Handler) validateRoleModifiable(c *gin.Context, roleID, serverID string) (int, error) {
	var isManaged bool
	var rolePosition int
	err := h.db.QueryRow(`SELECT is_managed, position FROM roles WHERE id = $1 AND server_id = $2`, roleID, serverID).Scan(&isManaged, &rolePosition)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return 0, err
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryRole, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return 0, err
	}
	if isManaged {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot modify managed roles"})
		return 0, fmt.Errorf("managed role")
	}
	return rolePosition, nil
}

// checkRoleOwnerAndHierarchy checks if the user is the owner (bypasses hierarchy) and enforces
// role hierarchy for non-owners. Returns (isOwner, error). Error means response was written.
func (h *Handler) checkRoleOwnerAndHierarchy(c *gin.Context, serverID, userID string, rolePosition int) (bool, error) {
	var ownerID string
	if err := h.db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return false, err
	}
	if ownerID == userID {
		return true, nil
	}

	var actorMaxPosition int
	if err := h.db.QueryRow(
		`SELECT COALESCE(MAX(r.position), 0) FROM member_roles mr INNER JOIN roles r ON mr.role_id = r.id WHERE mr.server_id = $1 AND mr.user_id = $2`,
		serverID, userID,
	).Scan(&actorMaxPosition); err != nil {
		h.log.Error(errMsgFailedGetActorPosition, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return false, err
	}
	if rolePosition >= actorMaxPosition {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot modify a role at or above your own position"})
		return false, fmt.Errorf("hierarchy check failed")
	}
	return false, nil
}

// checkPermissionEscalation verifies the actor cannot grant permissions they don't have.
// Returns true if the request was blocked (response written).
func (h *Handler) checkPermissionEscalation(c *gin.Context, serverID, userID string, isOwner bool, permissions *int64) bool {
	if isOwner || permissions == nil {
		return false
	}
	actorPerms, permErr := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, "")
	if permErr != nil {
		h.log.Error(errMsgFailedGetActorPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return true
	}
	if Permission(*permissions)&^actorPerms != 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgCannotGrantPerms})
		return true
	}
	return false
}

// buildRoleUpdateClauses builds the SET clauses and args for the role update query
// (excluding color, which requires validation).
func (h *Handler) buildRoleUpdateClauses(req UpdateRoleRequest, roleID, serverID string) ([]string, []interface{}, int) {
	updates := []string{}
	args := []interface{}{roleID, serverID}
	argIdx := 3

	if req.Name != nil {
		updates = append(updates, "name = $"+strconv.Itoa(argIdx))
		args = append(args, *req.Name)
		argIdx++
	}
	if req.Emoji != nil {
		updates = append(updates, "emoji = $"+strconv.Itoa(argIdx))
		args = append(args, *req.Emoji)
		argIdx++
	}
	if req.Permissions != nil {
		updates = append(updates, "permissions = $"+strconv.Itoa(argIdx))
		args = append(args, *req.Permissions)
		argIdx++
	}
	if req.Mentionable != nil {
		updates = append(updates, "mentionable = $"+strconv.Itoa(argIdx))
		args = append(args, *req.Mentionable)
		argIdx++
	}
	if req.DisplaySeparately != nil {
		updates = append(updates, "display_separately = $"+strconv.Itoa(argIdx))
		args = append(args, *req.DisplaySeparately)
		argIdx++
	}
	return updates, args, argIdx
}

// validateRoleColor validates and appends the color clause if provided.
// Returns true if the request was blocked due to invalid color format.
func (h *Handler) validateRoleColor(c *gin.Context, color *string, updates *[]string, args *[]interface{}, argIdx *int) bool {
	if color == nil || len(*color) == 0 {
		return false
	}
	if (*color)[0] != '#' || len(*color) != 7 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid color format (expected #RRGGBB)"})
		return true
	}
	*updates = append(*updates, "color = $"+strconv.Itoa(*argIdx))
	*args = append(*args, *color)
	*argIdx++
	return false
}

// auditRoleUpdate writes an audit log entry for a role update if audit logging is configured.
func (h *Handler) auditRoleUpdate(c *gin.Context, serverID, userID, roleID string, req UpdateRoleRequest) {
	if h.audit == nil {
		return
	}
	metadata := make(map[string]interface{})
	if req.Name != nil {
		metadata["new_name"] = *req.Name
	}
	if req.Permissions != nil {
		metadata["new_permissions"] = *req.Permissions
	}
	_ = h.audit.Log(c.Request.Context(), serverID, &userID, "role_updated", "role", &roleID, metadata)
}

// broadcastRoleUpdated sends a role_updated WebSocket event to server members.
func (h *Handler) broadcastRoleUpdated(serverID, roleID string, role Role) {
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "role_updated",
		Data: map[string]interface{}{
			"server_id": serverID,
			"role_id":   roleID,
			"role":      role,
		},
	})
}

// DeleteRole deletes a role from a server
func (h *Handler) DeleteRole(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	roleID := c.Param("role_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(roleID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRoleID})
		return
	}

	// Check if role is managed or default (cannot be deleted) and get position for hierarchy check
	var isManaged, isDefault bool
	var rolePosition int
	err := h.db.QueryRow(
		`SELECT is_managed, is_default, position FROM roles WHERE id = $1 AND server_id = $2`,
		roleID, serverID,
	).Scan(&isManaged, &isDefault, &rolePosition)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryRole, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteRole})
		return
	}

	if isManaged {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot delete managed roles"})
		return
	}
	if isDefault {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot delete default roles"})
		return
	}

	// Hierarchy check: actor can only delete roles below their highest role position
	var actorMaxPosition int
	if err := h.db.QueryRow(
		`SELECT COALESCE(MAX(r.position), 0) FROM member_roles mr INNER JOIN roles r ON mr.role_id = r.id WHERE mr.server_id = $1 AND mr.user_id = $2`,
		serverID, userID,
	).Scan(&actorMaxPosition); err != nil {
		h.log.Error(errMsgFailedGetActorPosition, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteRole})
		return
	}
	var ownerID string
	if err := h.db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteRole})
		return
	}
	if ownerID != userID && rolePosition >= actorMaxPosition {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot delete a role at or above your own position"})
		return
	}

	// Delete role (CASCADE will remove member_roles entries)
	result, err := h.db.Exec(`DELETE FROM roles WHERE id = $1 AND server_id = $2`, roleID, serverID)
	if err != nil {
		h.log.Error("Failed to delete role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteRole})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return
	}

	// Invalidate cache
	_ = h.cache.InvalidateServer(c.Request.Context(), serverID)

	// Audit log
	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "role_deleted", "role", &roleID, nil)
	}

	// Broadcast role_deleted event
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "role_deleted",
		Data: map[string]interface{}{
			"server_id": serverID,
			"role_id":   roleID,
		},
	})

	h.log.Info("Role deleted", "role_id", roleID, "server_id", serverID)
	c.JSON(http.StatusOK, gin.H{"message": "Role deleted"})
}

// ReorderRoles updates position values for roles (for role hierarchy)
func (h *Handler) ReorderRoles(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageRoles)
	if err != nil || !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	var req ReorderRolesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if h.checkReorderHierarchy(c, serverID, userID, req.RoleIDs) {
		return
	}

	if err := h.applyRolePositions(serverID, req.RoleIDs); err != nil {
		h.log.Error("Failed to apply role positions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReorderRoles})
		return
	}

	_ = h.cache.InvalidateServer(c.Request.Context(), serverID)

	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "roles_reordered", "role", nil,
			map[string]interface{}{"new_order": req.RoleIDs})
	}

	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "roles_reordered",
		Data: map[string]interface{}{
			"server_id": serverID,
			"role_ids":  req.RoleIDs,
		},
	})

	c.JSON(http.StatusOK, gin.H{"message": "Roles reordered"})
}

func (h *Handler) checkReorderHierarchy(c *gin.Context, serverID, userID string, roleIDs []string) bool {
	var actorMaxPosition int
	if err := h.db.QueryRow(
		`SELECT COALESCE(MAX(r.position), 0) FROM member_roles mr INNER JOIN roles r ON mr.role_id = r.id WHERE mr.server_id = $1 AND mr.user_id = $2`,
		serverID, userID,
	).Scan(&actorMaxPosition); err != nil {
		h.log.Error(errMsgFailedGetActorPosition, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReorderRoles})
		return true
	}

	var ownerID string
	if err := h.db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReorderRoles})
		return true
	}

	if ownerID == userID {
		return false
	}

	if h.checkRolePositionViolations(c, serverID, actorMaxPosition, roleIDs) {
		return true
	}

	if len(roleIDs)-1 >= actorMaxPosition {
		c.JSON(http.StatusForbidden, gin.H{"error": "Reorder would create roles at or above your position"})
		return true
	}
	return false
}

func (h *Handler) checkRolePositionViolations(c *gin.Context, serverID string, actorMaxPosition int, roleIDs []string) bool {
	args := make([]interface{}, 0, 2+len(roleIDs))
	args = append(args, serverID, actorMaxPosition)
	placeholders := make([]string, len(roleIDs))
	for i, id := range roleIDs {
		placeholders[i] = "$" + strconv.Itoa(i+3)
		args = append(args, id)
	}
	inClause := strings.Join(placeholders, ", ")

	var violationCount int
	checkQuery := `SELECT COUNT(*) FROM roles WHERE server_id = $1 AND position >= $2 AND id IN (` + inClause + `)`
	if err := h.db.QueryRow(checkQuery, args...).Scan(&violationCount); err != nil {
		h.log.Error("Failed to check role positions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReorderRoles})
		return true
	}
	if violationCount > 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot reorder roles at or above your own position"})
		return true
	}
	return false
}

func (h *Handler) applyRolePositions(serverID string, roleIDs []string) error {
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	for i, roleID := range roleIDs {
		position := len(roleIDs) - i - 1
		if _, err := tx.Exec(
			`UPDATE roles SET position = $1, updated_at = NOW() WHERE id = $2 AND server_id = $3 AND is_managed = FALSE`,
			position, roleID, serverID,
		); err != nil {
			return fmt.Errorf("role %s: %w", roleID, err)
		}
	}

	return tx.Commit()
}

// AssignRole assigns a role to a member
func (h *Handler) AssignRole(c *gin.Context) {
	actorID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	var req struct {
		RoleID string `json:"role_id" binding:"required,uuid"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Verify target is a member
	var targetExists bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, targetUserID,
	).Scan(&targetExists); err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAssignRole})
		return
	}
	if !targetExists {
		c.JSON(http.StatusNotFound, gin.H{"error": "User is not a member"})
		return
	}

	// Verify role exists and get its position for hierarchy check
	var rolePosition int
	err := h.db.QueryRow(
		`SELECT position FROM roles WHERE id = $1 AND server_id = $2`,
		req.RoleID, serverID,
	).Scan(&rolePosition)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryRole, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAssignRole})
		return
	}

	// Hierarchy check: actor can only assign roles with lower position than their highest role
	var actorMaxPosition int
	if err := h.db.QueryRow(
		`SELECT COALESCE(MAX(r.position), 0) FROM member_roles mr INNER JOIN roles r ON mr.role_id = r.id WHERE mr.server_id = $1 AND mr.user_id = $2`,
		serverID, actorID,
	).Scan(&actorMaxPosition); err != nil {
		h.log.Error(errMsgFailedGetActorPosition, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAssignRole})
		return
	}

	// Check if actor is server owner (owners bypass hierarchy)
	var ownerID string
	if err := h.db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAssignRole})
		return
	}

	if ownerID != actorID && rolePosition >= actorMaxPosition {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot assign a role with equal or higher position than your own"})
		return
	}

	// Assign role
	_, err = h.db.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id, assigned_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (server_id, user_id, role_id) DO NOTHING`,
		serverID, targetUserID, req.RoleID, actorID,
	)
	if err != nil {
		h.log.Error("Failed to assign role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAssignRole})
		return
	}

	// Invalidate cache
	_ = h.cache.Invalidate(c.Request.Context(), serverID, targetUserID)

	// Audit log
	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &actorID, "role_assigned", "member", &targetUserID,
			map[string]interface{}{"role_id": req.RoleID})
	}

	// Broadcast role_assigned event
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "role_assigned",
		Data: map[string]interface{}{
			"server_id": serverID,
			"user_id":   targetUserID,
			"role_id":   req.RoleID,
		},
	})

	c.JSON(http.StatusOK, gin.H{"message": "Role assigned"})
}

// UnassignRole removes a role from a member
func (h *Handler) UnassignRole(c *gin.Context) {
	actorID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")
	roleID := c.Param("role_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}
	if _, err := uuid.Parse(roleID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRoleID})
		return
	}

	// Cannot unassign default roles
	var isDefault bool
	var rolePosition int
	err := h.db.QueryRow(`SELECT is_default, position FROM roles WHERE id = $1 AND server_id = $2`, roleID, serverID).Scan(&isDefault, &rolePosition)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryRole, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnassignRole})
		return
	}
	if isDefault {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot unassign default roles"})
		return
	}

	// Hierarchy check: actor can only unassign roles with lower position than their highest role
	var actorMaxPosition int
	if err := h.db.QueryRow(
		`SELECT COALESCE(MAX(r.position), 0) FROM member_roles mr INNER JOIN roles r ON mr.role_id = r.id WHERE mr.server_id = $1 AND mr.user_id = $2`,
		serverID, actorID,
	).Scan(&actorMaxPosition); err != nil {
		h.log.Error(errMsgFailedGetActorPosition, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnassignRole})
		return
	}

	var ownerID string
	if err := h.db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnassignRole})
		return
	}

	if ownerID != actorID && rolePosition >= actorMaxPosition {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot unassign a role with equal or higher position than your own"})
		return
	}

	// Remove role assignment
	result, err := h.db.Exec(
		`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`,
		serverID, targetUserID, roleID,
	)
	if err != nil {
		h.log.Error("Failed to unassign role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnassignRole})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Role assignment not found"})
		return
	}

	// Invalidate cache
	_ = h.cache.Invalidate(c.Request.Context(), serverID, targetUserID)

	// Audit log
	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &actorID, "role_unassigned", "member", &targetUserID,
			map[string]interface{}{"role_id": roleID})
	}

	// Broadcast role_unassigned event
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "role_unassigned",
		Data: map[string]interface{}{
			"server_id": serverID,
			"user_id":   targetUserID,
			"role_id":   roleID,
		},
	})

	c.JSON(http.StatusOK, gin.H{"message": "Role unassigned"})
}

// GetMyServerPermissions returns the effective permissions bitfield for the authenticated user
func (h *Handler) GetMyServerPermissions(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	perms, err := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, "")
	if err != nil {
		h.log.Error("Failed to get permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchPermissions})
		return
	}

	c.JSON(http.StatusOK, gin.H{"permissions": int64(perms)})
}

// GetAuditLog returns paginated audit log entries for a server
func (h *Handler) GetAuditLog(c *gin.Context) {
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Parse pagination params
	limit := 50
	offset := 0
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 && parsedLimit <= 100 {
			limit = parsedLimit
		}
	}
	if offsetStr := c.Query("offset"); offsetStr != "" {
		if parsedOffset, err := strconv.Atoi(offsetStr); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}

	entries, err := h.audit.GetAuditLog(c.Request.Context(), serverID, limit, offset)
	if err != nil {
		h.log.Error("Failed to fetch audit log", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch audit log"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"entries": entries, "limit": limit, "offset": offset})
}

// ─────────────────────────────────────────────────────────────────────────────
// Channel Permission Overrides (SBAC Layer)
// ─────────────────────────────────────────────────────────────────────────────

// ChannelOverride represents a channel-specific permission override
type ChannelOverride struct {
	ID         string `json:"id"`
	ChannelID  string `json:"channel_id"`
	TargetType string `json:"target_type"` // "user" or "role"
	TargetID   string `json:"target_id"`
	Allow      int64  `json:"allow"`
	Deny       int64  `json:"deny"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// UpsertOverrideRequest represents a request to create/update a permission override
type UpsertOverrideRequest struct {
	TargetType string `json:"target_type" binding:"required,oneof=user role"`
	TargetID   string `json:"target_id" binding:"required,uuid"`
	Allow      int64  `json:"allow"`
	Deny       int64  `json:"deny"`
}

// ListChannelOverrides returns all permission overrides for a channel
func (h *Handler) ListChannelOverrides(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Verify channel exists and get server ID for membership check
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryChannel, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchOverrides})
		return
	}

	// Require PermManageChannels to view overrides (prevents leaking access control info)
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageChannels)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	query := `
		SELECT id, channel_id, target_type, target_id, allow, deny, created_at, updated_at
		FROM channel_permission_overrides
		WHERE channel_id = $1
		ORDER BY target_type, created_at
	`

	rows, err := h.db.Query(query, channelID)
	if err != nil {
		h.log.Error("Failed to query overrides", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchOverrides})
		return
	}
	defer rows.Close() //nolint:errcheck

	overrides := []ChannelOverride{}
	for rows.Next() {
		var override ChannelOverride
		if err := rows.Scan(
			&override.ID, &override.ChannelID, &override.TargetType, &override.TargetID,
			&override.Allow, &override.Deny, &override.CreatedAt, &override.UpdatedAt,
		); err != nil {
			continue
		}
		overrides = append(overrides, override)
	}

	if err := rows.Err(); err != nil {
		h.log.Error("Failed to iterate overrides", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchOverrides})
		return
	}

	c.JSON(http.StatusOK, gin.H{"overrides": overrides})
}

// UpsertChannelOverride creates or updates a channel permission override
func (h *Handler) UpsertChannelOverride(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req UpsertOverrideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Verify channel exists and get server ID for permission check
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryChannel, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSaveOverride})
		return
	}

	// Check PermManageChannels
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageChannels)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	// Privilege escalation check: allow bits must be a subset of actor's own permissions
	// (deny bits don't need this check — they remove permissions, not grant them)
	// Administrators bypass this check — they can grant any permission via channel overrides
	actorPerms, permErr := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, "")
	if permErr != nil {
		h.log.Error(errMsgFailedGetActorPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !actorPerms.Has(PermAdministrator) && Permission(req.Allow)&^actorPerms != 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgCannotGrantPerms})
		return
	}

	// Upsert override
	query := `
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (channel_id, target_type, target_id) DO UPDATE
		SET allow = EXCLUDED.allow, deny = EXCLUDED.deny, updated_at = NOW()
		RETURNING id, created_at, updated_at, (xmax = 0) AS is_insert
	`

	overrideID := uuid.New().String()
	var override ChannelOverride
	override.ID = overrideID
	override.ChannelID = channelID
	override.TargetType = req.TargetType
	override.TargetID = req.TargetID
	override.Allow = req.Allow
	override.Deny = req.Deny

	var isInsert bool
	err = h.db.QueryRow(query, overrideID, channelID, req.TargetType, req.TargetID, req.Allow, req.Deny).
		Scan(&override.ID, &override.CreatedAt, &override.UpdatedAt, &isInsert)
	if err != nil {
		h.log.Error("Failed to upsert override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSaveOverride})
		return
	}

	// Invalidate cache for affected channel
	_ = h.cache.InvalidateChannel(c.Request.Context(), serverID, channelID)

	// Audit log — xmax=0 means INSERT (new row), otherwise UPDATE (conflict)
	if h.audit != nil {
		action := "channel_override_updated"
		if isInsert {
			action = "channel_override_created"
		}
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, action, "channel", &channelID,
			map[string]interface{}{
				"target_type": req.TargetType,
				"target_id":   req.TargetID,
				"allow":       req.Allow,
				"deny":        req.Deny,
			})
	}

	c.JSON(http.StatusOK, gin.H{"override": override})
}

// DeleteChannelOverride removes a channel permission override
func (h *Handler) DeleteChannelOverride(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")
	overrideID := c.Param("override_id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}
	if _, err := uuid.Parse(overrideID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid override ID"})
		return
	}

	// Verify channel exists and get server ID for permission check
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryChannel, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

	// Check PermManageChannels
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageChannels)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	// Delete override
	result, err := h.db.Exec(
		`DELETE FROM channel_permission_overrides WHERE id = $1 AND channel_id = $2`,
		overrideID, channelID,
	)
	if err != nil {
		h.log.Error("Failed to delete override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Override not found"})
		return
	}

	// Invalidate cache
	_ = h.cache.InvalidateChannel(c.Request.Context(), serverID, channelID)

	// Audit log
	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "channel_override_deleted", "channel", &channelID,
			map[string]interface{}{"override_id": overrideID})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Override deleted"})
}

// GetMyChannelPermissions returns the effective permissions bitfield for the authenticated user in a channel
func (h *Handler) GetMyChannelPermissions(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Get server ID
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryChannel, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchPermissions})
		return
	}

	perms, err := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, channelID)
	if err != nil {
		h.log.Error("Failed to get channel permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchPermissions})
		return
	}

	c.JSON(http.StatusOK, gin.H{"permissions": int64(perms)})
}

// ─────────────────────────────────────────────────────────────────────────────
// Category Permission Overrides (SBAC for channel groups / categories)
// ─────────────────────────────────────────────────────────────────────────────

// CategoryOverride represents a permission override for a category (channel group)
type CategoryOverride struct {
	ID         string `json:"id"`
	CategoryID string `json:"category_id"`
	TargetType string `json:"target_type"` // "user" or "role"
	TargetID   string `json:"target_id"`
	Allow      int64  `json:"allow"`
	Deny       int64  `json:"deny"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// getCategoryServerID looks up the server_id for a category (channel_groups row)
func (h *Handler) getCategoryServerID(categoryID string) (string, error) {
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channel_groups WHERE id = $1`, categoryID).Scan(&serverID)
	return serverID, err
}

// ListCategoryOverrides returns all permission overrides for a category
func (h *Handler) ListCategoryOverrides(c *gin.Context) {
	userID := c.GetString("user_id")
	categoryID := c.Param("id")

	if _, err := uuid.Parse(categoryID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCategoryID})
		return
	}

	serverID, err := h.getCategoryServerID(categoryID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgCategoryNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryCategory, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchOverrides})
		return
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageChannels)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	query := `
		SELECT id, category_id, target_type, target_id, allow, deny, created_at, updated_at
		FROM category_permission_overrides
		WHERE category_id = $1
		ORDER BY target_type, created_at
	`

	rows, err := h.db.Query(query, categoryID)
	if err != nil {
		h.log.Error("Failed to query category overrides", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchOverrides})
		return
	}
	defer rows.Close() //nolint:errcheck

	overrides := []CategoryOverride{}
	for rows.Next() {
		var override CategoryOverride
		if err := rows.Scan(
			&override.ID, &override.CategoryID, &override.TargetType, &override.TargetID,
			&override.Allow, &override.Deny, &override.CreatedAt, &override.UpdatedAt,
		); err != nil {
			continue
		}
		overrides = append(overrides, override)
	}

	if err := rows.Err(); err != nil {
		h.log.Error("Failed to iterate category overrides", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchOverrides})
		return
	}

	c.JSON(http.StatusOK, gin.H{"overrides": overrides})
}

// UpsertCategoryOverride creates or updates a category permission override.
// When a category override changes, synced child channels are updated automatically.
func (h *Handler) UpsertCategoryOverride(c *gin.Context) {
	userID := c.GetString("user_id")
	categoryID := c.Param("id")

	if _, err := uuid.Parse(categoryID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCategoryID})
		return
	}

	var req UpsertOverrideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	serverID, err := h.getCategoryServerID(categoryID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgCategoryNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryCategory, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSaveOverride})
		return
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageChannels)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	// Privilege escalation check (same as channel overrides)
	actorPerms, permErr := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, "")
	if permErr != nil {
		h.log.Error(errMsgFailedGetActorPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !actorPerms.Has(PermAdministrator) && Permission(req.Allow)&^actorPerms != 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgCannotGrantPerms})
		return
	}

	// Upsert category override
	query := `
		INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (category_id, target_type, target_id) DO UPDATE
		SET allow = EXCLUDED.allow, deny = EXCLUDED.deny, updated_at = NOW()
		RETURNING id, created_at, updated_at, (xmax = 0) AS is_insert
	`

	overrideID := uuid.New().String()
	var override CategoryOverride
	override.ID = overrideID
	override.CategoryID = categoryID
	override.TargetType = req.TargetType
	override.TargetID = req.TargetID
	override.Allow = req.Allow
	override.Deny = req.Deny

	var isInsert bool
	err = h.db.QueryRow(query, overrideID, categoryID, req.TargetType, req.TargetID, req.Allow, req.Deny).
		Scan(&override.ID, &override.CreatedAt, &override.UpdatedAt, &isInsert)
	if err != nil {
		h.log.Error("Failed to upsert category override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSaveOverride})
		return
	}

	// Cascade to synced child channels: upsert matching channel_permission_overrides
	h.syncCategoryOverridesToChannels(c.Request.Context(), serverID, categoryID)

	// Invalidate cache for all channels in the category
	h.invalidateSyncedChannelCaches(c.Request.Context(), serverID, categoryID)

	// Audit log — xmax=0 means INSERT (new row), otherwise UPDATE (conflict)
	if h.audit != nil {
		action := "category_override_updated"
		if isInsert {
			action = "category_override_created"
		}
		catID := categoryID
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, action, "category", &catID,
			map[string]interface{}{
				"target_type": req.TargetType,
				"target_id":   req.TargetID,
				"allow":       req.Allow,
				"deny":        req.Deny,
			})
	}

	c.JSON(http.StatusOK, gin.H{"override": override})
}

// DeleteCategoryOverride removes a category permission override
func (h *Handler) DeleteCategoryOverride(c *gin.Context) {
	userID := c.GetString("user_id")
	categoryID := c.Param("id")
	overrideID := c.Param("override_id")

	if _, err := uuid.Parse(categoryID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCategoryID})
		return
	}
	if _, err := uuid.Parse(overrideID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid override ID"})
		return
	}

	serverID, err := h.getCategoryServerID(categoryID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgCategoryNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryCategory, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageChannels)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	// Get the override details before deleting (needed for cascade cleanup)
	var targetType, targetID string
	err = h.db.QueryRow(
		`SELECT target_type, target_id FROM category_permission_overrides WHERE id = $1 AND category_id = $2`,
		overrideID, categoryID,
	).Scan(&targetType, &targetID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Override not found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

	// Delete the category override
	_, err = h.db.Exec(
		`DELETE FROM category_permission_overrides WHERE id = $1 AND category_id = $2`,
		overrideID, categoryID,
	)
	if err != nil {
		h.log.Error("Failed to delete category override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

	// Cascade delete from synced child channels
	_, _ = h.db.Exec(
		`DELETE FROM channel_permission_overrides
		 WHERE channel_id IN (SELECT id FROM channels WHERE group_id = $1 AND sync_permissions = TRUE)
		   AND target_type = $2 AND target_id = $3`,
		categoryID, targetType, targetID,
	)

	// Invalidate cache
	h.invalidateSyncedChannelCaches(c.Request.Context(), serverID, categoryID)

	// Audit log
	if h.audit != nil {
		catID := categoryID
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "category_override_deleted", "category", &catID,
			map[string]interface{}{"override_id": overrideID})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Override deleted"})
}

// ─────────────────────────────────────────────────────────────────────────────
// Category ↔ Channel Permission Sync
// ─────────────────────────────────────────────────────────────────────────────

// SetChannelPermissionSync enables or disables category permission sync for a channel.
// When sync is enabled, the channel's overrides are replaced with the parent category's overrides
// and kept in sync when the category changes.
func (h *Handler) SetChannelPermissionSync(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req struct {
		SyncPermissions bool `json:"sync_permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	serverID, groupID, err := h.getChannelSyncInfo(channelID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryChannel, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateSync})
		return
	}

	if req.SyncPermissions && (groupID == nil || *groupID == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Channel is not in a category"})
		return
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", PermManageChannels)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return
	}

	if _, err = h.db.Exec(`UPDATE channels SET sync_permissions = $1 WHERE id = $2`, req.SyncPermissions, channelID); err != nil {
		h.log.Error("Failed to update sync flag", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateSync})
		return
	}

	if req.SyncPermissions && groupID != nil {
		if err := h.copyCategoryOverridesToChannel(c.Request.Context(), channelID, *groupID); err != nil {
			h.log.Error("Failed to sync category overrides to channel", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateSync})
			return
		}
		_ = h.cache.InvalidateChannel(c.Request.Context(), serverID, channelID)
	}

	if h.audit != nil {
		chID := channelID
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "channel_sync_updated", "channel", &chID,
			map[string]interface{}{"sync_permissions": req.SyncPermissions})
	}

	c.JSON(http.StatusOK, gin.H{"sync_permissions": req.SyncPermissions})
}

func (h *Handler) getChannelSyncInfo(channelID string) (string, *string, error) {
	var serverID string
	var groupID *string
	err := h.db.QueryRow(`SELECT server_id, group_id FROM channels WHERE id = $1`, channelID).Scan(&serverID, &groupID)
	return serverID, groupID, err
}

func (h *Handler) copyCategoryOverridesToChannel(ctx context.Context, channelID, categoryID string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_permission_overrides WHERE channel_id = $1`, channelID); err != nil {
		return fmt.Errorf("delete existing overrides: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		SELECT gen_random_uuid(), $1, target_type, target_id, allow, deny
		FROM category_permission_overrides
		WHERE category_id = $2
	`, channelID, categoryID); err != nil {
		return fmt.Errorf("copy category overrides: %w", err)
	}

	return tx.Commit()
}

// syncCategoryOverridesToChannels copies all category overrides to synced child channels
func (h *Handler) syncCategoryOverridesToChannels(ctx context.Context, _, categoryID string) {
	// Get all synced channels in this category
	rows, err := h.db.QueryContext(ctx, `SELECT id FROM channels WHERE group_id = $1 AND sync_permissions = TRUE`, categoryID)
	if err != nil {
		h.log.Error("Failed to query synced channels", "error", err)
		return
	}
	defer rows.Close() //nolint:errcheck

	var channelIDs []string
	for rows.Next() {
		var chID string
		if err := rows.Scan(&chID); err == nil {
			channelIDs = append(channelIDs, chID)
		}
	}
	// Explicitly close rows before starting a new transaction to avoid holding the connection
	_ = rows.Close() //nolint:errcheck

	if len(channelIDs) == 0 {
		return
	}

	// Wrap in transaction for atomicity — all channels update or none do
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin sync transaction", "error", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	for _, chID := range channelIDs {
		// Replace channel overrides with category overrides
		if _, err := tx.ExecContext(ctx, `DELETE FROM channel_permission_overrides WHERE channel_id = $1`, chID); err != nil {
			h.log.Error("Failed to delete channel overrides during sync", "error", err, "channel_id", chID)
			return
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
			SELECT gen_random_uuid(), $1, target_type, target_id, allow, deny
			FROM category_permission_overrides
			WHERE category_id = $2
		`, chID, categoryID); err != nil {
			h.log.Error("Failed to copy category overrides during sync", "error", err, "channel_id", chID)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit sync transaction", "error", err)
	}
}

// invalidateSyncedChannelCaches invalidates caches for all channels in a category
func (h *Handler) invalidateSyncedChannelCaches(ctx context.Context, serverID, categoryID string) {
	rows, err := h.db.Query(`SELECT id FROM channels WHERE group_id = $1`, categoryID)
	if err != nil {
		return
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var chID string
		if err := rows.Scan(&chID); err == nil {
			_ = h.cache.InvalidateChannel(ctx, serverID, chID)
		}
	}
}
