// Package members provides handlers for managing server membership.
package members

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgInvalidServerID        = "Invalid server ID"
	errMsgInvalidUserID          = "Invalid user ID"
	errMsgInvalidRequestBody     = "Invalid request body"
	errMsgInsufficientPerms      = "insufficient permissions"
	errMsgFailedFetchMembers     = "Failed to fetch members"
	errMsgMissingMemberPublicKey = "Every server member needs a public key for secure channel creation"
	errMsgFailedAddMember        = "Failed to add member"
	errMsgFailedUpdateMember     = "Failed to update member"
	errMsgFailedRemoveMember     = "Failed to remove member"
	errMsgFailedBanMember        = "Failed to ban member"
	errMsgFailedTimeoutMember    = "Failed to timeout member"
	errMsgFailedGetServerOwner   = "Failed to get server owner"
	errMsgFailedCheckPerms       = "Failed to check permissions"
	errMsgUserNotMember          = "User is not a member of this server"
	errMsgNotMember              = "Not a member of this server"

	minTimeoutDuration = time.Minute
	maxTimeoutDuration = 7 * 24 * time.Hour
	// permissionCacheInvalidationTimeout bounds post-commit Redis cleanup without
	// tying it to a client connection that may have already been cancelled.
	permissionCacheInvalidationTimeout = 3 * time.Second
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
	// voiceEnforcer pushes recomputed permissions to voice-connected members
	// after a membership change (CV-CAN-007 review P1). A kick/leave/ban deletes
	// server_members but leaves the media-plane participant holding its join-time
	// snapshot, so it could keep publishing until it voluntarily left. Wired via
	// SetVoiceEnforcer; nil means no push (the pre-push, join-snapshot behavior).
	voiceEnforcer rbac.VoiceEnforcer
	// purger backs the optional purge-on-ban/kick (#1353). Wired via
	// SetServerMessagePurger; nil means the moderation purge fails closed (skipped).
	purger serverMessagePurger
	// purgeRateLimit / purgeRateWindow are the fail-closed per-actor budget for the
	// moderation purge, wired from the same resolvePurgeRateLimit(cfg) values the standalone
	// purge endpoint uses (#1353 review, Codex P2) so PURGE_RATE_LIMIT/WINDOW overrides apply
	// consistently. Zero falls back to the package defaults.
	purgeRateLimit  int
	purgeRateWindow time.Duration

	// graphPresence is the #2447 membership presence capture. nil means unwired.
	graphPresence presencecapture.GraphPresenceCapture
	// snapshots serves the additive (hydrate) direction, outside the
	// presencecapture contract. nil means no hydrate.
	snapshots *presence.ActivitySnapshotService
}

// SetVoiceEnforcer wires the mid-session voice permission push. Called once at
// router construction, before the handler serves traffic.
func (h *Handler) SetVoiceEnforcer(e rbac.VoiceEnforcer) {
	h.voiceEnforcer = e
}

// recheckVoiceUser re-pushes permissions for a member who may be sitting in a
// voice channel right now. After a membership deletion the enforcer's fresh
// resolve returns ErrNotMember and publishes voice.enforce.disconnect, evicting
// the removed member from the room. Nil-safe: a no-op without an enforcer.
func (h *Handler) recheckVoiceUser(serverID, userID string) {
	if h.voiceEnforcer == nil {
		return
	}
	h.voiceEnforcer.RecheckUser(serverID, userID)
}

