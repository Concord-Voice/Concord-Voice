package invites

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgInvalidServerID   = "Invalid server ID"
	errMsgInvalidInviteCode = "Invalid invite code"
	errMsgFailedJoinServer  = "Failed to join server"
)

// PublicInviteIconSVG is the shared anonymous fallback for invite icon routes.
const PublicInviteIconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128"><rect width="128" height="128" rx="28" fill="#20242c"/><circle cx="64" cy="58" r="30" fill="#eef3ff"/><path d="M34 106c6-20 20-31 30-31s24 11 30 31" fill="#eef3ff"/></svg>`

// Handler handles invite-related requests.
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	hub      *websocket.Hub
	resolver *rbac.Resolver
}

// NewHandler creates a new invite handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver) *Handler {
	return &Handler{db: db, log: log, hub: hub, resolver: resolver}
}

// createInviteRequest is the JSON body for creating an invite.
type createInviteRequest struct {
	MaxUses   *int `json:"max_uses"`   // nil → default 1
	ExpiresIn *int `json:"expires_in"` // seconds; nil → default 86400 (24h)
}

// joinServerRequest is the JSON body for joining via invite code.
type joinServerRequest struct {
	Code string `json:"code" binding:"required"`
}

// --- helpers ---

func (h *Handler) checkInvitePermission(c *gin.Context, serverID, userID string) bool {
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermInvite)
	if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal error"})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return false
	}
	return true
}

func resolveMaxUses(req createInviteRequest) *int {
	if req.MaxUses == nil {
		defaultMax := 1
		return &defaultMax
	}
	if *req.MaxUses <= 0 {
		return nil // unlimited
	}
	maxUses := *req.MaxUses
	if maxUses > 100 {
		maxUses = 100
	}
	return &maxUses
}

func resolveExpiresIn(req createInviteRequest) int {
	if req.ExpiresIn == nil {
		return 86400
	}
	sec := *req.ExpiresIn
	if sec < 300 {
		return 300
	}
	if sec > 604800 {
		return 604800
	}
	return sec
}

type txQuerier interface {
	QueryRow(query string, args ...interface{}) *sql.Row
	Exec(stmt string, args ...interface{}) (sql.Result, error)
}

func validateInvite(invite models.ServerInvite) (int, string) {
	if invite.IsRevoked {
		return http.StatusGone, "This invite has been revoked"
	}
	if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now().UTC()) {
		return http.StatusGone, "This invite has expired"
	}
	if invite.MaxUses != nil && *invite.MaxUses > 0 && invite.UseCount >= *invite.MaxUses {
		return http.StatusGone, "This invite has reached its maximum uses"
	}
	return 0, ""
}

type publicInvitePreview struct {
	serverName string
	serverIcon *string
	expiresAt  *time.Time
	isRevoked  bool
	maxUses    *int
	useCount   int
}

func (p publicInvitePreview) valid(now time.Time) bool {
	if p.isRevoked {
		return false
	}
	if p.expiresAt != nil && !p.expiresAt.After(now) {
		return false
	}
	return p.maxUses == nil || *p.maxUses == 0 || p.useCount < *p.maxUses
}

func checkBanAndMembership(tx txQuerier, serverID, userID string) (int, string, error) {
	var isBanned bool
	err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isBanned)
	if err != nil {
		return http.StatusInternalServerError, errMsgFailedJoinServer, err
	}
	if isBanned {
		return http.StatusForbidden, "You are banned from this server", nil
	}

	var exists bool
	err = tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&exists)
	if err != nil {
		return http.StatusInternalServerError, errMsgFailedJoinServer, err
	}
	if exists {
		return http.StatusConflict, "You are already a member of this server", nil
	}
	return 0, "", nil
}

func addMemberToServer(tx txQuerier, serverID, userID, inviteID string) error {
	_, err := tx.Exec(`
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', NOW())
	`, serverID, userID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`
		INSERT INTO member_roles (server_id, user_id, role_id)
		SELECT $1, $2, id FROM roles
		WHERE server_id = $1 AND is_default = TRUE
	`, serverID, userID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`UPDATE server_invites SET use_count = use_count + 1 WHERE id = $1`, inviteID)
	return err
}

