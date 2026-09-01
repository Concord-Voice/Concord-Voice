// Package servers provides handlers for managing Concord servers (Discord-like communities).
package servers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	dataImagePrefix             = "data:image/"
	dataURLHeaderSlack          = 64
	errMsgInvalidServerID       = "Invalid server ID"
	errMsgServerNotFound        = "Server not found"
	errMsgFailedCreate          = "Failed to create server"
	errMsgFailedFetch           = "Failed to fetch server"
	errMsgFailedUpdate          = "Failed to update server"
	errMsgFailedDelete          = "Failed to delete server"
	errMsgServerActivityChanged = "Server activity changed; retry deletion"
)

// Handler handles server-related requests
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	resolver    *rbac.Resolver
	tiers       entitlements.TierResolver       // user-axis tier resolution (#1555 server-creation cap)
	serverTiers entitlements.ServerTierResolver // server-axis tier resolution (#1521)
	store       media.ObjectDeleter             // nil when object storage is not configured

	// graphPresence is the #2447 membership presence capture. nil means unwired.
	graphPresence presencecapture.GraphPresenceCapture
	activePlans   ActivePlanRail
}

// ActivePlanRail is the narrow durable active-category seam used by server
// deletion. The concrete rail remains owned by internal/activepresence.
type ActivePlanRail interface {
	WithGatedRevocationTx(context.Context, []uuid.UUID, func() func(), func(*sql.Tx) error) error
	CapturePlansTx(context.Context, *sql.Tx, []activepresence.Plan) error
	CompleteAlreadyGated(context.Context, *sql.Tx, []activepresence.PlanKey) error
}

