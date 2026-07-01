// Package servers provides handlers for managing Concord servers (Discord-like communities).
package servers

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/media"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	dataImagePrefix       = "data:image/"
	dataURLHeaderSlack    = 64
	errMsgInvalidServerID = "Invalid server ID"
	errMsgServerNotFound  = "Server not found"
	errMsgFailedCreate    = "Failed to create server"
	errMsgFailedFetch     = "Failed to fetch server"
	errMsgFailedUpdate    = "Failed to update server"
	errMsgFailedDelete    = "Failed to delete server"
)

// Handler handles server-related requests
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	resolver    *rbac.Resolver
	serverTiers entitlements.ServerTierResolver // server-axis tier resolution (#1521)
	store       media.ObjectDeleter             // nil when object storage is not configured
}

// NewHandler creates a new server handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, serverTiers entitlements.ServerTierResolver) *Handler {
	return &Handler{
		db:          db,
		log:         log,
		hub:         hub,
		resolver:    resolver,
		serverTiers: serverTiers,
	}
}

// SetMediaStore configures optional object storage for media cleanup on icon/banner removal.
func (h *Handler) SetMediaStore(store media.ObjectDeleter) {
	h.store = store
}

// CreateServerRequest represents a request to create a server
type CreateServerRequest struct {
	Name      string  `json:"name" binding:"required,min=3,max=100"`
	IconURL   *string `json:"icon_url"`
	BannerURL *string `json:"banner_url"`
}

// UpdateServerRequest represents a request to update a server.
// IconURL uses json.RawMessage so we can distinguish between
// "field absent" (nil → don't touch icon) and "field set to null" (remove icon).
type UpdateServerRequest struct {
	Name                 string          `json:"name" binding:"required,min=3,max=100"`
	IconURL              json.RawMessage `json:"icon_url"`
	BannerURL            json.RawMessage `json:"banner_url"`
	AllowEmbeddedContent *bool           `json:"allow_embedded_content,omitempty"` // Server-level embed policy
}

// ListServers returns all servers the user is a member of
func (h *Handler) ListServers(c *gin.Context) {
	userID := c.GetString("user_id")

	// Get connected user IDs from the hub for online count computation
	connectedUsers := h.hub.GetConnectedUsers()
	connectedIDs := make([]string, 0, len(connectedUsers))
	for uid := range connectedUsers {
		connectedIDs = append(connectedIDs, uid.String())
	}

	query := `
		SELECT s.id, s.name, s.icon_url, s.banner_url, s.owner_id, s.allow_embedded_content, s.created_at, s.updated_at, sm.role,
			(SELECT COUNT(*) FROM server_members WHERE server_id = s.id) AS member_count,
			(SELECT COUNT(*) FROM server_members WHERE server_id = s.id AND user_id = ANY($2::uuid[])) AS online_count
		FROM servers s
		INNER JOIN server_members sm ON s.id = sm.server_id
		WHERE sm.user_id = $1
		ORDER BY s.created_at DESC
	`

	rows, err := h.db.Query(query, userID, pq.Array(connectedIDs))
	if err != nil {
		h.log.Error("Failed to query servers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetch})
		return
	}
	defer func() { _ = rows.Close() }()

	servers := []models.ServerWithRole{}
	for rows.Next() {
		var server models.ServerWithRole
		err := rows.Scan(
			&server.ID,
			&server.Name,
			&server.IconURL,
			&server.BannerURL,
			&server.OwnerID,
			&server.AllowEmbeddedContent,
			&server.CreatedAt,
			&server.UpdatedAt,
			&server.Role,
			&server.MemberCount,
			&server.OnlineCount,
		)
		if err != nil {
			h.log.Error("Failed to scan server", "error", err)
			continue
		}
		server.ServerTier = h.serverTiers.GetServerTier(c.Request.Context(), server.ID)
		servers = append(servers, server)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating servers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetch})
		return
	}

	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// validateCreateDataURL validates an image data URL for CreateServer.