func (h *Handler) tryInsertInvite(c *gin.Context, serverID, userID string, maxUsesPtr *int, expiresAt time.Time) (models.ServerInvite, bool) {
	for attempts := 0; attempts < 5; attempts++ {
		code, err := GenerateCode()
		if err != nil {
			h.log.Error("Failed to generate invite code", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invite"})
			return models.ServerInvite{}, false
		}

		invite := models.ServerInvite{
			ID:        uuid.New().String(),
			ServerID:  serverID,
			Code:      code,
			CreatedBy: userID,
			MaxUses:   maxUsesPtr,
			UseCount:  0,
			ExpiresAt: &expiresAt,
			IsRevoked: false,
		}

		insertErr := h.db.QueryRow(`
			INSERT INTO server_invites (id, server_id, code, created_by, max_uses, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW())
			RETURNING created_at
		`, invite.ID, serverID, code, userID, maxUsesPtr, expiresAt).Scan(&invite.CreatedAt)

		if insertErr != nil {
			continue
		}
		return invite, true
	}

	h.log.Error("Failed to create unique invite code after retries")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invite"})
	return models.ServerInvite{}, false
}

func (h *Handler) broadcastMemberJoined(serverID, userID string) {
	var username string
	var displayName *string
	var avatarURL *string
	_ = h.db.QueryRow(
		"SELECT username, display_name, avatar_url FROM users WHERE id = $1", userID,
	).Scan(&username, &displayName, &avatarURL)

	serverUUID, parseErr := uuid.Parse(serverID)
	if parseErr != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "member_joined",
		Data: map[string]interface{}{
			"server_id":    serverID,
			"user_id":      userID,
			"username":     username,
			"display_name": displayName,
			"avatar_url":   avatarURL,
			"role":         "member",
		},
	})
}

func (h *Handler) createPendingKeyRequests(serverID, userID string) {
	rows, qErr := h.db.Query(`
		INSERT INTO pending_key_requests (channel_id, user_id)
		SELECT c.id, $1
		FROM channels c
		WHERE c.server_id = $2
		ON CONFLICT (channel_id, user_id) DO NOTHING
		RETURNING channel_id
	`, userID, serverID)
	if qErr != nil {
		h.log.Error("Failed to create pending key requests", "error", qErr)
		return
	}

	var pendingChannels []string
	for rows.Next() {
		var chID string
		if rows.Scan(&chID) == nil {
			pendingChannels = append(pendingChannels, chID)
		}
	}
	_ = rows.Close()

	if len(pendingChannels) == 0 {
		return
	}

	serverUUID, parseErr := uuid.Parse(serverID)
	if parseErr != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "key_needed",
		Data: map[string]interface{}{
			"server_id":   serverID,
			"user_id":     userID,
			"channel_ids": pendingChannels,
		},
	})
	h.log.Info("Pending key requests created",
		"user_id", userID, "server_id", serverID,
		"channels", len(pendingChannels))
}

// CreateInvite generates a new invite code for a server.
// Default: 1 use, expires in 24 hours. Caller can override.
func (h *Handler) CreateInvite(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var req createInviteRequest
	_ = c.ShouldBindJSON(&req)

	if !h.checkInvitePermission(c, serverID, userID) {
		return
	}

	maxUsesPtr := resolveMaxUses(req)
	expiresAt := time.Now().UTC().Add(time.Duration(resolveExpiresIn(req)) * time.Second)

	invite, ok := h.tryInsertInvite(c, serverID, userID, maxUsesPtr, expiresAt)
	if !ok {
		return
	}

	h.log.Info("Invite created", "server_id", serverID, "created_by", userID)
	c.JSON(http.StatusCreated, gin.H{"invite": invite})
}