// NewHandler creates a new server handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, tiers entitlements.TierResolver, serverTiers entitlements.ServerTierResolver) *Handler {
	return &Handler{
		db:          db,
		log:         log,
		hub:         hub,
		resolver:    resolver,
		tiers:       tiers,
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

	// #1555 server-creation cap: resolve the user's tier before opening the
	// transaction (the resolver may touch Redis/DB; keep the tx short).
	// Fail-closed: unknown tier resolves to the free set (cap 5).
	ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), userID))

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

	// The cap counts servers currently OWNED (deleting one frees a slot);
	// negative = unlimited (premium). Count + INSERT run in the same
	// transaction; the residual concurrent-create race is accepted — the cap
	// is a product boundary, not a security invariant (#1555 spec).
	if ent.MaxServersCreated >= 0 {
		var owned int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM servers WHERE owner_id = $1`, userID).Scan(&owned); err != nil {
			h.log.Error("Failed to count owned servers", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreate})
			return
		}
		if owned >= ent.MaxServersCreated {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Server limit reached",
				"code":  "server_cap_reached",
				"limit": ent.MaxServersCreated,
			})
			return
		}
	}

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
		if err := media.CleanupObject(c.Request.Context(), h.db, h.store, key); err != nil {
			h.log.Warn("Server media cleanup incomplete")
		}
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
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	ctx := c.Request.Context()
	candidates, err := h.preflightServerVoiceCandidates(ctx, serverID, userID)
	switch {
	case errors.Is(err, errServerDeleteNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	case errors.Is(err, errServerDeleteNotOwner):
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can delete the server"})
		return
	case errors.Is(err, errServerVoiceCandidateConflict):
		h.log.Warn(errMsgFailedDelete, "failure_class", serverVoiceCandidateFailureClass(err))
		c.JSON(http.StatusConflict, gin.H{"error": errMsgServerActivityChanged})
		return
	case err != nil:
		h.log.Error(errMsgFailedDelete, "failure_class", "preflight")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDelete})
		return
	}
	if h.activePlans == nil {
		h.log.Error(errMsgFailedDelete, "failure_class", "rail_unwired")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDelete})
		return
	}

	// C3 Server Voice captures exact durable plans. C2 Custom Status remains the
	// bounded member-audience disconnect below, after the gated bracket closes.
	// The destructive lock order is gates -> fence -> Server Voice advisories ->
	// users -> server -> channels.
	var outcome serverDeleteOutcome
	err = h.activePlans.WithGatedRevocationTx(ctx, candidateSubjectIDs(candidates), func() func() {
		return h.hub.BeginAudienceRevocation()
	}, func(tx *sql.Tx) error {
		return h.deleteServerWithActivePlans(
			ctx, tx, serverID, userID, candidates, &outcome,
		)
	})
	if outcome.committed {
		h.reconcileServerDeleteAudience(ctx, outcome.affected, outcome.oversized)
		h.log.Info("Server deleted", "server_id", serverID, "user_id", userID)
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "server_deleted",
			Data: map[string]interface{}{
				"server_id": serverID,
			},
		})
	}
	if err != nil {
		if outcome.committed {
			h.log.Error(errMsgFailedDelete, "failure_class", "delivery")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Server deleted; presence cleanup is pending"})
			return
		}
		if errors.Is(err, errServerDeleteNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
			return
		}
		if errors.Is(err, errServerDeleteNotOwner) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can delete the server"})
			return
		}
		if errors.Is(err, errServerVoiceCandidateConflict) {
			h.log.Warn(errMsgFailedDelete, "failure_class", serverVoiceCandidateFailureClass(err))
			c.JSON(http.StatusConflict, gin.H{"error": errMsgServerActivityChanged})
			return
		}
		h.log.Error(errMsgFailedDelete, "failure_class", serverDeleteTransactionFailureClass(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDelete})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Server deleted successfully"})
}

const maxServerVoiceCandidates = 16

var (
	errServerDeleteNotFound         = errors.New("servers: server was not found")
	errServerDeleteNotOwner         = errors.New("servers: server ownership changed")
	errServerVoiceCandidateConflict = errors.New("servers: server voice candidates changed")
	errServerDeleteCommitUnresolved = errors.New("servers: server deletion commit is unresolved")
)

var (
	errServerVoiceCandidateBound     = fmt.Errorf("%w: bound exceeded", errServerVoiceCandidateConflict)
	errServerVoiceCandidateAmbiguous = fmt.Errorf("%w: duplicate evidence", errServerVoiceCandidateConflict)
	errServerVoiceCandidateDrift     = fmt.Errorf("%w: candidate drift", errServerVoiceCandidateConflict)
)

func serverVoiceCandidateFailureClass(err error) string {
	switch {
	case errors.Is(err, errServerVoiceCandidateBound):
		return "candidate_bound"
	case errors.Is(err, errServerVoiceCandidateAmbiguous):
		return "candidate_ambiguous"
	default:
		return "candidate_drift"
	}
}

func serverDeleteTransactionFailureClass(err error) string {
	if errors.Is(err, errServerDeleteCommitUnresolved) {
		return "commit_unresolved"
	}
	return "transaction"
}

type serverVoiceCandidate struct {
	subjectID        uuid.UUID
	channelID        uuid.UUID
	lifecycleEventAt time.Time
}

type serverDeleteOutcome struct {
	committed bool
	affected  []uuid.UUID
	oversized bool
}

func (h *Handler) preflightServerVoiceCandidates(ctx context.Context, serverID, userID string) (candidates []serverVoiceCandidate, returnErr error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin server-delete preflight: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("rollback server-delete preflight: %w", rollbackErr))
		}
	}()

	var ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errServerDeleteNotFound
		}
		return nil, fmt.Errorf("lock server-delete preflight: %w", err)
	}
	if ownerID != userID {
		return nil, errServerDeleteNotOwner
	}
	candidates, err = readServerVoiceCandidates(ctx, tx, serverID)
	if err != nil {
		return nil, err
	}
	if err := validateServerVoiceCandidates(candidates, nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit server-delete preflight: %w", err)
	}
	return candidates, nil
}

func (h *Handler) deleteServerWithActivePlans(
	ctx context.Context,
	tx *sql.Tx,
	serverID, userID string,
	preflight []serverVoiceCandidate,
	outcome *serverDeleteOutcome,
) error {
	for _, candidate := range preflight {
		if err := voice.LockServerVoiceLifecycleTx(ctx, tx, candidate.subjectID); err != nil {
			return fmt.Errorf("lock server voice lifecycle: %w", err)
		}
	}
	survivingUsers, err := lockServerVoiceCandidateUsers(ctx, tx, preflight)
	if err != nil {
		return err
	}

	var ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errServerDeleteNotFound
		}
		return fmt.Errorf("lock server deletion: %w", err)
	}
	if ownerID != userID {
		return errServerDeleteNotOwner
	}
	channelParents, err := lockServerVoiceChannelParents(ctx, tx, serverID)
	if err != nil {
		return err
	}
	final, err := readServerVoiceCandidates(ctx, tx, serverID)
	if err != nil {
		return err
	}
	final = preserveDeletedChannelCandidates(preflight, final, survivingUsers, channelParents)
	allowed := make(map[uuid.UUID]struct{}, len(preflight))
	for _, candidate := range preflight {
		allowed[candidate.subjectID] = struct{}{}
	}
	if err := validateServerVoiceCandidates(final, allowed); err != nil {
		return err
	}

	plans, keys := serverVoicePlans(final)
	if err := h.activePlans.CapturePlansTx(ctx, tx, plans); err != nil {
		return fmt.Errorf("capture server voice active plans: %w", err)
	}
	capturedAudience, captureErr := h.captureServerAudience(ctx, tx, serverID)
	outcome.oversized = errors.Is(captureErr, errServerAudienceTooLarge)
	if captureErr != nil && !outcome.oversized {
		return captureErr
	}
	outcome.affected = capturedAudience
	if _, err := tx.ExecContext(ctx, `DELETE FROM servers WHERE id = $1`, serverID); err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %w", errServerDeleteCommitUnresolved, err)
	}
	outcome.committed = true
	return h.activePlans.CompleteAlreadyGated(ctx, nil, keys)
}

func readServerVoiceCandidates(
	ctx context.Context,
	tx *sql.Tx,
	serverID string,
) (candidates []serverVoiceCandidate, returnErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT vp.user_id, vp.channel_id, vp.lifecycle_event_at
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE c.server_id = $1
		ORDER BY vp.user_id, vp.channel_id
		LIMIT 17`, serverID)
	if err != nil {
		return nil, fmt.Errorf("read server voice candidates: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close server voice candidates: %w", closeErr))
		}
	}()
	candidates = make([]serverVoiceCandidate, 0, maxServerVoiceCandidates)
	for rows.Next() {
		var candidate serverVoiceCandidate
		if err := rows.Scan(&candidate.subjectID, &candidate.channelID, &candidate.lifecycleEventAt); err != nil {
			return nil, fmt.Errorf("scan server voice candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate server voice candidates: %w", err)
	}
	return candidates, nil
}