// disconnectVoiceUser force-disconnects a member from any voice channel they are
// currently in. Used by the timeout path: a timed-out member is barred from
// voice by AuthorizeJoin via timed_out_until, a gate independent of the
// permission bitfield, so a recheck would re-push their unchanged bits and never
// evict them. Nil-safe: a no-op without an enforcer.
func (h *Handler) disconnectVoiceUser(serverID, userID string) {
	if h.voiceEnforcer == nil {
		return
	}
	h.voiceEnforcer.DisconnectUser(serverID, userID)
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
		rotator:  keyrotation.NewRotator(db, log, resolver.CanDistributeChannelKeyTx, websocket.KeyRevocationBroadcaster(hub)),
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

// MemberPublicKey is a server member's current public key for E2EE wrapping.
type MemberPublicKey struct {
	UserID     string `json:"user_id"`
	PublicKey  string `json:"public_key"`
	KeyVersion int    `json:"key_version"`
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
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
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

// ListMemberPublicKeys returns current E2EE public keys for the server's members.
func (h *Handler) ListMemberPublicKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM server_members
			WHERE server_id = $1 AND user_id = $2
		)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMembers})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	}

	rows, err := h.db.Query(
		`SELECT sm.user_id, pk.public_key, pk.key_version
		 FROM server_members sm
		 LEFT JOIN LATERAL (
			SELECT public_key, key_version FROM public_keys
			WHERE user_id = sm.user_id ORDER BY key_version DESC LIMIT 1
		 ) pk ON TRUE
		 WHERE sm.server_id = $1
		 ORDER BY sm.joined_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query member public keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMembers})
		return
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			h.log.Error("Failed to close member public key rows", "error", closeErr)
		}
	}()

	keys := []MemberPublicKey{}
	for rows.Next() {
		var key MemberPublicKey
		var publicKey []byte
		var keyVersion sql.NullInt64
		if err := rows.Scan(&key.UserID, &publicKey, &keyVersion); err != nil {
			h.log.Error("Failed to scan member public key", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMembers})
			return
		}
		if !keyVersion.Valid {
			c.JSON(http.StatusConflict, gin.H{"error": errMsgMissingMemberPublicKey})
			return
		}
		key.PublicKey = base64.StdEncoding.EncodeToString(publicKey)
		key.KeyVersion = int(keyVersion.Int64)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating member public keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMembers})
		return
	}

	c.JSON(http.StatusOK, gin.H{"members": keys})
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

	ctx := c.Request.Context()

	// Probe-then-gate (#2854 stage C, finding C). Adding a user who is already
	// a member is a proven no-op, and entering WithGatedTx for it holds one of
	// 64 shared sender stripes on a user who is not party to this request.
	//
	// Placed AFTER the permission and user-exists checks on purpose: earlier,
	// the 409-vs-fallthrough distinction would be a membership oracle for a
	// server the caller cannot see.
	//
	// MEMBERSHIP ONLY. The ban read stays inside the transaction -- hoisting it
	// lost a race once already, and the ON CONFLICT insert re-established
	// membership for a banned user.
	//
	// Non-authoritative: the in-transaction alreadyMember read below remains the
	// authority and is unchanged. FAILS OPEN -- a probe error falls through to
	// today's gated path, because an unreadable probe proves nothing.
	//
	// Accepted delta: a target removed between this read and the response yields
	// a retryable 409 where today it would 201.
	if alreadyMember, probeErr := h.checkMembership(ctx, serverID, req.UserID); probeErr == nil && alreadyMember {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a member"})
		return
	}

	// ONE spec for the gates, the capture and the focal set, so the three cannot
	// drift apart. Direct add is ADDITIVE: FamilyMemberAdd is registered with
	// CanRevokeVisibility false, because true would seed plan.viewers and tear
	// down every device of the user who was just added.
	spec := presencehook.Spec{
		Family:      presencecapture.FamilyMemberAdd,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: req.UserID,
	}

	// WithGatedTx acquires the sender gates BEFORE opening the transaction and
	// owns the deferred rollback. Both writes now share that transaction: the
	// default-role insert used to be a blank-discarded h.db.Exec, so a failure
	// left a member with no roles and still returned 201.
	err = presencehook.WithGatedTx(ctx, h.graphPresence, h.db, h.log, spec, func(tx *sql.Tx) error {
		// The ban and existing-membership reads run INSIDE the transaction and
		// BEFORE the capture, for two separate reasons.
		//
		// Ban: this read used to be an autocommit h.db.QueryRow before the
		// transaction opened, so a ban committing in that window still lost the
		// race — the ON CONFLICT insert below would re-establish membership for a
		// user who is banned. invites.JoinServer already reads it in-transaction;
		// this is the same shape (rbac review, PR #2840).
		//
		// Membership: capturing first would take the TARGET's sender gate and
		// write topology markers (users / user_presence_settings /
		// presence_settings_pending_operations FOR UPDATE on them) before
		// discovering the add is a no-op. Since AddMember needs no consent from
		// the target, that let an actor repeatedly re-add an existing member and
		// hold a stranger's presence locks, surfacing to them as 503s on their own
		// presence writes (security review, PR #2840).
		var isBanned bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
			serverID, req.UserID,
		).Scan(&isBanned); err != nil {
			return fmt.Errorf("check ban status: %w", err)
		}
		if isBanned {
			return errMemberBanned
		}

		var alreadyMember bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
			serverID, req.UserID,
		).Scan(&alreadyMember); err != nil {
			return fmt.Errorf("check existing membership: %w", err)
		}
		if alreadyMember {
			return errMemberAlreadyPresent
		}

		plan, captureErr := presencehook.Capture(ctx, h.graphPresence, tx, spec)
		if captureErr != nil {
			return fmt.Errorf("capture member add presence: %w", captureErr)
		}

		scanErr := tx.QueryRowContext(ctx, insertQuery, serverID, req.UserID).Scan(&member.JoinedAt)
		if errors.Is(scanErr, sql.ErrNoRows) {
			// Nothing was written, so drop the plan WITHOUT disconnecting anyone:
			// the rollback also discards the topology markers, which is what keeps
			// a no-op add from suppressing a Custom Status snapshot for the whole
			// grace window.
			return errMemberAlreadyPresent
		}
		if scanErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("insert server member: %w", scanErr)
		}

		// Assign all default roles (including @all) to the new member.
		if _, roleErr := tx.ExecContext(ctx, `
			INSERT INTO member_roles (server_id, user_id, role_id)
			SELECT $1, $2, id FROM roles
			WHERE server_id = $1 AND is_default = TRUE
			ON CONFLICT DO NOTHING
		`, serverID, req.UserID); roleErr != nil {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("assign default roles: %w", roleErr)
		}

		return presencehook.Complete(ctx, h.graphPresence, tx, plan)
	})
	if errors.Is(err, errMemberBanned) {
		c.JSON(http.StatusForbidden, gin.H{"error": "User is banned from this server"})
		return
	}
	if errors.Is(err, errMemberAlreadyPresent) {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a member"})
		return
	}
	if err != nil {
		failure := presencehook.Classify(err)
		if retryAfter, ok := failure.RetryAfterHeader(); ok {
			c.Header(presencehook.HeaderRetryAfter, retryAfter)
		}
		h.log.Error("Failed to add member", "failure_class", failure.Code, "error", err)
		c.JSON(failure.Status, gin.H{"error": failure.Body(errMsgFailedAddMember)})
		return
	}

	// Post-commit and deliberately fail-OPEN: hydration has no minuend, so a
	// missed hydrate shows the joiner LESS than they are entitled to and
	// self-corrects. Returning 5xx on a committed membership row would instead
	// drive duplicate-add retries.
	h.hydrateJoinerPresence(ctx, req.UserID)

	h.log.Info("Member added", "server_id", serverID, "new_member", req.UserID, "added_by", userID)

	c.JSON(http.StatusCreated, gin.H{"member": member})
}

// errMemberAlreadyPresent is the in-transaction signal for a no-op add. It never
// reaches the client as an error string; AddMember maps it to 409.
// classifyMutationOutcome splits a hooked-mutation error into the two outcomes
// the handlers must treat differently, so neither has to re-derive the
// distinction inline.
//
// Returns (nil, true) when the caller should stop: the mutation did NOT commit
// and the response has been written. Returns (failure, false) for a DURABLE
// outcome whose delivery failed — the caller continues through its
// de-authorization sequence and reports the failure afterwards.
func (h *Handler) classifyMutationOutcome(
	c *gin.Context, err error, logMessage, userMessage string, logArgs ...any,
) (*presencehook.Failure, bool) {
	failure := presencehook.Classify(err)
	h.log.Error(logMessage, append([]any{"failure_class", failure.Code, "error", err}, logArgs...)...)

	if errors.Is(err, presencecapture.ErrPostCommitDelivery) {
		return &failure, false
	}
	if retryAfter, ok := failure.RetryAfterHeader(); ok {
		c.Header(presencehook.HeaderRetryAfter, retryAfter)
	}
	c.JSON(failure.Status, gin.H{"error": failure.Body(userMessage)})
	return nil, true
}

// respondDurableDeliveryFailure reports a mutation that COMMITTED but whose
// presence delivery did not settle. It is called only after the caller has run
// its full de-authorization sequence — returning before that is what left
// removed members holding a live RBAC cache entry (rbac review, PR #2840).
//
// The purge outcome rides along when there is one: the purge already happened,
// and dropping it loses a moderation result on exactly the path where the caller
// most needs to know it ran.
func (h *Handler) respondDurableDeliveryFailure(
	c *gin.Context, failure *presencehook.Failure, message string, resp gin.H,
) {
	if retryAfter, ok := failure.RetryAfterHeader(); ok {
		c.Header(presencehook.HeaderRetryAfter, retryAfter)
	}
	body := gin.H{"error": failure.Body(message)}
	if purge, ok := resp["purge"]; ok {
		body["purge"] = purge
	}
	c.JSON(failure.Status, body)
}

var errMemberAlreadyPresent = errors.New("members: user is already a member")

// errMemberBanned is the in-transaction signal for a banned target. Like
// errMemberAlreadyPresent it never reaches the client as a string; AddMember
// maps it to 403.
var errMemberBanned = errors.New("members: user is banned from this server")

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

	targetExists, err := h.checkMembership(c.Request.Context(), serverID, targetUserID)
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

	// A timeout bars the member from voice (AuthorizeJoin rejects an active
	// timed_out_until), so a member already sitting in a voice channel must be
	// evicted now — otherwise their media-plane session survives until they
	// leave. Fire-and-forget; degrades to the pre-push behavior without an
	// enforcer (CV-CAN-007 review P1).
	h.disconnectVoiceUser(serverID, targetUserID)

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

// checkMembership reports whether userID is a member of serverID.
//
// It takes a context because it also serves as the pre-transaction MEMBERSHIP
// PROBE for #2854 stage C. Without one it ignores client disconnect and blocks
// unboundedly on pool acquisition under saturation — the same unbounded wait
// the probe contract's no-locking-clause rule exists to keep off the pool.
//
// It carries NO locking clause, and must not gain one: on the pooled connection
// no SET LOCAL lock_timeout applies.
func (h *Handler) checkMembership(ctx context.Context, serverID, userID string) (bool, error) {
	var exists bool
	err := h.db.QueryRowContext(ctx,
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

// execRemovalTx removes the member inside a HOOKED transaction. It no longer
// begins or commits: presencehook.WithGatedTx acquires the process-local sender
// gates BEFORE opening the transaction — the durable topology rail requires that
// order, because acquiring them after BeginTx creates a gate-vs-row-lock cycle
// against users/presence_settings.go — and Complete owns the commit.
//
// Covers kick AND self-leave: there is no separate Leave handler. RemoveMember
// branches on isSelfRemoval, and a user leaves by naming themselves on
// DELETE /servers/:id/members/:user_id.
//
// The capture strictly precedes DELETE FROM server_members, which is the write
// that destroys the audience being captured. Everything after this function —
// including BroadcastToServerAndPrune's deliver-then-prune ordering — is
// untouched: the presence work has already completed by then.
func (h *Handler) execRemovalTx(ctx context.Context, serverID, targetUserID string) error {
	spec := presencehook.Spec{
		Family:      presencecapture.FamilyMemberRemove,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: targetUserID,
	}

	return presencehook.WithGatedTx(ctx, h.graphPresence, h.db, h.log, spec, func(tx *sql.Tx) error {
		plan, captureErr := presencehook.Capture(ctx, h.graphPresence, tx, spec)
		if captureErr != nil {
			return fmt.Errorf("capture member removal presence: %w", captureErr)
		}

		queries := []string{
			// FIRST, and deliberately: this is the audience-destroying write.
			`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`,
			`DELETE FROM channel_keys WHERE user_id = $2 AND channel_id IN (SELECT id FROM channels WHERE server_id = $1)`,
			`DELETE FROM pending_key_requests WHERE user_id = $2 AND channel_id IN (SELECT id FROM channels WHERE server_id = $1)`,
			`DELETE FROM channel_read_states WHERE user_id = $2 AND channel_id IN (SELECT id FROM channels WHERE server_id = $1)`,
		}
		for _, query := range queries {
			if _, execErr := tx.ExecContext(ctx, query, serverID, targetUserID); execErr != nil {
				presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
				return fmt.Errorf("remove member rows: %w", execErr)
			}
		}

		return presencehook.Complete(ctx, h.graphPresence, tx, plan)
	})
}

func (h *Handler) invalidateMemberPermissions(serverID, userID, action string) {
	ctx, cancel := context.WithTimeout(context.Background(), permissionCacheInvalidationTimeout)
	defer cancel()
	if err := h.resolver.InvalidateUser(ctx, serverID, userID); err != nil {
		h.log.Error("Failed to invalidate member permissions", "error", err, "server_id", serverID, "user_id", userID, "action", action)
	}
}

// RemoveMemberRequest is the optional body for the kick (DELETE member) endpoint (#1353).
// An empty body binds to the zero value, so existing bodyless callers are unaffected.
type RemoveMemberRequest struct {
	PurgeMessages bool `json:"purge_messages"`
}

// bindOptionalBody binds an OPTIONAL JSON request body. An empty body is fine (fields stay at
// their zero value); a MALFORMED non-empty body is rejected with 400 (#1353 review, Codex P1).
// Discarding the bind error would let a truncated body like `{"purge_messages":true,` set the
// flag before ShouldBindJSON errors and trigger an irreversible purge from an invalid request.
// Returns true to proceed; on false the caller must return (the 400 is already written).
func bindOptionalBody(c *gin.Context, req any) bool {
	if err := c.ShouldBindJSON(req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return false
	}
	return true
}

// RemoveMember removes a member from a server (kick or leave)
func (h *Handler) RemoveMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")
	purgeCtx, purgeCancel := context.WithTimeout(c.Request.Context(), purgeOnModerationTimeout)
	defer purgeCancel()

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	var req RemoveMemberRequest
	if !bindOptionalBody(c, &req) { // #1353 optional body; empty OK, malformed rejected
		return
	}

	requesterExists, err := h.checkMembership(c.Request.Context(), serverID, userID)
	if err != nil || !requesterExists {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	}
	targetExists, err := h.checkMembership(c.Request.Context(), serverID, targetUserID)
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

	// deliveryFailure is non-nil only for a DURABLE outcome whose presence
	// delivery failed. The de-authorization sequence still runs; the 503 is
	// written after it.
	var deliveryFailure *presencehook.Failure
	if err := h.execRemovalTx(c.Request.Context(), serverID, targetUserID); err != nil {
		// Classify distinguishes a PRE-commit failure (500, nothing written) from
		// a POST-commit delivery failure (503, the member IS removed and a retry
		// is safe). The old blanket 500 told a client nothing had happened when
		// the removal had in fact committed.
		// A PRE-commit failure means nothing landed and classifyMutationOutcome
		// has already responded. A POST-COMMIT delivery failure has NOT: the
		// membership row is already gone, and returning early would skip every
		// de-authorization step below — leaving the removed member with a live
		// RBAC cache entry, an intact WS subscription, un-revoked channel keys and
		// a media-plane session. That is stale authorization on a user who has
		// actually been removed.
		var handled bool
		deliveryFailure, handled = h.classifyMutationOutcome(
			c, err, "Failed to remove member", errMsgFailedRemoveMember)
		if handled {
			return
		}
	}

	action := "removed"
	if auth.isSelfRemoval {
		action = "left"
	}

	serverUUID, _ := uuid.Parse(serverID)
	memberRemoved := websocket.OutgoingMessage{
		Type: "member_removed",
		Data: map[string]interface{}{
			"server_id": serverID,
			"user_id":   targetUserID,
		},
	}

	// CV-CAN-027: deliver member_removed and then evict the removed member from the
	// server-level WS subscription set in the SAME serialized hub operation. This
	// orders the eviction AFTER the member_removed delivery (so they still receive
	// their own removal event) and BEFORE the key-revocation fanout below (a later
	// broadcast on the same channel), so they no longer receive key_revocation or any
	// later server broadcast. Membership is already deleted at this point.
	if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
		h.hub.BroadcastToServerAndPrune(serverUUID, memberRemoved, targetUUID)
	} else {
		h.hub.BroadcastToServer(serverUUID, memberRemoved)
	}

	// Rotate before the cache scan so a slow invalidation cannot extend the old-key window.
	h.triggerKeyRevocationsForServer(serverID, targetUserID, userID)

	// CV-CAN-007 (review P1): membership is now deleted — recheck the removed
	// member's voice presence so the media plane evicts them (the fresh resolve
	// returns ErrNotMember -> voice.enforce.disconnect) instead of letting them
	// publish on the stale join-time snapshot until they voluntarily leave.
	h.recheckVoiceUser(serverID, targetUserID)
	h.invalidateMemberPermissions(serverID, targetUserID, "removed")
	h.log.Info("Member "+action, "server_id", serverID, "target_user", targetUserID, "by_user", userID)

	resp := gin.H{"message": "Member " + action + " successfully"}
	// #1353: additive purge applies ONLY to a moderator removal, never a self-leave
	// (no moderation intent — a user leaving must not bulk-wipe their own history).
	if req.PurgeMessages && !auth.isSelfRemoval {
		resp["purge"] = h.applyPurgeOnModeration(purgeCtx, serverID, userID, targetUserID, "kick")
	}

	// The member is removed and fully de-authorized by this point; only presence
	// delivery failed. Reported AFTER the purge, not before: returning first
	// silently skipped a requested moderation purge, and the action was
	// unrecoverable because a retry 404s on checkMembership. The purge result
	// rides along so the caller still learns what happened (code review, PR #2840).
	if deliveryFailure != nil {
		h.respondDurableDeliveryFailure(c, deliveryFailure, errMsgFailedRemoveMember, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// BanRequest represents a request to ban a member
type BanRequest struct {
	Reason        string `json:"reason"`
	PurgeMessages bool   `json:"purge_messages"` // #1353 additive purge-on-ban (optional; default false)
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

// execBanTx bans the member inside a HOOKED transaction, on the same contract as
// execRemovalTx: WithGatedTx gates before BeginTx, Complete owns the commit.
//
// Ban is the sharpest case this slice exists for. A banned user who keeps
// receiving the server's live voice activity for the remaining TTL is both a
// privacy failure and a moderation-bypass signal, which is why the posture is
// fail-closed: a capture read that fails refuses the ban rather than completing
// one it cannot reconcile.
//
// Capture precedes DELETE FROM server_members specifically. execBanTx's explicit
// member_roles delete is already covered by the composite FK cascade (000035),
// so it is retained for explicitness but is NOT the audience-destroying write.
//
// probedMember is a pooled, NON-AUTHORITATIVE pre-transaction read of whether
// the target is a member (#2854 stage C). Banning a NON-member is a supported
// feature -- a pre-emptive ban -- and changes no shared-server audience, so
// there is nothing to reconcile and no reason to hold that stranger's stripe.
// The ban is recorded either way.
//
// ONE value -- gated -- feeds WithGatedTx, Capture, Abandon and Complete.
// Feeding it to WithGatedTx alone would leave Capture running UNGATED, taking
// FOR UPDATE row locks with no stripe held: the lock-order invariant violated
// in the one direction it forbids, and invisible to every behavioural test
// because the response and the durable state come out byte-identical.
func (h *Handler) execBanTx(
	ctx context.Context, serverID, targetUserID, actorID string, reason *string, probedMember bool,
) error {
	// `gated` and `skipGateForProbe` are SEPARATE on purpose, and conflating them
	// was a real defect (CodeRabbit, PR #2883). A nil capture means TWO different
	// things: "this replica has no capture wired at all" — the documented unwired
	// fallback, which requireGraphPresenceCaptureWired does NOT rule out because
	// it early-returns when activityService is nil — and "the probe said this ban
	// reconciles nothing, so skip the gate".
	//
	// Only the second may raise ErrProbeStale. Testing `gated == nil` for
	// staleness made every ban of a real member 503 on an unwired replica, which
	// is a fail-closed refusal of a moderation action that should simply proceed.
	gated := h.graphPresence
	skipGateForProbe := false
	if gated != nil && !probedMember {
		gated = nil
		skipGateForProbe = true
	}

	spec := presencehook.Spec{
		Family:      presencecapture.FamilyMemberBan,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: targetUserID,
	}

	return presencehook.WithGatedTx(ctx, gated, h.db, h.log, spec, func(tx *sql.Tx) error {
		// Capture ONLY when the target is actually a member.
		//
		// This read stays INSIDE the transaction and remains THE AUTHORITY
		// (#2854 stage C). A pooled probe now precedes it and decides only
		// whether a gate is taken; its verdict is never substituted for this
		// read. When the two disagree the request fails CLOSED below rather
		// than writing a revocation with no capture.
		//
		// Banning a non-member is permitted (a pre-emptive ban) and changes no
		// shared-server audience, so there is nothing to reconcile. The capture
		// would not know that: FamilyMemberBan is a revoking family and this
		// family carries no counterpart, so graphpresence's accepted-edge gate
		// (`policy.CanRevokeVisibility && subject.Counterpart != uuid.Nil`) is
		// structurally unreachable here and the capture seeds plan.viewers with
		// the named principal unconditionally. On the SUCCESS path that is a full
		// websocket teardown of every device belonging to whoever was named.
		//
		// Any user can create a server and thereby hold PermBan on it, so leaving
		// that ungated lets an attacker force-disconnect an arbitrary stranger
		// with no relationship to the server — the exact abuse the accepted-edge
		// gate's own comment describes, re-opened through a family that has no
		// counterpart to gate on (security review, PR #2840).
		//
		// A nil plan is well-defined: Complete's foreign-plan guard is
		// `!ok && plan != nil`, so nil falls through to the bare commit.
		var isMember bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
			serverID, targetUserID,
		).Scan(&isMember); err != nil {
			return fmt.Errorf("check ban target membership: %w", err)
		}

		// C4 fail-closed. The probe said this target had no audience, so no gate
		// was taken and no capture exists -- but the authoritative read
		// disagrees. Proceeding would DELETE this member's rows with NO
		// reconciliation, leaving every viewer holding their Custom Status,
		// which carries no TTL (CWE-284). The deferred rollback discards the
		// transaction; the ban is idempotent, so a retry bans correctly and
		// WITH a capture.
		if isMember && skipGateForProbe {
			return presencehook.ErrProbeStale
		}

		var plan presencecapture.Plan
		if isMember {
			var captureErr error
			plan, captureErr = presencehook.Capture(ctx, gated, tx, spec)
			if captureErr != nil {
				return fmt.Errorf("capture member ban presence: %w", captureErr)
			}
		}

		abandon := func(err error, msg string) error {
			presencehook.Abandon(gated, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("%s: %w", msg, err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO server_bans (server_id, user_id, banned_by, reason)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (server_id, user_id) DO UPDATE SET
				banned_by = EXCLUDED.banned_by,
				reason = EXCLUDED.reason,
				created_at = NOW()
		`, serverID, targetUserID, actorID, reason); err != nil {
			return abandon(err, "record server ban")
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, targetUserID); err != nil {
			return abandon(err, "delete server member")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2`, serverID, targetUserID); err != nil {
			return abandon(err, "delete member roles")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM channel_keys WHERE user_id = $1 AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)`, targetUserID, serverID); err != nil {
			return abandon(err, "delete channel keys")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_key_requests WHERE user_id = $1 AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)`, targetUserID, serverID); err != nil {
			return abandon(err, "delete pending key requests")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM channel_read_states WHERE user_id = $1 AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)`, targetUserID, serverID); err != nil {
			return abandon(err, "delete channel read states")
		}

		return presencehook.Complete(ctx, gated, tx, plan)
	})
}