// Returns an error string if invalid, or empty string if valid.
func validateCreateDataURL(url *string, maxLen int, label, sizeHint string) string {
	if url == nil || *url == "" {
		return ""
	}
	if !strings.HasPrefix(*url, dataImagePrefix) {
		return label + " must be a valid image data URL (use UpdateServer for uploaded media URLs)"
	}
	if len(*url) > maxLen {
		return label + " image too large (max " + sizeHint + ")"
	}
	return ""
}

func maxDataURLLen(maxBytes int64) int {
	return base64.StdEncoding.EncodedLen(int(maxBytes)) + dataURLHeaderSlack
}

// insertServerRow inserts the server row and scans back the timestamps.
func insertServerRow(tx *sql.Tx, server *models.Server) error {
	query := `
		INSERT INTO servers (id, name, icon_url, banner_url, owner_id, allow_embedded_content, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, FALSE, NOW(), NOW())
		RETURNING created_at, updated_at
	`
	return tx.QueryRow(query, server.ID, server.Name, server.IconURL, server.BannerURL, server.OwnerID).Scan(
		&server.CreatedAt,
		&server.UpdatedAt,
	)
}

// insertOwnerMembership adds the owner as a server member, creates the @all role, and assigns it.
func insertOwnerMembership(tx *sql.Tx, serverID, userID string) error {
	memberQuery := `
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'owner', NOW())
	`
	if _, err := tx.Exec(memberQuery, serverID, userID); err != nil {
		return fmt.Errorf("add owner as member: %w", err)
	}

	allRoleID := uuid.New().String()
	allRoleQuery := `
		INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		VALUES ($1, $2, '@all', 0, $3, TRUE, TRUE)
	`
	if _, err := tx.Exec(allRoleQuery, allRoleID, serverID, int64(rbac.BasePermissions)); err != nil {
		return fmt.Errorf("create @all role: %w", err)
	}

	assignRoleQuery := `
		INSERT INTO member_roles (server_id, user_id, role_id, assigned_by)
		VALUES ($1, $2, $3, $4)
	`
	if _, err := tx.Exec(assignRoleQuery, serverID, userID, allRoleID, userID); err != nil {
		return fmt.Errorf("assign @all role to owner: %w", err)
	}

	return nil
}

// CreateServer creates a new server
func (h *Handler) CreateServer(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	groundspeed := entitlements.ForServer(entitlements.TierGroundspeed)
	if errMsg := validateCreateDataURL(req.IconURL, maxDataURLLen(groundspeed.MaxServerIconBytes), "Icon", "5MB"); errMsg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
		return
	}
	if errMsg := validateCreateDataURL(req.BannerURL, maxDataURLLen(groundspeed.MaxServerBannerBytes), "Banner", "5MB"); errMsg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreate})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	serverID := uuid.New().String()
	server := models.Server{
		ID:                   serverID,
		Name:                 req.Name,
		IconURL:              req.IconURL,
		BannerURL:            req.BannerURL,
		OwnerID:              userID,
		AllowEmbeddedContent: false,
	}

	if err := insertServerRow(tx, &server); err != nil {
		h.log.Error("Failed to create server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreate})
		return
	}

	if err := insertOwnerMembership(tx, serverID, userID); err != nil {
		h.log.Error("Failed to set up owner membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreate})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreate})
		return
	}

	h.log.Info("Server created", "server_id", serverID, "user_id", userID)
	server.ServerTier = h.serverTiers.GetServerTier(c.Request.Context(), server.ID)

	c.JSON(http.StatusCreated, gin.H{
		"server": server,
		"role":   "owner",
	})
}

// GetServer returns a specific server
func (h *Handler) GetServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Check if user is a member
	var role string
	memberQuery := `
		SELECT role FROM server_members
		WHERE server_id = $1 AND user_id = $2
	`

	err := h.db.QueryRow(memberQuery, serverID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetch})
		return
	}

	// Get server details
	var server models.Server
	serverQuery := `
		SELECT id, name, icon_url, banner_url, owner_id, allow_embedded_content, created_at, updated_at
		FROM servers
		WHERE id = $1
	`

	err = h.db.QueryRow(serverQuery, serverID).Scan(
		&server.ID,
		&server.Name,
		&server.IconURL,
		&server.BannerURL,
		&server.OwnerID,
		&server.AllowEmbeddedContent,
		&server.CreatedAt,
		&server.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetch})
		return
	}

	server.ServerTier = h.serverTiers.GetServerTier(c.Request.Context(), serverID)

	c.JSON(http.StatusOK, gin.H{
		"server": server,
		"role":   role,
	})
}