func validateServerVoiceCandidates(candidates []serverVoiceCandidate, allowed map[uuid.UUID]struct{}) error {
	seen := make(map[uuid.UUID]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, exists := seen[candidate.subjectID]; exists {
			return errServerVoiceCandidateAmbiguous
		}
		seen[candidate.subjectID] = struct{}{}
	}
	if len(seen) > maxServerVoiceCandidates {
		return errServerVoiceCandidateBound
	}
	if allowed == nil {
		return nil
	}
	for subjectID := range seen {
		if _, exists := allowed[subjectID]; !exists {
			return errServerVoiceCandidateDrift
		}
	}
	return nil
}

func candidateSubjectIDs(candidates []serverVoiceCandidate) []uuid.UUID {
	subjects := make([]uuid.UUID, 0, len(candidates))
	for _, candidate := range candidates {
		subjects = append(subjects, candidate.subjectID)
	}
	sort.Slice(subjects, func(left, right int) bool { return subjects[left].String() < subjects[right].String() })
	return subjects
}

func lockServerVoiceCandidateUsers(
	ctx context.Context,
	tx *sql.Tx,
	candidates []serverVoiceCandidate,
) (surviving map[uuid.UUID]struct{}, returnErr error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	ids := candidateSubjectIDs(candidates)
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE`, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("lock server voice users: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close locked server voice users: %w", closeErr))
		}
	}()
	surviving = make(map[uuid.UUID]struct{}, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan locked server voice user: %w", err)
		}
		surviving[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate locked server voice users: %w", err)
	}
	return surviving, nil
}

func lockServerVoiceChannelParents(
	ctx context.Context,
	tx *sql.Tx,
	serverID string,
) (parents map[uuid.UUID]struct{}, returnErr error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM channels WHERE server_id = $1 ORDER BY id FOR UPDATE`, serverID)
	if err != nil {
		return nil, fmt.Errorf("lock server voice channel parents: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close locked server voice channel parents: %w", closeErr))
		}
	}()
	parents = make(map[uuid.UUID]struct{})
	for rows.Next() {
		var channelID uuid.UUID
		if err := rows.Scan(&channelID); err != nil {
			return nil, fmt.Errorf("scan locked server voice channel parent: %w", err)
		}
		parents[channelID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate locked server voice channel parents: %w", err)
	}
	return parents, nil
}