// ListInvites returns all invites for a server.
func (h *Handler) ListInvites(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	if !h.checkInvitePermission(c, serverID, userID) {
		return
	}

	rows, err := h.db.Query(`
		SELECT si.id, si.server_id, si.code, si.created_by, si.max_uses,
		       si.use_count, si.expires_at, si.is_revoked, si.created_at,
		       u.username AS creator_username
		FROM server_invites si
		INNER JOIN users u ON si.created_by = u.id
		WHERE si.server_id = $1
		ORDER BY si.created_at DESC
	`, serverID)
	if err != nil {
		h.log.Error("Failed to query invites", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list invites"})
		return
	}
	defer func() { _ = rows.Close() }()

	invites := []models.ServerInviteWithCreator{}
	for rows.Next() {
		var inv models.ServerInviteWithCreator
		if err := rows.Scan(
			&inv.ID, &inv.ServerID, &inv.Code, &inv.CreatedBy,
			&inv.MaxUses, &inv.UseCount, &inv.ExpiresAt,
			&inv.IsRevoked, &inv.CreatedAt, &inv.CreatorUsername,
		); err != nil {
			h.log.Error("Failed to scan invite", "error", err)
			continue
		}
		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating invites", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list invites"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"invites": invites})
}

// RevokeInvite soft-revokes an invite by setting is_revoked = true.
func (h *Handler) RevokeInvite(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	inviteID := c.Param("invite_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(inviteID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid invite ID"})
		return
	}

	if !h.checkInvitePermission(c, serverID, userID) {
		return
	}

	result, err := h.db.Exec(`
		UPDATE server_invites SET is_revoked = TRUE
		WHERE id = $1 AND server_id = $2 AND is_revoked = FALSE
	`, inviteID, serverID)
	if err != nil {
		h.log.Error("Failed to revoke invite", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke invite"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invite not found or already revoked"})
		return
	}

	h.log.Info("Invite revoked", "invite_id", inviteID, "server_id", serverID, "revoked_by", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Invite revoked"})
}