// BanMember bans a member from a server (removes + prevents rejoin)
func (h *Handler) BanMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")
	purgeCtx, purgeCancel := context.WithTimeout(c.Request.Context(), purgeOnModerationTimeout)
	defer purgeCancel()

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
	if !bindOptionalBody(c, &req) { // #1353 optional body; empty OK, malformed rejected
		return
	}

	var reason *string
	if req.Reason != "" {
		reason = &req.Reason
	}

	// banDeliveryFailure is non-nil only for a DURABLE ban whose presence delivery
	// failed. As in RemoveMember, the de-authorization sequence still runs.
	var banDeliveryFailure *presencehook.Failure
	// Probe-then-gate (#2854 stage C). A probe ERROR yields true, which is the
	// fail-OPEN direction here: it takes the gate and captures, i.e. today's
	// behaviour.
	probedMember, probeErr := h.checkMembership(c.Request.Context(), serverID, targetUserID)
	if probeErr != nil {
		probedMember = true
	}

	if err := h.execBanTx(c.Request.Context(), serverID, targetUserID, userID, reason, probedMember); err != nil {
		// Same split as RemoveMember: a post-commit delivery failure means the
		// member IS banned, so it must not be reported as a 500 that implies
		// nothing happened — and must not skip the de-authorization below. A
		// banned user left holding a live RBAC cache entry, an intact WS
		// subscription and un-revoked channel keys is the moderation bypass this
		// whole slice exists to close.
		var handled bool
		banDeliveryFailure, handled = h.classifyMutationOutcome(
			c, err, "Failed to ban member", errMsgFailedBanMember,
			"server_id", serverID, "user_id", targetUserID)
		if handled {
			return
		}
	}
	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "member_banned", "member", &targetUserID, //nolint:errcheck
			map[string]interface{}{"reason": req.Reason})
	}

	serverUUID, _ := uuid.Parse(serverID)
	memberRemoved := websocket.OutgoingMessage{
		Type: "member_removed",
		Data: map[string]interface{}{
			"server_id": serverID,
			"user_id":   targetUserID,
			"reason":    "banned",
		},
	}

	// CV-CAN-028: deliver member_removed and then evict the banned member from the
	// server-level WS subscription set in the SAME serialized hub operation, ordering
	// the eviction AFTER the member_removed delivery and BEFORE the key-revocation
	// fanout below, so a banned user no longer receives that server's WebSocket
	// messages. Membership is already deleted by execBanTx above.
	if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
		h.hub.BroadcastToServerAndPrune(serverUUID, memberRemoved, targetUUID)
	} else {
		h.hub.BroadcastToServer(serverUUID, memberRemoved)
	}

	// Rotate before the cache scan so a slow invalidation cannot extend the old-key window.
	h.triggerKeyRevocationsForServer(serverID, targetUserID, userID)

	// CV-CAN-007 (review P1): membership is now deleted — recheck the banned
	// member's voice presence so the media plane evicts them (the fresh resolve
	// returns ErrNotMember -> voice.enforce.disconnect) instead of letting them
	// publish on the stale join-time snapshot until they voluntarily leave.
	h.recheckVoiceUser(serverID, targetUserID)
	h.invalidateMemberPermissions(serverID, targetUserID, "banned")

	resp := gin.H{"message": "Member banned"}
	// #1353: additive purge — runs AFTER the ban has committed; a denied/failed purge
	// never affects the ban (best-effort, surfaced via the purge object only).
	if req.PurgeMessages {
		resp["purge"] = h.applyPurgeOnModeration(purgeCtx, serverID, userID, targetUserID, "ban")
	}

	// Banned and fully de-authorized by this point; only presence delivery
	// failed. Report that rather than a 200 the caller would read as settled.
	if banDeliveryFailure != nil {
		h.respondDurableDeliveryFailure(c, banDeliveryFailure, errMsgFailedBanMember, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
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
		`SELECT c.id
		 FROM channels c
		 WHERE c.server_id = $1`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query E2EE channels for key revocation", "error", err, "server_id", serverID)
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			h.log.Error("Failed to scan channel for key revocation", "error", err)
			continue
		}

		// Per-channel rotation with the member-removal payload shape.
		h.rotator.RevokeChannelKeyEpoch(channelID, "member_removal", actorID, removedUserID)
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