func preserveDeletedChannelCandidates(
	preflight, final []serverVoiceCandidate,
	survivingUsers, channelParents map[uuid.UUID]struct{},
) []serverVoiceCandidate {
	finalSubjects := make(map[uuid.UUID]struct{}, len(final))
	for _, candidate := range final {
		finalSubjects[candidate.subjectID] = struct{}{}
	}
	for _, candidate := range preflight {
		_, alreadyFinal := finalSubjects[candidate.subjectID]
		_, userSurvived := survivingUsers[candidate.subjectID]
		_, channelSurvived := channelParents[candidate.channelID]
		if alreadyFinal || !userSurvived || channelSurvived {
			continue
		}
		final = append(final, candidate)
	}
	sort.Slice(final, func(left, right int) bool {
		if final[left].subjectID != final[right].subjectID {
			return final[left].subjectID.String() < final[right].subjectID.String()
		}
		return final[left].channelID.String() < final[right].channelID.String()
	})
	return final
}

func serverVoicePlans(candidates []serverVoiceCandidate) ([]activepresence.Plan, []activepresence.PlanKey) {
	plans := make([]activepresence.Plan, 0, len(candidates))
	keys := make([]activepresence.PlanKey, 0, len(candidates))
	for _, candidate := range candidates {
		plans = append(plans, activepresence.Plan{
			SubjectID: candidate.subjectID, Category: activepresence.CategoryServerVoice,
			OperationID: uuid.New(), Resolution: activepresence.ResolutionExact,
			LifecycleID: candidate.channelID, EventAt: candidate.lifecycleEventAt,
		})
		keys = append(keys, activepresence.PlanKey{
			SubjectID: candidate.subjectID, Category: activepresence.CategoryServerVoice,
		})
	}
	return plans, keys
}

func (h *Handler) reconcileServerDeleteAudience(ctx context.Context, affected []uuid.UUID, oversized bool) {
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serverDeletePresenceTimeout)
	defer cancel()
	if oversized {
		h.log.Info("Server delete audience exceeded the capture bound; disconnecting conservatively")
		if h.hub != nil {
			if err := h.hub.DisconnectAllRichPresenceClients(reconcileCtx); err != nil {
				h.log.Error("Server delete conservative disconnect failed", "failure_class", "delivery")
			}
		}
		return
	}
	h.disconnectServerAudience(reconcileCtx, affected)
}