// JoinServer allows any authenticated user to join a server via invite code.
func (h *Handler) JoinServer(c *gin.Context) {
	userID := c.GetString("user_id")

	var req joinServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invite code is required"})
		return
	}

	if len(req.Code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid invite code format"})
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to begin transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedJoinServer})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback", "error", rbErr)
		}
	}()

	invite, err := h.lookupInvite(tx, req.Code)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgInvalidInviteCode})
		return
	}
	if err != nil {
		h.log.Error("Failed to query invite", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedJoinServer})
		return
	}

	if status, msg := validateInvite(invite); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	if status, msg, checkErr := checkBanAndMembership(tx, invite.ServerID, userID); checkErr != nil {
		h.log.Error("Failed to check ban/membership", "error", checkErr)
		c.JSON(status, gin.H{"error": msg})
		return
	} else if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	if err := addMemberToServer(tx, invite.ServerID, userID, invite.ID); err != nil {
		h.log.Error("Failed to add member to server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedJoinServer})
		return
	}

	server, err := h.fetchServer(tx, invite.ServerID)
	if err != nil {
		h.log.Error("Failed to fetch server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedJoinServer})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedJoinServer})
		return
	}

	h.log.Info("User joined server", "user_id", userID, "server_id", invite.ServerID)
	h.broadcastMemberJoined(invite.ServerID, userID)
	h.createPendingKeyRequests(invite.ServerID, userID)

	c.JSON(http.StatusOK, gin.H{"server": server, "role": "member"})
}

func (h *Handler) lookupInvite(tx *sql.Tx, code string) (models.ServerInvite, error) {
	var invite models.ServerInvite
	err := tx.QueryRow(`
		SELECT id, server_id, code, created_by, max_uses, use_count, expires_at, is_revoked, created_at
		FROM server_invites
		WHERE code = $1
		FOR UPDATE
	`, code).Scan(
		&invite.ID, &invite.ServerID, &invite.Code, &invite.CreatedBy,
		&invite.MaxUses, &invite.UseCount, &invite.ExpiresAt,
		&invite.IsRevoked, &invite.CreatedAt,
	)
	return invite, err
}

func (h *Handler) fetchServer(tx *sql.Tx, serverID string) (models.Server, error) {
	var server models.Server
	err := tx.QueryRow(`
		SELECT id, name, icon_url, banner_url, owner_id, allow_embedded_content, created_at, updated_at
		FROM servers WHERE id = $1
	`, serverID).Scan(
		&server.ID, &server.Name, &server.IconURL, &server.BannerURL, &server.OwnerID,
		&server.AllowEmbeddedContent, &server.CreatedAt, &server.UpdatedAt,
	)
	return server, err
}

func (h *Handler) lookupPublicInvitePreview(code string) (publicInvitePreview, error) {
	var preview publicInvitePreview
	err := h.db.QueryRow(`
		SELECT si.expires_at, si.is_revoked, si.max_uses, si.use_count,
		       s.name, s.icon_url
		FROM server_invites si
		INNER JOIN servers s ON si.server_id = s.id
		WHERE si.code = $1
	`, code).Scan(
		&preview.expiresAt, &preview.isRevoked, &preview.maxUses, &preview.useCount,
		&preview.serverName, &preview.serverIcon,
	)
	return preview, err
}

// GetPublicInvitePreview returns an unauthenticated, privacy-trimmed invite
// card for invite.concordvoice.chat.
func (h *Handler) GetPublicInvitePreview(c *gin.Context) {
	code := c.Param("code")
	if !IsValidCode(code) {
		c.JSON(http.StatusOK, gin.H{"valid": false})
		return
	}

	preview, err := h.lookupPublicInvitePreview(code)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, gin.H{"valid": false})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch public invite preview", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch invite preview"})
		return
	}
	if !preview.valid(time.Now().UTC()) {
		c.JSON(http.StatusOK, gin.H{"valid": false})
		return
	}

	body := gin.H{
		"valid":       true,
		"server_name": preview.serverName,
	}
	if preview.serverIcon != nil {
		body["icon_url"] = "/api/v1/invites/" + code + "/icon"
	}
	c.JSON(http.StatusOK, body)
}

// GetPublicInviteIconFallback serves a constant icon when object storage is not
// configured. Production route wiring uses media.ProxyInviteServerIcon.
func (h *Handler) GetPublicInviteIconFallback(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=60, must-revalidate")
	c.Data(http.StatusOK, "image/svg+xml; charset=utf-8", []byte(PublicInviteIconSVG))
}

// GetInviteInfo returns a preview of the server for an invite code.
func (h *Handler) GetInviteInfo(c *gin.Context) {
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidInviteCode})
		return
	}

	var (
		serverID     string
		expiresAt    *time.Time
		isRevoked    bool
		maxUses      *int
		useCount     int
		serverName   string
		serverIcon   *string
		serverBanner *string
		memberCount  int
	)

	err := h.db.QueryRow(`
		SELECT si.server_id, si.expires_at, si.is_revoked, si.max_uses, si.use_count,
		       s.name, s.icon_url, s.banner_url,
		       (SELECT COUNT(*) FROM server_members WHERE server_id = s.id)
		FROM server_invites si
		INNER JOIN servers s ON si.server_id = s.id
		WHERE si.code = $1
	`, code).Scan(
		&serverID, &expiresAt, &isRevoked, &maxUses, &useCount,
		&serverName, &serverIcon, &serverBanner, &memberCount,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgInvalidInviteCode})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch invite info", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch invite info"})
		return
	}

	valid := !isRevoked &&
		(expiresAt == nil || expiresAt.After(time.Now().UTC())) &&
		(maxUses == nil || *maxUses == 0 || useCount < *maxUses)

	c.JSON(http.StatusOK, gin.H{
		"server_name":   serverName,
		"server_icon":   serverIcon,
		"server_banner": serverBanner,
		"member_count":  memberCount,
		"valid":         valid,
	})
}
