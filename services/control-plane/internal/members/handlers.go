// Package members provides handlers for managing server membership.
package members

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/keyrotation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgInvalidServerID      = "Invalid server ID"
	errMsgInvalidUserID        = "Invalid user ID"
	errMsgInvalidRequestBody   = "Invalid request body"
	errMsgInsufficientPerms    = "insufficient permissions"
	errMsgFailedFetchMembers   = "Failed to fetch members"
	errMsgFailedAddMember      = "Failed to add member"
	errMsgFailedUpdateMember   = "Failed to update member"
	errMsgFailedRemoveMember   = "Failed to remove member"
	errMsgFailedBanMember      = "Failed to ban member"
	errMsgFailedTimeoutMember  = "Failed to timeout member"
	errMsgFailedGetServerOwner = "Failed to get server owner"
	errMsgFailedCheckPerms     = "Failed to check permissions"
	errMsgUserNotMember        = "User is not a member of this server"

	minTimeoutDuration = time.Minute
	maxTimeoutDuration = 7 * 24 * time.Hour
)

// Handler handles member-related requests
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	redis    *redis.Client
	hub      *websocket.Hub
	resolver *rbac.Resolver
	audit    *rbac.AuditWriter
	rotator  *keyrotation.Rotator
}

// NewHandler creates a new member handler
func NewHandler(db *sql.DB, log *logger.Logger, redisClient *redis.Client, hub *websocket.Hub, resolver *rbac.Resolver, audit *rbac.AuditWriter) *Handler {
	return &Handler{
		db:       db,
		log:      log,
		redis:    redisClient,
		hub:      hub,
		resolver: resolver,
		audit:    audit,
		rotator:  keyrotation.NewRotator(db, log, hub),
	}
}

// AddMemberRequest represents a request to add a member to a server
type AddMemberRequest struct {
	UserID string `json:"user_id" binding:"required,uuid"`
}

// UpdateMemberRequest represents a request to update a member's role
type UpdateMemberRequest struct {
	Role string `json:"role" binding:"required,oneof=admin member"`
}

// TimeoutMemberRequest represents a request to temporarily restrict a server member.
type TimeoutMemberRequest struct {
	DurationSeconds int64  `json:"duration_seconds" binding:"required"`
	Reason          string `json:"reason"`
}

// MemberRoleInfo represents a lightweight role reference for display
type MemberRoleInfo struct {
	RoleID            string  `json:"role_id"`
	RoleName          string  `json:"role_name"`
	RoleColor         *string `json:"role_color,omitempty"`
	RoleEmoji         *string `json:"role_emoji,omitempty"`
	Position          int     `json:"position"`
	DisplaySeparately bool    `json:"display_separately"`
}

// MemberWithUser represents a member with user details
type MemberWithUser struct {
	UserID         string           `json:"user_id"`
	Username       string           `json:"username"`
	DisplayName    *string          `json:"display_name,omitempty"`
	Bio            *string          `json:"bio,omitempty"`
	AvatarURL      *string          `json:"avatar_url,omitempty"`
	HeaderImageURL *string          `json:"header_image_url,omitempty"`
	ColorScheme    *string          `json:"color_scheme,omitempty"`
	Role           string           `json:"role"`
	JoinedAt       string           `json:"joined_at"`
	LastSeen       *int64           `json:"last_seen,omitempty"`
	Roles          []MemberRoleInfo `json:"roles"`
	ServerMuted    bool             `json:"server_muted"`
	ServerDeafened bool             `json:"server_deafened"`
	TimedOutUntil  *time.Time       `json:"timed_out_until,omitempty"`
}

// ListMembers returns all members of a server
func (h *Handler) ListMembers(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Check if user is a member of the server
	var isMember bool
	memberQuery := `
		SELECT EXISTS(
			SELECT 1 FROM server_members
			WHERE server_id = $1 AND user_id = $2
		)
	`

	err := h.db.QueryRow(memberQuery, serverID, userID).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMembers})
		return
	}

	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Get all members of the server with user details
	members, err := h.queryServerMembers(serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMembers})
		return
	}

	h.populateLastSeen(members)
	h.populateRBAcRoles(serverID, members)
	h.ensureRolesNotNil(members)
	h.maskOwnerRole(c, serverID, userID, members)

	c.JSON(http.StatusOK, gin.H{"members": members})
}