// GetServerEntitlements returns the server-axis entitlement set for a server.
// GET /api/v1/servers/:id/entitlements — members only.
//
// Today every server resolves to Groundspeed (free) via the inert Mach hook
// (#1521); the response shape is stable so the client can gate server-scoped
// features on it now and the Mach values flip on with no client change when
// server subscriptions ship (v1.0 / #211).
func (h *Handler) GetServerEntitlements(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Members only — mirror GetServer's membership gate.
	var role string
	err := h.db.QueryRow(
		`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, userID,
	).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetch})
		return
	}

	ent := entitlements.ForServer(h.serverTiers.GetServerTier(c.Request.Context(), serverID))
	c.JSON(http.StatusOK, gin.H{"entitlement": ent})
}

// UpdateServer updates a server's details
func (h *Handler) UpdateServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var req UpdateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Check permission to manage server
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageServer)
	if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdate})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	// Legacy role for backwards-compat response (dropped in migration 000036)
	var role string
	if err := h.db.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, userID).Scan(&role); err != nil {
		h.log.Warn("Failed to fetch legacy role for UpdateServer response", "error", err, "server_id", serverID, "user_id", userID)
	}

	// Parse and validate icon_url / banner_url. Inline data URLs are broadcast verbatim
	// to server subscribers, so they stay pinned to the Groundspeed floor; the
	// per-tier allowance applies on the uploaded-media path, which broadcasts keys.
	inlineEnt := entitlements.ForServer(entitlements.TierGroundspeed)
	iconURLProvided, iconURL, iconErr := parseMediaURL(req.IconURL)
	if iconErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid icon_url format"})
		return
	}
	if err := validateMediaURL(iconURL, fmt.Sprintf("/api/v1/media/server-icons/%s", serverID), maxDataURLLen(inlineEnt.MaxServerIconBytes), "Icon"); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	bannerURLProvided, bannerURL, bannerErr := parseMediaURL(req.BannerURL)
	if bannerErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid banner_url format"})
		return
	}
	if err := validateMediaURL(bannerURL, fmt.Sprintf("/api/v1/media/server-banners/%s", serverID), maxDataURLLen(inlineEnt.MaxServerBannerBytes), "Banner"); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Build dynamic UPDATE query
	setClauses, args, argIdx, mediaKeysToDelete := buildUpdateClauses(serverID, req, iconURLProvided, iconURL, bannerURLProvided, bannerURL)

	args = append(args, serverID)
	updateQuery := fmt.Sprintf("UPDATE servers SET %s WHERE id = $%d RETURNING name, icon_url, banner_url, owner_id, allow_embedded_content, created_at, updated_at", //nolint:gosec // setClauses are hardcoded column names, argIdx is an integer — no injection risk // nosemgrep:concord-go-sql-sprintf
		strings.Join(setClauses, ", "), argIdx)

	var server models.Server
	server.ID = serverID
	// nosemgrep: go.net.sql.go-vanillasql-format-string-sqli-taint-med-conf.go-vanillasql-format-string-sqli-taint-med-conf,go.net.sql.go-vanillasql-format-string-sqli-taint.go-vanillasql-format-string-sqli-taint
	err = h.db.QueryRow(updateQuery, args...).Scan( //nolint:gosec // updateQuery composed by buildUpdateClauses: hardcoded column names + integer argIdx via fmt.Sprintf; user values flow only through args... as parameterized $N placeholders. See matching nosemgrep on the fmt.Sprintf above.
		&server.Name, &server.IconURL, &server.BannerURL, &server.OwnerID, &server.AllowEmbeddedContent, &server.CreatedAt, &server.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to update server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdate})
		return
	}

	// Clean up orphaned media objects from storage after successful DB update
	for _, key := range mediaKeysToDelete {
		media.CleanupObject(c.Request.Context(), h.db, h.store, h.log, key)
	}

	h.log.Info("Server updated", "server_id", serverID, "user_id", userID)
	server.ServerTier = h.serverTiers.GetServerTier(c.Request.Context(), server.ID)

	// Broadcast update to server subscribers so members see changes in real time
	if serverUUID, parseErr := uuid.Parse(serverID); parseErr == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "server_updated",
			Data: map[string]interface{}{
				"server_id":              serverID,
				"name":                   server.Name,
				"icon_url":               server.IconURL,
				"banner_url":             server.BannerURL,
				"allow_embedded_content": server.AllowEmbeddedContent,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"server": server,
		"role":   role,
	})
}

// parseMediaURL parses a json.RawMessage into (provided, *string, error).
// Returns (false, nil, nil) if the field was absent, (true, nil, nil) for explicit null,
// or (true, &url, nil) for a non-null value.
func parseMediaURL(raw json.RawMessage) (provided bool, url *string, err error) {
	if raw == nil {
		return false, nil, nil
	}
	if string(raw) == "null" {
		return true, nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return true, nil, err
	}
	return true, &s, nil
}

// validateMediaURL validates a media URL (icon or banner) against allowed patterns and size limits.
// Returns nil if valid or the URL was nil/empty; returns a descriptive error otherwise.
func validateMediaURL(url *string, expectedPath string, maxDataLen int, label string) error {
	if url == nil || *url == "" {
		return nil
	}
	if *url != expectedPath && !strings.HasPrefix(*url, dataImagePrefix) {
		return fmt.Errorf("%s must be an uploaded %s URL for this server or an image data URL", label, strings.ToLower(label))
	}
	if strings.HasPrefix(*url, dataImagePrefix) && len(*url) > maxDataLen {
		return fmt.Errorf("%s image too large", label)
	}
	return nil
}

// buildUpdateClauses constructs the SET clauses, args, and media cleanup keys for UpdateServer.
func buildUpdateClauses(
	serverID string,
	req UpdateServerRequest,
	iconURLProvided bool, iconURL *string,
	bannerURLProvided bool, bannerURL *string,
) (setClauses []string, args []interface{}, argIdx int, mediaKeysToDelete []string) {
	setClauses = []string{"name = $1", "updated_at = NOW()"}
	args = []interface{}{req.Name}
	argIdx = 2

	if iconURLProvided {
		setClauses = append(setClauses, fmt.Sprintf("icon_url = $%d", argIdx))
		args = append(args, iconURL)
		argIdx++
		if iconURL == nil {
			mediaKeysToDelete = append(mediaKeysToDelete, fmt.Sprintf("server-icons/%s", serverID))
		}
	}
	if bannerURLProvided {
		setClauses = append(setClauses, fmt.Sprintf("banner_url = $%d", argIdx))
		args = append(args, bannerURL)
		argIdx++
		if bannerURL == nil {
			mediaKeysToDelete = append(mediaKeysToDelete, fmt.Sprintf("server-banners/%s", serverID))
		}
	}
	if req.AllowEmbeddedContent != nil {
		setClauses = append(setClauses, fmt.Sprintf("allow_embedded_content = $%d", argIdx))
		args = append(args, *req.AllowEmbeddedContent)
		argIdx++
	}

	return setClauses, args, argIdx, mediaKeysToDelete
}

// DeleteServer deletes a server (owner only)
func (h *Handler) DeleteServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Check if user is owner
	var ownerID string
	ownerQuery := `
		SELECT owner_id FROM servers WHERE id = $1
	`

	err := h.db.QueryRow(ownerQuery, serverID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check ownership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDelete})
		return
	}

	if ownerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can delete the server"})
		return
	}

	// Delete server (cascades to members and channels)
	deleteQuery := `DELETE FROM servers WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, serverID)
	if err != nil {
		h.log.Error("Failed to delete server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDelete})
		return
	}

	h.log.Info("Server deleted", "server_id", serverID, "user_id", userID)

	// Broadcast deletion to server subscribers so frontends can clean up
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "server_deleted",
			Data: map[string]interface{}{
				"server_id": serverID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Server deleted successfully"})
}