// SetGraphPresenceCapture wires the #2447 membership presence capture. A nil
// capture leaves this handler behaving exactly as it did before the hook, so a
// replica without it degrades to the pre-existing <=90s presence TTL.
func (h *Handler) SetGraphPresenceCapture(c presencecapture.GraphPresenceCapture) {
	h.graphPresence = c
}

// SetActivePlanRail wires the durable Server Voice deletion producer.
func (h *Handler) SetActivePlanRail(rail ActivePlanRail) { h.activePlans = rail }

// HasActivePlanRail reports whether the deletion producer was wired at boot.
func (h *Handler) HasActivePlanRail() bool { return h.activePlans != nil }

// HasGraphPresenceCapture reports whether the capture was wired. The router's
// boot guard interrogates the HANDLER through this, never the constructed
// reconciler value: graphpresence.New always returns a non-nil pointer, so a
// check on that value is a tautology that still boots with the wiring line
// deleted -- the one fail-OPEN path the guard exists to catch.
func (h *Handler) HasGraphPresenceCapture() bool { return h.graphPresence != nil }

// serverDeletePresenceTimeout bounds the post-commit presence reconcile. It is
// a const rather than configuration, matching the bounds discipline the other
// capture paths use.
const serverDeletePresenceTimeout = 10 * time.Second

// maxServerDeleteAudience bounds the in-memory capture. Every other capture path
// carries a const bound (maxFocalSenders, maxCapturedViewers); this hand-rolled
// one did not, so a very large server handed an unbounded slice to a synchronous
// in-request disconnect (security review, PR #2840).
const maxServerDeleteAudience = 5000

// errServerAudienceTooLarge signals that the member set exceeded
// maxServerDeleteAudience. The caller degrades to the conservative fleet-wide
// disconnect rather than reconciling a truncated audience: a partial clear would
// leave the untouched remainder holding revoked state with no signal at all,
// which is worse than over-disconnecting.
var errServerAudienceTooLarge = errors.New("servers: server audience exceeds the capture bound")

// captureServerAudience reads the member set that defines this server's presence
// audience, inside the caller's transaction and BEFORE the delete.
//
// Fails closed: a read error refuses the delete rather than proceeding with an
// audience the handler can no longer reconstruct. Once DELETE FROM servers runs,
// server_members has cascaded and this information is gone for good.
func (h *Handler) captureServerAudience(ctx context.Context, tx *sql.Tx, serverID string) ([]uuid.UUID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM server_members WHERE server_id = $1`, serverID)
	if err != nil {
		return nil, fmt.Errorf("read server audience: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []uuid.UUID
	for rows.Next() {
		var member uuid.UUID
		if scanErr := rows.Scan(&member); scanErr != nil {
			return nil, fmt.Errorf("scan server audience: %w", scanErr)
		}
		members = append(members, member)
		if len(members) > maxServerDeleteAudience {
			return nil, errServerAudienceTooLarge
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate server audience: %w", rowsErr)
	}
	return members, nil
}

// disconnectServerAudience closes the captured members' Rich Presence clients so
// each reconnect rebuilds an authorized snapshot. The deleted server's rows are
// gone by then, so the rebuild simply omits it.
//
// A disconnect rather than an exact clear, deliberately. ClearServerVoice is
// keyed on the specific voice channel a sender occupies. Server Voice now has
// exact durable plans; this remains only the bounded C2 Custom Status
// compensation, which cannot be represented on the active-presence rail.
func (h *Handler) disconnectServerAudience(ctx context.Context, members []uuid.UUID) {
	if h.hub == nil || len(members) == 0 {
		return
	}
	recipients := make(map[uuid.UUID]bool, len(members))
	for _, member := range members {
		recipients[member] = true
	}
	if err := h.hub.DisconnectRichPresenceClients(ctx, recipients); err != nil {
		h.log.Error("Server delete presence disconnect failed", "failure_class", "delivery")
	}
	h.log.Info("Server delete presence reconciled", "member_count", len(members))
}