// queryServerMembers fetches all members of a server with user details.
func (h *Handler) queryServerMembers(serverID string) ([]MemberWithUser, error) {
	query := `
		SELECT sm.user_id, u.username, u.display_name, u.bio, u.avatar_url, u.header_image_url, u.color_scheme,
		       sm.role, sm.joined_at, sm.server_muted, sm.server_deafened, sm.timed_out_until
		FROM server_members sm
		INNER JOIN users u ON sm.user_id = u.id
		WHERE sm.server_id = $1
		ORDER BY sm.joined_at ASC
	`

	rows, err := h.db.Query(query, serverID)
	if err != nil {
		h.log.Error("Failed to query members", "error", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	members := []MemberWithUser{}
	for rows.Next() {
		var member MemberWithUser
		var timedOutUntil sql.NullTime
		err := rows.Scan(
			&member.UserID,
			&member.Username,
			&member.DisplayName,
			&member.Bio,
			&member.AvatarURL,
			&member.HeaderImageURL,
			&member.ColorScheme,
			&member.Role,
			&member.JoinedAt,
			&member.ServerMuted,
			&member.ServerDeafened,
			&timedOutUntil,
		)
		if err != nil {
			h.log.Error("Failed to scan member", "error", err)
			continue
		}
		if timedOutUntil.Valid {
			t := timedOutUntil.Time
			member.TimedOutUntil = &t
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating members", "error", err)
		return nil, err
	}
	return members, nil
}

// populateLastSeen batch-fetches last_seen timestamps from Redis for all members.
func (h *Handler) populateLastSeen(members []MemberWithUser) {
	if len(members) == 0 {
		return
	}
	keys := make([]string, len(members))
	for i, m := range members {
		keys[i] = fmt.Sprintf("last_seen:%s", m.UserID)
	}
	ctx := context.Background()
	vals, err := h.redis.MGet(ctx, keys...).Result()
	if err != nil {
		return
	}
	for i, val := range vals {
		if val == nil {
			continue
		}
		tsStr, ok := val.(string)
		if !ok {
			continue
		}
		ts, parseErr := strconv.ParseInt(tsStr, 10, 64)
		if parseErr == nil {
			members[i].LastSeen = &ts
		}
	}
}

// populateRBAcRoles fetches RBAC roles for all members in a server and attaches them.
func (h *Handler) populateRBAcRoles(serverID string, members []MemberWithUser) {
	if len(members) == 0 {
		return
	}
	roleRows, err := h.db.Query(`
		SELECT mr.user_id, r.id, r.name, r.color, r.emoji, r.position, r.display_separately
		FROM member_roles mr
		INNER JOIN roles r ON mr.role_id = r.id
		WHERE mr.server_id = $1
		ORDER BY r.position DESC
	`, serverID)
	if err != nil {
		return
	}
	defer func() { _ = roleRows.Close() }()

	memberRoleMap := make(map[string][]MemberRoleInfo)
	for roleRows.Next() {
		var uid, roleID, roleName string
		var roleColor, roleEmoji *string
		var position int
		var displaySeparately bool
		if err := roleRows.Scan(&uid, &roleID, &roleName, &roleColor, &roleEmoji, &position, &displaySeparately); err != nil {
			continue
		}
		memberRoleMap[uid] = append(memberRoleMap[uid], MemberRoleInfo{
			RoleID:            roleID,
			RoleName:          roleName,
			RoleColor:         roleColor,
			RoleEmoji:         roleEmoji,
			Position:          position,
			DisplaySeparately: displaySeparately,
		})
	}
	for i := range members {
		if roles, ok := memberRoleMap[members[i].UserID]; ok {
			members[i].Roles = roles
		}
	}
}

// ensureRolesNotNil ensures Roles is never null in JSON output.
func (h *Handler) ensureRolesNotNil(members []MemberWithUser) {
	for i := range members {
		if members[i].Roles == nil {
			members[i].Roles = []MemberRoleInfo{}
		}
	}
}

// maskOwnerRole masks the owner's role for non-owner viewers (#244: Hidden Owner Role).
// Non-owners see the owner's highest RBAC role name instead of "owner".
func (h *Handler) maskOwnerRole(c *gin.Context, serverID, viewerUserID string, members []MemberWithUser) {
	var serverOwnerID string
	if err := h.db.QueryRowContext(c.Request.Context(), `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&serverOwnerID); err != nil {
		h.log.Error("Failed to fetch server owner for role masking", "error", err, "server_id", serverID)
		return
	}
	if viewerUserID == serverOwnerID {
		return // Owner sees their own "owner" role
	}
	for i := range members {
		if members[i].UserID != serverOwnerID || members[i].Role != "owner" {
			continue
		}
		if len(members[i].Roles) > 0 {
			members[i].Role = members[i].Roles[0].RoleName // highest position (sorted DESC)
		} else {
			members[i].Role = "member"
		}
		break
	}
}

// AddMember adds a user to a server
func (h *Handler) AddMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var req AddMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Check permission to invite members
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermInvite)
	if err != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Check if user to add exists
	var userExists bool
	userQuery := `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`
	err = h.db.QueryRow(userQuery, req.UserID).Scan(&userExists)
	if err != nil {
		h.log.Error("Failed to check user existence", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}

	if !userExists {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Check if target user is banned from this server
	var isBanned bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
		serverID, req.UserID,
	).Scan(&isBanned); err != nil {
		h.log.Error("Failed to check ban status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}
	if isBanned {
		c.JSON(http.StatusForbidden, gin.H{"error": "User is banned from this server"})
		return
	}

	insertQuery := `
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', NOW())
		ON CONFLICT (server_id, user_id) DO NOTHING
		RETURNING joined_at
	`

	var member models.ServerMember
	member.ServerID = serverID
	member.UserID = req.UserID
	member.Role = "member"

	err = h.db.QueryRow(insertQuery, serverID, req.UserID).Scan(&member.JoinedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a member"})
		return
	} else if err != nil {
		h.log.Error("Failed to add member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}

	// Assign all default roles (including @all) to the new member
	_, _ = h.db.Exec(`
		INSERT INTO member_roles (server_id, user_id, role_id)
		SELECT $1, $2, id FROM roles
		WHERE server_id = $1 AND is_default = TRUE
		ON CONFLICT DO NOTHING
	`, serverID, req.UserID)

	h.log.Info("Member added", "server_id", serverID, "new_member", req.UserID, "added_by", userID)

	c.JSON(http.StatusCreated, gin.H{"member": member})
}

// UpdateMember updates a member's role
func (h *Handler) UpdateMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	// Validate IDs
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	var req UpdateMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Check permission to assign roles
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageRolesAssign)
	if err != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMember})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Verify target is a member
	var targetExists bool
	_ = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, targetUserID,
	).Scan(&targetExists)
	if !targetExists {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}

	// Cannot change the owner's legacy role
	var ownerID string
	if err := h.db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMember})
		return
	}
	if targetUserID == ownerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot change the owner's role"})
		return
	}

	// Update role
	updateQuery := `
		UPDATE server_members
		SET role = $1
		WHERE server_id = $2 AND user_id = $3
		RETURNING joined_at
	`

	var member models.ServerMember
	member.ServerID = serverID
	member.UserID = targetUserID
	member.Role = req.Role

	err = h.db.QueryRow(updateQuery, req.Role, serverID, targetUserID).Scan(&member.JoinedAt)
	if err != nil {
		h.log.Error("Failed to update member role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMember})
		return
	}

	h.log.Info("Member role updated", "server_id", serverID, "target_user", targetUserID, "new_role", req.Role, "updated_by", userID)

	c.JSON(http.StatusOK, gin.H{"member": member})
}

func (h *Handler) authorizeTimeout(c *gin.Context, serverID, userID, targetUserID string) (int, string, bool) {
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermTimeoutMembers)
	if err != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", err)
		return http.StatusInternalServerError, errMsgFailedTimeoutMember, false
	}
	if !hasPerm {
		return http.StatusForbidden, errMsgInsufficientPerms, false
	}
	if targetUserID == userID {
		return http.StatusBadRequest, "Cannot timeout yourself", false
	}

	targetExists, err := h.checkMembership(serverID, targetUserID)
	if err != nil {
		h.log.Error("Failed to check target membership", "error", err)
		return http.StatusInternalServerError, errMsgFailedTimeoutMember, false
	}
	if !targetExists {
		return http.StatusNotFound, errMsgUserNotMember, false
	}

	ownerID, err := h.getServerOwnerID(serverID)
	if err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		return http.StatusInternalServerError, errMsgFailedTimeoutMember, false
	}
	if targetUserID == ownerID {
		return http.StatusForbidden, "Cannot timeout the server owner", false
	}
	if h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, targetUserID) != nil {
		return http.StatusForbidden, "Cannot timeout a member with equal or higher role position", false
	}

	return 0, "", true
}

func (h *Handler) broadcastTimeout(serverID, targetUserID string, timedOutUntil *time.Time) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	var timeoutValue interface{}
	if timedOutUntil != nil {
		timeoutValue = timedOutUntil.UTC().Format(time.RFC3339)
	}

	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "member_timeout",
		Data: map[string]interface{}{
			"server_id":       serverID,
			"user_id":         targetUserID,
			"timed_out_until": timeoutValue,
		},
	})
}

// TimeoutMember temporarily bars a member from sending messages and joining voice.
func (h *Handler) TimeoutMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	var req TimeoutMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	duration := time.Duration(req.DurationSeconds) * time.Second
	if duration < minTimeoutDuration || duration > maxTimeoutDuration {
		c.JSON(http.StatusBadRequest, gin.H{"error": "duration_seconds must be between 60 and 604800"})
		return
	}

	if status, msg, ok := h.authorizeTimeout(c, serverID, userID, targetUserID); !ok {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	timedOutUntil := time.Now().UTC().Add(duration)
	var storedUntil time.Time
	if err := h.db.QueryRowContext(c.Request.Context(),
		"UPDATE server_members SET timed_out_until = $1 WHERE server_id = $2 AND user_id = $3 RETURNING timed_out_until",
		timedOutUntil, serverID, targetUserID,
	).Scan(&storedUntil); err != nil {
		h.log.Error("Failed to timeout member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedTimeoutMember})
		return
	}

	if h.audit != nil {
		metadata := map[string]interface{}{"duration_seconds": req.DurationSeconds}
		if req.Reason != "" {
			metadata["reason"] = req.Reason
		}
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "member_timed_out", "member", &targetUserID, metadata) //nolint:errcheck
	}

	h.broadcastTimeout(serverID, targetUserID, &storedUntil)

	c.JSON(http.StatusOK, gin.H{
		"message":         "Member timed out",
		"server_id":       serverID,
		"user_id":         targetUserID,
		"timed_out_until": storedUntil,
	})
}

// RemoveTimeout clears a member timeout restriction.
func (h *Handler) RemoveTimeout(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	if status, msg, ok := h.authorizeTimeout(c, serverID, userID, targetUserID); !ok {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	result, err := h.db.ExecContext(c.Request.Context(),
		"UPDATE server_members SET timed_out_until = NULL WHERE server_id = $1 AND user_id = $2",
		serverID, targetUserID,
	)
	if err != nil {
		h.log.Error("Failed to remove member timeout", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedTimeoutMember})
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}

	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "member_timeout_removed", "member", &targetUserID, nil) //nolint:errcheck
	}

	h.broadcastTimeout(serverID, targetUserID, nil)

	c.JSON(http.StatusOK, gin.H{
		"message":         "Member timeout removed",
		"server_id":       serverID,
		"user_id":         targetUserID,
		"timed_out_until": nil,
	})
}

func (h *Handler) checkMembership(serverID, userID string) (bool, error) {
	var exists bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&exists)
	return exists, err
}

func (h *Handler) getServerOwnerID(serverID string) (string, error) {
	var ownerID string
	err := h.db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID)
	return ownerID, err
}

type removalAuth struct {
	isSelfRemoval bool
}

func (h *Handler) authorizeRemoval(c *gin.Context, serverID, userID, targetUserID, ownerID string) (*removalAuth, int, string) {
	isSelfRemoval := userID == targetUserID

	if isSelfRemoval {
		if userID == ownerID {
			return nil, http.StatusForbidden, "Server owner cannot leave. Delete the server or transfer ownership first."
		}
		return &removalAuth{isSelfRemoval: true}, 0, ""
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermKick)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		return nil, http.StatusInternalServerError, errMsgFailedRemoveMember
	}
	if !hasPerm {
		return nil, http.StatusForbidden, errMsgInsufficientPerms
	}
	if targetUserID == ownerID {
		return nil, http.StatusForbidden, "Cannot remove the server owner"
	}
	if h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, targetUserID) != nil {
		return nil, http.StatusForbidden, "Cannot remove a member with equal or higher role position"
	}

	return &removalAuth{isSelfRemoval: false}, 0, ""
}

func (h *Handler) execRemovalTx(serverID, targetUserID string) error {
	tx, err := h.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	queries := []struct{ query string }{
		{`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`},
		{`DELETE FROM channel_keys WHERE user_id = $2 AND channel_id IN (SELECT id FROM channels WHERE server_id = $1)`},
		{`DELETE FROM pending_key_requests WHERE user_id = $2 AND channel_id IN (SELECT id FROM channels WHERE server_id = $1)`},
		{`DELETE FROM channel_read_states WHERE user_id = $2 AND channel_id IN (SELECT id FROM channels WHERE server_id = $1)`},
	}
	for _, q := range queries {
		if _, err := tx.Exec(q.query, serverID, targetUserID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RemoveMember removes a member from a server (kick or leave)
func (h *Handler) RemoveMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	requesterExists, err := h.checkMembership(serverID, userID)
	if err != nil || !requesterExists {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}
	targetExists, err := h.checkMembership(serverID, targetUserID)
	if err != nil || !targetExists {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}

	ownerID, err := h.getServerOwnerID(serverID)
	if err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveMember})
		return
	}

	auth, status, errMsg := h.authorizeRemoval(c, serverID, userID, targetUserID, ownerID)
	if auth == nil {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	if err := h.execRemovalTx(serverID, targetUserID); err != nil {
		h.log.Error("Failed to remove member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveMember})
		return
	}

	action := "removed"
	if auth.isSelfRemoval {
		action = "left"
	}

	h.log.Info("Member "+action, "server_id", serverID, "target_user", targetUserID, "by_user", userID)

	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "member_removed",
		Data: map[string]interface{}{
			"server_id": serverID,
			"user_id":   targetUserID,
		},
	})

	h.triggerKeyRevocationsForServer(serverID, targetUserID, userID)

	c.JSON(http.StatusOK, gin.H{"message": "Member " + action + " successfully"})
}

// BanRequest represents a request to ban a member
type BanRequest struct {
	Reason string `json:"reason"`
}

// BannedMember represents a banned user
type BannedMember struct {
	ID           string  `json:"id"`
	UserID       string  `json:"user_id"`
	Username     string  `json:"username"`
	DisplayName  *string `json:"display_name,omitempty"`
	AvatarURL    *string `json:"avatar_url,omitempty"`
	BannedBy     *string `json:"banned_by,omitempty"`
	BannedByName *string `json:"banned_by_name,omitempty"`
	Reason       *string `json:"reason,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

func (h *Handler) execBanTx(serverID, targetUserID, actorID string, reason *string) error {
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback", "error", rbErr)
		}
	}()

	_, err = tx.Exec(`
		INSERT INTO server_bans (server_id, user_id, banned_by, reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (server_id, user_id) DO UPDATE SET
			banned_by = EXCLUDED.banned_by,
			reason = EXCLUDED.reason,
			created_at = NOW()
	`, serverID, targetUserID, actorID, reason)
	if err != nil {
		return err
	}

	_, _ = tx.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, targetUserID)
	_, _ = tx.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2`, serverID, targetUserID)
	_, _ = tx.Exec(`DELETE FROM channel_keys WHERE user_id = $1 AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)`, targetUserID, serverID)
	_, _ = tx.Exec(`DELETE FROM pending_key_requests WHERE user_id = $1 AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)`, targetUserID, serverID)
	_, _ = tx.Exec(`DELETE FROM channel_read_states WHERE user_id = $1 AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)`, targetUserID, serverID)

	return tx.Commit()
}

// BanMember bans a member from a server (removes + prevents rejoin)
func (h *Handler) BanMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermBan)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBanMember})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	ownerID, err := h.getServerOwnerID(serverID)
	if err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBanMember})
		return
	}
	if targetUserID == ownerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot ban the server owner"})
		return
	}
	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot ban yourself"})
		return
	}

	if h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, targetUserID) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot ban a member with equal or higher role position"})
		return
	}

	var req BanRequest
	_ = c.ShouldBindJSON(&req)

	var reason *string
	if req.Reason != "" {
		reason = &req.Reason
	}

	if err := h.execBanTx(serverID, targetUserID, userID, reason); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBanMember})
		return
	}

	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "member_banned", "member", &targetUserID, //nolint:errcheck
			map[string]interface{}{"reason": req.Reason})
	}

	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "member_removed",
		Data: map[string]interface{}{
			"server_id": serverID,
			"user_id":   targetUserID,
			"reason":    "banned",
		},
	})

	h.triggerKeyRevocationsForServer(serverID, targetUserID, userID)

	c.JSON(http.StatusOK, gin.H{"message": "Member banned"})
}

// UnbanMember removes a ban from a server
func (h *Handler) UnbanMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermBan)
	if err != nil || !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	result, err := h.db.Exec(`DELETE FROM server_bans WHERE server_id = $1 AND user_id = $2`, serverID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unban member"})
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "User is not banned"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Member unbanned"})
}

// ListBans returns all banned members for a server
func (h *Handler) ListBans(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermBan)
	if err != nil || !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	dbRows, err := h.db.Query(`
		SELECT sb.id, sb.user_id, u.username, u.display_name, u.avatar_url,
		       sb.banned_by, bu.username, sb.reason, sb.created_at
		FROM server_bans sb
		INNER JOIN users u ON sb.user_id = u.id
		LEFT JOIN users bu ON sb.banned_by = bu.id
		WHERE sb.server_id = $1
		ORDER BY sb.created_at DESC
	`, serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch bans"})
		return
	}
	defer func() { _ = dbRows.Close() }()

	bans := []BannedMember{}
	for dbRows.Next() {
		var b BannedMember
		if err := dbRows.Scan(&b.ID, &b.UserID, &b.Username, &b.DisplayName, &b.AvatarURL,
			&b.BannedBy, &b.BannedByName, &b.Reason, &b.CreatedAt); err != nil {
			continue
		}
		bans = append(bans, b)
	}

	c.JSON(http.StatusOK, bans)
}

// triggerKeyRevocationsForServer creates key_revocations records and broadcasts
// key_revocation events for all E2EE channels in a server. Called after a member
// is removed so remaining clients rotate to a new epoch the removed user can't decrypt.
//
// It iterates the server's channels and delegates each channel's rotation to the
// shared keyrotation.Rotator core (RevokeChannelKeyEpoch), passing removedUserID so
// the broadcast payload preserves the member-removal shape. The same Rotator backs
// single-channel rotations in the voice package (temporary-SBAC access revocation,
// #487 P2) — the rotation SQL + broadcast lives in ONE place (internal/keyrotation).
func (h *Handler) triggerKeyRevocationsForServer(serverID, removedUserID, actorID string) {
	rows, err := h.db.Query(
		`SELECT c.id, COALESCE(MAX(ck.key_version), 1)
		 FROM channels c
		 LEFT JOIN channel_keys ck ON ck.channel_id = c.id
		 WHERE c.server_id = $1
		 GROUP BY c.id`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query E2EE channels for key revocation", "error", err, "server_id", serverID)
		return
	}
	defer func() { _ = rows.Close() }()

	serverUUID, _ := uuid.Parse(serverID)

	for rows.Next() {
		var channelID string
		var maxEpoch int
		if err := rows.Scan(&channelID, &maxEpoch); err != nil {
			h.log.Error("Failed to scan channel for key revocation", "error", err)
			continue
		}

		// Per-channel rotation with the member-removal payload shape.
		h.rotator.RevokeChannelKeyEpoch(serverID, serverUUID, channelID, maxEpoch, "member_removal", actorID, removedUserID)
	}

	h.log.Info("Key revocations triggered for member removal",
		"server_id", serverID, "removed_user", removedUserID, "actor", actorID)
}

// triggerKeyRevocationForChannel rotates the CSK epoch for ONE channel and broadcasts
// key_revocation to the remaining members. Thin delegation to the shared
// keyrotation.Rotator (the broadcast omits removed_user_id, which is specific to the
// member-removal path). Retained as a package-local method so existing members
// internal tests keep exercising the rotation path through the handler.
func (h *Handler) triggerKeyRevocationForChannel(channelID, reason, actorID string) {
	h.rotator.TriggerForChannel(channelID, reason, actorID)
}
