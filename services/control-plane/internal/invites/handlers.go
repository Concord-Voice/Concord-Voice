package invites

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

	// graphPresence is the #2447 membership presence capture. nil means unwired.
	graphPresence presencecapture.GraphPresenceCapture
	// snapshots serves the additive (hydrate) direction, outside the
	// presencecapture contract. nil means no hydrate.
	snapshots *presence.ActivitySnapshotService
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

// addMemberToServer inserts the membership row and its default roles inside the
// caller's transaction.
//
// Both inserts carry ON CONFLICT DO NOTHING, matching members.AddMember. That is
// what makes JoinServer's fail-OPEN posture safe: the additive path returns 200
// on a committed row even when post-commit delivery fails (with
// "presence":"pending"), so a client may
// legitimately retry the join, and without ON CONFLICT that retry would 500 on a
// duplicate key.
func addMemberToServer(tx txQuerier, serverID, userID, inviteID string) error {
	_, err := tx.Exec(`
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', NOW())
		ON CONFLICT (server_id, user_id) DO NOTHING
	`, serverID, userID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`
		INSERT INTO member_roles (server_id, user_id, role_id)
		SELECT $1, $2, id FROM roles
		WHERE server_id = $1 AND is_default = TRUE
		ON CONFLICT DO NOTHING
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

	ctx := c.Request.Context()

	// ONE spec for the gates, the capture and the focal set. Invite join is
	// ADDITIVE: FamilyMemberJoin is registered with CanRevokeVisibility false,
	// because true would seed plan.viewers and disconnect every device of the
	// user who just joined.
	spec := presencehook.Spec{
		Family:      presencecapture.FamilyMemberJoin,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: userID,
	}

	var server models.Server
	var serverID string

	// WithGatedTx acquires the sender gates BEFORE opening the transaction and
	// owns both the rollback and — through Complete — the commit. This handler
	// no longer calls tx.Commit(): doing so would yield sql.ErrTxDone.
	err := presencehook.WithGatedTx(ctx, h.graphPresence, h.db, h.log, spec, func(tx *sql.Tx) error {
		invite, lookupErr := h.lookupInvite(tx, req.Code)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return &joinRejection{status: http.StatusNotFound, msg: errMsgInvalidInviteCode}
		}
		if lookupErr != nil {
			return fmt.Errorf("query invite: %w", lookupErr)
		}

		if status, msg := validateInvite(invite); status != 0 {
			return &joinRejection{status: status, msg: msg}
		}

		if status, msg, checkErr := checkBanAndMembership(tx, invite.ServerID, userID); checkErr != nil {
			return fmt.Errorf("check ban/membership: %w", checkErr)
		} else if status != 0 {
			return &joinRejection{status: status, msg: msg}
		}

		serverID = invite.ServerID

		// Capture strictly precedes the write, with nothing between them.
		plan, captureErr := presencehook.Capture(ctx, h.graphPresence, tx, spec)
		if captureErr != nil {
			return fmt.Errorf("capture invite join presence: %w", captureErr)
		}

		if addErr := addMemberToServer(tx, invite.ServerID, userID, invite.ID); addErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("add member to server: %w", addErr)
		}

		fetched, fetchErr := h.fetchServer(tx, invite.ServerID)
		if fetchErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("fetch server: %w", fetchErr)
		}
		server = fetched

		return presencehook.Complete(ctx, h.graphPresence, tx, plan)
	})

	var rejection *joinRejection
	if errors.As(err, &rejection) {
		c.JSON(rejection.status, gin.H{"error": rejection.msg})
		return
	}
	// A DURABLE join whose presence delivery failed must NOT return here. The
	// membership row is committed, and returning would skip broadcastMemberJoined
	// and — far worse — createPendingKeyRequests, leaving the joiner with no path
	// to any channel key. The retry that might repair it does not exist:
	// checkBanAndMembership 409s a second join before the insert is reached. Same
	// class as the kick/ban fall-through (code review, PR #2840).
	var deliveryFailure *presencehook.Failure
	if err != nil {
		failure := presencehook.Classify(err)
		h.log.Error("Failed to join server", "failure_class", failure.Code, "error", err)
		if !errors.Is(err, presencecapture.ErrPostCommitDelivery) {
			if retryAfter, ok := failure.RetryAfterHeader(); ok {
				c.Header(presencehook.HeaderRetryAfter, retryAfter)
			}
			c.JSON(failure.Status, gin.H{"error": failure.Body(errMsgFailedJoinServer)})
			return
		}
		deliveryFailure = &failure
	}

	server.ServerTier = entitlements.ResolveServerTier(ctx, h.db, server.ID)

	h.log.Info("User joined server", "user_id", userID, "server_id", serverID)
	h.broadcastMemberJoined(serverID, userID)
	h.createPendingKeyRequests(serverID, userID)

	// Post-commit and fail-OPEN, exactly as members.AddMember: a missed hydrate
	// shows the joiner less than they are entitled to and self-corrects.
	h.hydrateJoinerPresence(ctx, userID)

	// Joined, broadcast and key-requested; only presence delivery is unsettled.
	// The additive posture fails OPEN, so this stays a success — it just reports
	// the unsettled fan-out rather than a bare 200.
	if deliveryFailure != nil {
		c.JSON(http.StatusOK, gin.H{"server": server, "role": "member", "presence": "pending"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"server": server, "role": "member"})
}

// joinRejection carries a non-5xx outcome out of the hooked transaction. The
// pre-hook handler wrote its response inline and relied on the deferred
// rollback; WithGatedTx's closure must return an error instead, so the status
// and message ride along rather than being flattened into a generic 500.
type joinRejection struct {
	status int
	msg    string
}

func (e *joinRejection) Error() string { return e.msg }

// hydrateJoinerPresence pushes the newly authorized viewer their current
// snapshot. It runs AFTER the commit and NEVER changes the response: hydration
// has no minuend, so a missed hydrate shows the joiner less than they are
// entitled to and self-corrects on the next presence event.
//
// Duplicated in members and invites, and it stays that way — the #2840 scope
// review proposed hoisting it, and BOTH obvious homes are blocked by a real
// constraint. internal/presence cannot host it: that package forbids importing
// pkg/logger at all, enforced by TestActivityProductionEmitsNoPayloadOrIdentityLogs,
// so a helper that logs cannot live there. internal/presencehook cannot host it
// either: it deliberately imports only the zero-internal-dependency
// presencecapture leaf, and giving it a dependency on presence to host a helper
// would invert that layering. Twelve duplicated lines is the cheaper defect.
func (h *Handler) hydrateJoinerPresence(ctx context.Context, viewerID string) {
	if h.snapshots == nil {
		return
	}
	parsed, parseErr := uuid.Parse(viewerID)
	if parseErr != nil {
		h.log.Error("Presence hydrate skipped", "failure_class", "invalid_viewer")
		return
	}
	if _, err := h.snapshots.Snapshot(ctx, parsed); err != nil {
		h.log.Error("Presence hydrate failed", "failure_class", "delivery")
	}
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
	// The edge rate-limit rule matches on the RAW wire path, but gin routes on
	// the percent-DECODED path, so /…/CODE%2Fpreview reaches this handler while
	// matching no WAF rule — no managed challenge, no edge bucket.
	// URL.RawPath is non-empty only when the raw and decoded forms differ, i.e.
	// exactly when something was percent-encoded, and no legitimate caller
	// encodes anything here: the code charset is entirely RFC-3986 unreserved.
	// Reject in the SAME uniform shape as every other invalid class, so closing
	// the rate-limit bypass introduces no enumeration oracle (#945, VULN-001).
	if c.Request.URL.RawPath != "" {
		c.JSON(http.StatusOK, gin.H{"valid": false})
		return
	}

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

// SetGraphPresenceCapture wires the #2447 membership presence capture. A nil
// capture leaves this handler behaving exactly as it did before the hook, so a
// replica without it degrades to the pre-existing <=90s presence TTL.
func (h *Handler) SetGraphPresenceCapture(c presencecapture.GraphPresenceCapture) {
	h.graphPresence = c
}

// HasGraphPresenceCapture reports whether the capture was wired. The router's
// boot guard interrogates the HANDLER through this, never the constructed
// reconciler value: graphpresence.New always returns a non-nil pointer, so a
// check on that value is a tautology that still boots with the wiring line
// deleted -- the one fail-OPEN path the guard exists to catch.
func (h *Handler) HasGraphPresenceCapture() bool { return h.graphPresence != nil }

// SetActivitySnapshots wires the viewer-scoped snapshot service the additive
// direction hydrates through. Nil means no hydrate, which is a safe degrade:
// hydration has no minuend, so a missed hydrate shows the joiner LESS than they
// are entitled to and self-corrects on the next presence event.
func (h *Handler) SetActivitySnapshots(s *presence.ActivitySnapshotService) {
	h.snapshots = s
}
