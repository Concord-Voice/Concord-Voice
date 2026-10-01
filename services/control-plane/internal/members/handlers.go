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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
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
	errMsgInvalidServerID             = "Invalid server ID"
	errMsgInvalidUserID               = "Invalid user ID"
	errMsgInvalidRequestBody          = "Invalid request body"
	errMsgInsufficientPerms           = "insufficient permissions"
	errMsgFailedFetchMembers          = "Failed to fetch members"
	errMsgMissingMemberPublicKey      = "Every server member needs a public key for secure channel creation"
	errMsgFailedAddMember             = "Failed to add member"
	errMsgFailedUpdateMember          = "Failed to update member"
	errMsgFailedRemoveMember          = "Failed to remove member"
	errMsgFailedBanMember             = "Failed to ban member"
	errMsgFailedTimeoutMember         = "Failed to timeout member"
	errMsgFailedGetServerOwner        = "Failed to get server owner"
	errMsgFailedCheckPerms            = "Failed to check permissions"
	errMsgUserNotMember               = "User is not a member of this server"
	errMsgNotMember                   = "Not a member of this server"
	errMsgFailedCheckTargetMembership = "Failed to check target membership"
	errMsgFailedCheckHierarchy        = "Failed to check role hierarchy"

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
	// authority is the shared RBAC channel-fence coordinator. It is injected
	// from router construction so membership removal uses the same epoch rail
	// and lock domain as category and role authority mutations.
	authority memberAuthorityCoordinator
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

type memberAuthorityCoordinator interface {
	LockServerAuthorityChannelsTx(context.Context, *sql.Tx, string) ([]string, error)
	FenceKnownLossTx(context.Context, *sql.Tx, string, string, string, []string) ([]keyrotation.Rotation, map[string][]string, error)
	CompleteChannelAuthorityMutationWithRotations(context.Context, string, []string, rbac.PresenceRecheckPlan, []keyrotation.Rotation, map[string][]string)
	FailClosedChannelAuthorityMutation(context.Context, string, []string)
}

// SetAuthorityHandler injects the one RBAC authority coordinator constructed
// by the router; it does not create another key delivery or rotation rail.
func (h *Handler) SetAuthorityHandler(authority *rbac.Handler) { h.authority = authority }

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
	// A failure here must not degrade to role-less members: `roles: []` reads
	// as "has no roles", and the client builds assign/unassign from it.
	if err := h.populateRBAcRoles(serverID, members); err != nil {
		h.log.Error("Failed to fetch member roles", "server_id", serverID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMembers})
		return
	}
	h.ensureRolesNotNil(members)
	h.maskOwnerRole(userID, members)

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
			return nil, err // skipping would drop a member from a 200
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
// On error the caller must discard members: the map may be partially applied.
func (h *Handler) populateRBAcRoles(serverID string, members []MemberWithUser) error {
	roleRows, err := h.db.Query(`
		SELECT mr.user_id, r.id, r.name, r.color, r.emoji, r.position, COALESCE(r.display_separately, FALSE)
		FROM member_roles mr
		INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = mr.server_id
		WHERE mr.server_id = $1
		ORDER BY r.position DESC
	`, serverID)
	if err != nil {
		return fmt.Errorf("query member roles: %w", err)
	}
	defer func() { _ = roleRows.Close() }()

	memberRoleMap := make(map[string][]MemberRoleInfo)
	for roleRows.Next() {
		var uid, roleID, roleName string
		var roleColor, roleEmoji *string
		var position int
		var displaySeparately bool
		if err := roleRows.Scan(&uid, &roleID, &roleName, &roleColor, &roleEmoji, &position, &displaySeparately); err != nil {
			return fmt.Errorf("scan member role: %w", err)
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
	return roleRows.Err()
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
func (h *Handler) maskOwnerRole(viewerUserID string, members []MemberWithUser) {
	for i := range members {
		if members[i].Role != "owner" || members[i].UserID == viewerUserID {
			continue
		}
		if len(members[i].Roles) > 0 {
			members[i].Role = members[i].Roles[0].RoleName // highest position (sorted DESC)
		} else {
			members[i].Role = "member"
		}
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
		return h.addMemberTx(ctx, tx, spec, &member)
	})
	if err != nil {
		h.respondAddMemberError(c, err)
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

// classifyModerationTxError maps the authoritative transaction-time denial
// sentinels before handling an ordinary pre- or post-commit mutation failure.
func (h *Handler) classifyModerationTxError(
	c *gin.Context, err error, ownerMessage, hierarchyMessage, logMessage, userMessage string, logArgs ...any,
) (*presencehook.Failure, bool) {
	switch {
	case errors.Is(err, errRemoveCurrentOwner), errors.Is(err, errBanCurrentOwner):
		c.JSON(http.StatusForbidden, gin.H{"error": ownerMessage})
	case errors.Is(err, errModerationTargetGone):
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
	case errors.Is(err, errModerationPermissionDenied):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
	case errors.Is(err, errModerationHierarchyDenied):
		c.JSON(http.StatusForbidden, gin.H{"error": hierarchyMessage})
	default:
		return h.classifyMutationOutcome(c, err, logMessage, userMessage, logArgs...)
	}
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

// addMemberInsertQuery inserts the added member. ON CONFLICT DO NOTHING makes a
// concurrent add a no-op that returns no row.
const addMemberInsertQuery = `
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', NOW())
		ON CONFLICT (server_id, user_id) DO NOTHING
		RETURNING joined_at
	`

// addMemberTx is AddMember's transaction, run inside WithGatedTx's sender
// gates. It sets member.JoinedAt when the member is added.
func (h *Handler) addMemberTx(ctx context.Context, tx *sql.Tx, spec presencehook.Spec, member *models.ServerMember) error {
	serverID, targetUserID := member.ServerID, member.UserID

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
	if err := rbac.LockAuthorityPrincipalsTx(ctx, tx, []string{targetUserID}); err != nil {
		return fmt.Errorf("lock added member: %w", err)
	}
	var isBanned bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
		serverID, targetUserID,
	).Scan(&isBanned); err != nil {
		return fmt.Errorf("check ban status: %w", err)
	}
	if isBanned {
		return errMemberBanned
	}

	var alreadyMember bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, targetUserID,
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

	scanErr := tx.QueryRowContext(ctx, addMemberInsertQuery, serverID, targetUserID).Scan(&member.JoinedAt)
	if errors.Is(scanErr, sql.ErrNoRows) {
		// Nothing was written, so drop the plan WITHOUT disconnecting anyone:
		// the rollback also discards the topology markers, which is what keeps
		// a no-op add from suppressing a Custom Status snapshot for the whole
		// grace window.
		return errMemberAlreadyPresent
	}
	if scanErr != nil {
		presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
		if rbac.IsServerFKViolation(scanErr, serverMembersServerFKConstraint) {
			// Nothing here holds the servers row, so the server can be
			// deleted after the permission check; see errServerGone.
			return errServerGone
		}
		return fmt.Errorf("insert server member: %w", scanErr)
	}

	// Assign all default roles (including @all) to the new member.
	if _, roleErr := tx.ExecContext(ctx, `
		INSERT INTO member_roles (server_id, user_id, role_id)
		SELECT $1, $2, id FROM roles
		WHERE server_id = $1 AND is_default = TRUE
		ON CONFLICT DO NOTHING
	`, serverID, targetUserID); roleErr != nil {
		presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
		return fmt.Errorf("assign default roles: %w", roleErr)
	}

	return presencehook.Complete(ctx, h.graphPresence, tx, plan)
}

// respondAddMemberError writes AddMember's response for a transaction that did
// not commit.
func (h *Handler) respondAddMemberError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errMemberBanned):
		c.JSON(http.StatusForbidden, gin.H{"error": "User is banned from this server"})
	case errors.Is(err, errMemberAlreadyPresent):
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a member"})
	case errors.Is(err, errServerGone):
		// AddMember's permission check is this route's membership check.
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
	default:
		failure := presencehook.Classify(err)
		if retryAfter, ok := failure.RetryAfterHeader(); ok {
			c.Header(presencehook.HeaderRetryAfter, retryAfter)
		}
		h.log.Error("Failed to add member", "failure_class", failure.Code, "error", err)
		c.JSON(failure.Status, gin.H{"error": failure.Body(errMsgFailedAddMember)})
	}
}

// errMemberAlreadyPresent is the in-transaction signal for a no-op add. It never
// reaches the client as an error string; AddMember maps it to 409.
var errMemberAlreadyPresent = errors.New("members: user is already a member")

// errMemberBanned is the in-transaction signal for a banned target. Like
// errMemberAlreadyPresent it never reaches the client as a string; AddMember
// maps it to 403.
var errMemberBanned = errors.New("members: user is banned from this server")

// These authoritative in-transaction denials keep the pooled preflight from
// granting a moderation action after its mutable facts have changed.
var (
	errRemoveCurrentOwner         = errors.New("members: cannot remove the current server owner")
	errBanCurrentOwner            = errors.New("members: cannot ban the current server owner")
	errModerationTargetGone       = errors.New("members: moderation target is no longer a member")
	errModerationPermissionDenied = errors.New("members: moderator no longer has permission")
	errModerationHierarchyDenied  = errors.New("members: moderator no longer outranks target")
)

// authorizeModerationTx revalidates the mutable moderator facts inside the
// destructive transaction. The cached preflight is only an optimization.
func (h *Handler) authorizeModerationTx(
	ctx context.Context, tx *sql.Tx, serverID, actorID, targetUserID string, permission rbac.Permission,
) error {
	perms, err := h.resolver.ResolveServerPermissionsTx(ctx, tx, serverID, actorID)
	if errors.Is(err, rbac.ErrNotMember) {
		return errModerationPermissionDenied
	}
	if err != nil {
		return fmt.Errorf("resolve current moderator permissions: %w", err)
	}
	if !perms.Has(permission) {
		return errModerationPermissionDenied
	}
	if err := h.resolver.CheckHierarchyTx(ctx, tx, serverID, actorID, targetUserID); err != nil {
		if errors.Is(err, rbac.ErrHierarchyViolation) {
			return errModerationHierarchyDenied
		}
		return fmt.Errorf("check current moderation hierarchy: %w", err)
	}
	return nil
}

// lockModerationUsersTx acquires every users row a moderation write can reach
// through its direct FKs after the server visibility serializer. ORDER BY keeps
// the pair deterministic when two moderators cross-target each other.
func lockModerationUsersTx(ctx context.Context, tx *sql.Tx, actorID, targetUserID string) (returnErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM users
		WHERE id IN ($1, $2)
		ORDER BY id
		FOR NO KEY UPDATE
	`, actorID, targetUserID)
	if err != nil {
		return fmt.Errorf("lock moderation users: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close moderation users: %w", closeErr))
		}
	}()
	locked := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan locked moderation user: %w", err)
		}
		locked++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked moderation users: %w", err)
	}
	expected := 2
	if actorID == targetUserID {
		expected = 1
	}
	if locked != expected {
		return fmt.Errorf("moderation user no longer exists: %w", errModerationTargetGone)
	}
	return nil
}

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
	serverID, targetUserID, idsOK := moderationTarget(c)
	if !idsOK {
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
		status, msg := h.refusalUnlessServerGone(c.Request.Context(), serverID, updateVanished, http.StatusForbidden, errMsgInsufficientPerms, nil)
		c.JSON(status, gin.H{"error": msg})
		return
	}

	// Verify target is a member. An EXISTS always returns a row, so a missing
	// target is false; an error is a fault, never "not a member".
	targetExists, err := h.checkMembership(c.Request.Context(), serverID, targetUserID)
	if err != nil {
		h.log.Error(errMsgFailedCheckTargetMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMember})
		return
	}
	if !targetExists {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}

	// Cannot change the owner's legacy role
	ownerID, err := h.getServerOwnerID(c.Request.Context(), serverID)
	if errors.Is(err, errServerGone) {
		// Deleted since the target check above: the target is no longer a member.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}
	if err != nil {
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
	if errors.Is(err, sql.ErrNoRows) {
		// The row is gone since the owner read: the server was deleted (its
		// cascade removes the row) or the target left. Either way the target is
		// no longer a member. Any other error is a fault.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}
	if err != nil {
		h.log.Error("Failed to update member role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMember})
		return
	}
	if h.audit != nil {
		if err := h.audit.Log(c.Request.Context(), serverID, &userID, "member_updated", "member", &targetUserID, nil); err != nil {
			h.log.Warn("Member update audit write failed", "error", err)
		}
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
		status, msg := h.refusalUnlessServerGone(c.Request.Context(), serverID, timeoutVanished, http.StatusForbidden, errMsgInsufficientPerms, nil)
		return status, msg, false
	}
	if targetUserID == userID {
		return http.StatusBadRequest, "Cannot timeout yourself", false
	}

	targetExists, err := h.checkMembership(c.Request.Context(), serverID, targetUserID)
	if err != nil {
		h.log.Error(errMsgFailedCheckTargetMembership, "error", err)
		return http.StatusInternalServerError, errMsgFailedTimeoutMember, false
	}
	if !targetExists {
		return http.StatusNotFound, errMsgUserNotMember, false
	}

	ownerID, err := h.getServerOwnerID(c.Request.Context(), serverID)
	if errors.Is(err, errServerGone) {
		// Deleted since the target check above: the target is no longer a member.
		return http.StatusNotFound, errMsgUserNotMember, false
	}
	if err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		return http.StatusInternalServerError, errMsgFailedTimeoutMember, false
	}
	if targetUserID == ownerID {
		return http.StatusForbidden, "Cannot timeout the server owner", false
	}
	if err := h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, targetUserID); err != nil {
		status, msg := h.hierarchyRefusal(c.Request.Context(), serverID, timeoutVanished, "Cannot timeout a member with equal or higher role position", err)
		return status, msg, false
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
	serverID, targetUserID, idsOK := moderationTarget(c)
	if !idsOK {
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
	err := h.db.QueryRowContext(c.Request.Context(),
		"UPDATE server_members SET timed_out_until = $1 WHERE server_id = $2 AND user_id = $3 RETURNING timed_out_until",
		timedOutUntil, serverID, targetUserID,
	).Scan(&storedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		// Gone since the owner read, as in UpdateMember: not a member any more.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}
	if err != nil {
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
	serverID, targetUserID, idsOK := moderationTarget(c)
	if !idsOK {
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

// errServerGone reports a servers row found absent after the request was
// admitted: getServerOwnerID's answer for sql.ErrNoRows, and AddMember's for an
// INSERT that fails server_members.server_id's foreign key. Every caller has
// already seen the server — the route admitted the request and each caller
// checked membership or permission first — so an absent row means the server
// was deleted since, by DeleteServer or by the owner's erasure through
// servers.owner_id's ON DELETE CASCADE. Each caller answers it the way it
// answers membership that vanished one check earlier, never with a 500.
//
// ONLY those two signals become errServerGone. Any other error is returned
// unchanged and stays a fault: an outage must never read as a denial.
var errServerGone = errors.New("members: server no longer exists")

// serverMembersServerFKConstraint is server_members.server_id's foreign key.
const serverMembersServerFKConstraint = "server_members_server_id_fkey"

func (h *Handler) getServerOwnerID(ctx context.Context, serverID string) (string, error) {
	var ownerID string
	err := h.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errServerGone
	}
	return ownerID, err
}

// vanishedAnswer is a moderation route's answer for a server deleted
// mid-request, and the body of its 500.
type vanishedAnswer struct {
	status   int
	msg      string
	faultMsg string
}

var (
	removalVanished = vanishedAnswer{http.StatusNotFound, errMsgUserNotMember, errMsgFailedRemoveMember}
	updateVanished  = vanishedAnswer{http.StatusNotFound, errMsgUserNotMember, errMsgFailedUpdateMember}
	timeoutVanished = vanishedAnswer{http.StatusNotFound, errMsgUserNotMember, errMsgFailedTimeoutMember}
	// A ban's vanished answer is the 403 its permission check gives, because a
	// pre-emptive ban's target need not be a member at all.
	banVanished = vanishedAnswer{http.StatusForbidden, errMsgInsufficientPerms, errMsgFailedBanMember}
)

// refusalUnlessServerGone answers a refusal reached after the request was
// admitted: a missing permission, a hierarchy violation, or a hierarchy read
// that found no row (cause). A server deleted since admission reaches all
// three — its membership rows are gone, so a permission lookup that misses the
// cache finds none (where a cached grant would let the request on to the
// route's vanished answer), and the role positions compare as a violation, and
// the hierarchy's own owner read finds no row — so the servers row is read
// again first. Gone, the route's vanished answer wins; a failed re-read is a
// fault; otherwise the refusal stands, and a hierarchy read that found no row
// is a fault, never a denial.
func (h *Handler) refusalUnlessServerGone(ctx context.Context, serverID string, v vanishedAnswer, status int, msg string, cause error) (int, string) {
	_, err := h.getServerOwnerID(ctx, serverID)
	if errors.Is(err, errServerGone) {
		return v.status, v.msg
	}
	if err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		return http.StatusInternalServerError, v.faultMsg
	}
	if cause != nil {
		h.log.Error(errMsgFailedCheckHierarchy, "error", cause)
		return http.StatusInternalServerError, v.faultMsg
	}
	return status, msg
}

// hierarchyRefusal answers a CheckHierarchy error after the owner read: a
// violation is the route's 403 (unless the server has gone), and a missing row
// is a fault unless the server has gone. Only those two can be a deletion the
// check misread, so only they re-read the server; any other error is a fault
// outright, because confirming a deletion that merely coincides with an outage
// would turn the outage into the route's vanished answer.
func (h *Handler) hierarchyRefusal(ctx context.Context, serverID string, v vanishedAnswer, violationMsg string, err error) (int, string) {
	switch {
	case errors.Is(err, rbac.ErrHierarchyViolation):
		return h.refusalUnlessServerGone(ctx, serverID, v, http.StatusForbidden, violationMsg, nil)
	case errors.Is(err, sql.ErrNoRows):
		return h.refusalUnlessServerGone(ctx, serverID, v, http.StatusInternalServerError, v.faultMsg, err)
	default:
		h.log.Error(errMsgFailedCheckHierarchy, "error", err)
		return http.StatusInternalServerError, v.faultMsg
	}
}

type removalAuth struct {
	isSelfRemoval bool
}

func removalCurrentOwnerMessage(isSelfRemoval bool) string {
	if isSelfRemoval {
		return "Server owner cannot leave. Delete the server or transfer ownership first."
	}
	return "Cannot remove the server owner"
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
		status, msg := h.refusalUnlessServerGone(c.Request.Context(), serverID, removalVanished, http.StatusForbidden, errMsgInsufficientPerms, nil)
		return nil, status, msg
	}
	if targetUserID == ownerID {
		return nil, http.StatusForbidden, "Cannot remove the server owner"
	}
	if err := h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, targetUserID); err != nil {
		status, msg := h.hierarchyRefusal(c.Request.Context(), serverID, removalVanished, "Cannot remove a member with equal or higher role position", err)
		return nil, status, msg
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
type memberAuthorityFence struct {
	channelIDs      []string
	rotations       []keyrotation.Rotation
	deniedByChannel map[string][]string
}

func (h *Handler) execRemovalFenceTx(ctx context.Context, serverID, targetUserID, actorID, credentialEpoch string) (memberAuthorityFence, error) {
	var fence memberAuthorityFence
	spec := presencehook.Spec{
		Family:      presencecapture.FamilyMemberRemove,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: targetUserID,
	}

	err := presencehook.WithGatedTx(ctx, h.graphPresence, h.db, h.log, spec, func(tx *sql.Tx) error {
		if err := h.prepareRemovalFenceTx(ctx, tx, serverID, targetUserID, actorID, credentialEpoch, &fence); err != nil {
			return err
		}
		plan, captureErr := presencehook.Capture(ctx, h.graphPresence, tx, spec)
		if captureErr != nil {
			return fmt.Errorf("capture member removal presence: %w", captureErr)
		}
		abandon := func(err error, msg string) error {
			presencehook.Abandon(h.graphPresence, plan, presencecapture.CauseWriteFailed)
			return fmt.Errorf("%s: %w", msg, err)
		}

		queries := []string{
			// FIRST, and deliberately: this is the audience-destroying write.
			`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`,
			`DELETE FROM channel_read_states WHERE user_id = $2 AND channel_id IN (SELECT id FROM channels WHERE server_id = $1)`,
		}
		for _, query := range queries {
			if _, execErr := tx.ExecContext(ctx, query, serverID, targetUserID); execErr != nil {
				return abandon(execErr, "remove member rows")
			}
		}
		var fenceErr error
		fence.rotations, fence.deniedByChannel, fenceErr = h.authority.FenceKnownLossTx(ctx, tx, serverID, actorID, targetUserID, fence.channelIDs)
		if fenceErr != nil {
			return abandon(fenceErr, "fence removed member channel authority")
		}

		return presencehook.Complete(ctx, h.graphPresence, tx, plan)
	})
	return fence, err
}

// prepareRemovalFenceTx takes the server visibility serializer before users.
// Reversing that order lets concurrent authority mutations form a cycle while
// one waits for visibility and the other waits for a moderation user row.
func (h *Handler) prepareRemovalFenceTx(
	ctx context.Context, tx *sql.Tx, serverID, targetUserID, actorID, credentialEpoch string, fence *memberAuthorityFence,
) error {
	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return fmt.Errorf("lock member removal server: %w", err)
	}
	if err := lockModerationUsersTx(ctx, tx, actorID, targetUserID); err != nil {
		return err
	}
	// Take the sorted stronger pair before GuardTx's actor FOR SHARE read so
	// concurrent cross-target removals cannot form a lock-upgrade cycle.
	if err := credepoch.GuardTx(ctx, tx, actorID, credentialEpoch); err != nil {
		return err
	}

	// Revalidate mutable server facts after all authoritative locks are held.
	// None of them holds the servers row, so a server deleted since the
	// preflight arrives here with no row: the target is no longer a member,
	// which is this transaction's own answer for a vanished membership. Only
	// sql.ErrNoRows is reclassified; any other error stays a fault.
	var ownerID string
	err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return errModerationTargetGone
	}
	if err != nil {
		return fmt.Errorf("query current server owner for member removal: %w", err)
	}
	if ownerID == targetUserID {
		return errRemoveCurrentOwner
	}
	if actorID != targetUserID && actorID != ownerID {
		if err := h.authorizeModerationTx(ctx, tx, serverID, actorID, targetUserID, rbac.PermKick); err != nil {
			return err
		}
	}
	if h.authority == nil {
		return errors.New("member authority coordinator unavailable")
	}
	channelIDs, err := h.authority.LockServerAuthorityChannelsTx(ctx, tx, serverID)
	if err != nil {
		return fmt.Errorf("lock member removal authority channels: %w", err)
	}
	fence.channelIDs = channelIDs
	var targetStillMember bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`, serverID, targetUserID,
	).Scan(&targetStillMember); err != nil {
		return fmt.Errorf("revalidate removed member: %w", err)
	}
	if !targetStillMember {
		return errModerationTargetGone
	}
	return nil
}

// execRemovalTx preserves the package-private test seam while the HTTP path
// consumes the confirmed fence result for post-commit delivery.
func (h *Handler) execRemovalTx(ctx context.Context, serverID, targetUserID, actorID string) error {
	_, err := h.execRemovalFenceTx(ctx, serverID, targetUserID, actorID, "")
	return err
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

func credentialFenceRejected(c *gin.Context, err error) bool {
	if !errors.Is(err, credepoch.ErrEpochMismatch) && !errors.Is(err, credepoch.ErrBlocked) {
		return false
	}
	c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
	return true
}

func (h *Handler) classifyRemovalFenceError(
	c *gin.Context, err error, fence memberAuthorityFence, serverID string, selfRemoval bool,
) (*presencehook.Failure, bool) {
	if credentialFenceRejected(c, err) {
		return nil, true
	}
	if errors.Is(err, rbac.ErrChannelAuthorityChannelLimit) {
		c.JSON(http.StatusConflict, gin.H{"error": "Channel key cleanup exceeds 500 recipients; resolve access changes in batches of 500 or fewer"})
		return nil, true
	}
	if !errors.Is(err, presencecapture.ErrPostCommitDelivery) && !isAmbiguousMemberCommit(err) && len(fence.channelIDs) > 0 {
		h.authority.FailClosedChannelAuthorityMutation(c.Request.Context(), serverID, fence.channelIDs)
	}
	return h.classifyModerationTxError(
		c, err, removalCurrentOwnerMessage(selfRemoval),
		"Cannot remove a member with equal or higher role position", "Failed to remove member", errMsgFailedRemoveMember,
	)
}

func isAmbiguousMemberCommit(err error) bool {
	return rbac.IsAmbiguousAuthorityCommit(err) || errors.Is(err, presencecapture.ErrCommitUnresolved)
}

// reconcileAmbiguousMemberDeauthorization closes the externally visible
// authority after an acknowledgement-lost removal or ban. A concurrent re-add
// cannot prove the original deletion rolled back, so recovery always
// deauthorizes. Rotations are deliberately not published without a proven
// commit.
func (h *Handler) reconcileAmbiguousMemberDeauthorization(serverID, targetUserID string, fence memberAuthorityFence, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), permissionCacheInvalidationTimeout)
	defer cancel()
	if h.authority != nil && len(fence.channelIDs) > 0 {
		h.authority.FailClosedChannelAuthorityMutation(ctx, serverID, fence.channelIDs)
	}
	if h.hub != nil {
		serverUUID, serverErr := uuid.Parse(serverID)
		targetUUID, targetErr := uuid.Parse(targetUserID)
		if serverErr == nil && targetErr == nil {
			h.hub.PruneServerSubscriber(serverUUID, targetUUID)
		}
	}
	h.recheckVoiceUser(serverID, targetUserID)
	if h.resolver != nil {
		h.invalidateMemberPermissions(serverID, targetUserID, "ambiguous_"+reason)
	}
}

// moderationTarget validates the member-moderation path ids and returns both
// in canonical form. PostgreSQL treats alternate UUID spellings as the same
// row, while guards and Redis permission-cache keys compare strings (#3362).
func moderationTarget(c *gin.Context) (serverID, targetUserID string, ok bool) {
	parsedServerID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return "", "", false
	}
	parsedTargetUserID, err := uuid.Parse(c.Param("user_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return "", "", false
	}
	return parsedServerID.String(), parsedTargetUserID.String(), true
}

// requireRemovalParties writes RemoveMember's refusal and returns false unless
// both the requester and the target are members of the server. A membership
// read that fails is a fault, never "not a member": only a false EXISTS is a
// missing member.
func (h *Handler) requireRemovalParties(c *gin.Context, serverID, userID, targetUserID string) bool {
	requesterExists, err := h.checkMembership(c.Request.Context(), serverID, userID)
	if err != nil {
		h.log.Error("Failed to check requester membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveMember})
		return false
	}
	if !requesterExists {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return false
	}
	targetExists, err := h.checkMembership(c.Request.Context(), serverID, targetUserID)
	if err != nil {
		h.log.Error(errMsgFailedCheckTargetMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveMember})
		return false
	}
	if !targetExists {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return false
	}
	return true
}

// RemoveMember removes a member from a server (kick or leave)
func (h *Handler) RemoveMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID, targetUserID, idsOK := moderationTarget(c)
	if !idsOK {
		return
	}
	purgeCtx, purgeCancel := context.WithTimeout(c.Request.Context(), purgeOnModerationTimeout)
	defer purgeCancel()

	// moderationTarget parsed and canonicalized both IDs above.
	serverUUID := uuid.MustParse(serverID)
	targetUUID := uuid.MustParse(targetUserID)

	var req RemoveMemberRequest
	if !bindOptionalBody(c, &req) { // #1353 optional body; empty OK, malformed rejected
		return
	}

	if !h.requireRemovalParties(c, serverID, userID, targetUserID) {
		return
	}

	ownerID, err := h.getServerOwnerID(c.Request.Context(), serverID)
	if errors.Is(err, errServerGone) {
		// Deleted since the target check above: the target is no longer a member.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotMember})
		return
	}
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
	fence, removalErr := h.execRemovalFenceTx(c.Request.Context(), serverID, targetUserID, userID, middleware.TokenCredentialEpoch(c))
	if removalErr != nil {
		// presencehook wraps commit acknowledgement failures, so they do not
		// retain rbac's ambiguity sentinel. Any non-delivery error after this
		// exact set was locked receives idempotent fail-closed recovery; only a
		// classified post-commit delivery failure may publish rotations below.
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
		if isAmbiguousMemberCommit(removalErr) {
			h.reconcileAmbiguousMemberDeauthorization(serverID, targetUserID, fence, "removed")
		}
		var handled bool
		deliveryFailure, handled = h.classifyRemovalFenceError(c, removalErr, fence, serverID, auth.isSelfRemoval)
		if handled {
			return
		}
	}
	action := "removed"
	auditAction := "member_removed"
	if auth.isSelfRemoval {
		action = "left"
		auditAction = "member_left"
	}
	if h.audit != nil {
		if err := h.audit.Log(c.Request.Context(), serverID, &userID, auditAction, "member", &targetUserID, nil); err != nil {
			h.log.Warn("Member removal audit write failed", "error", err)
		}
	}

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
	h.hub.BroadcastToServerAndPrune(serverUUID, memberRemoved, targetUUID)
	if len(fence.rotations) > 0 {
		h.authority.CompleteChannelAuthorityMutationWithRotations(c.Request.Context(), serverID, fence.channelIDs, nil, fence.rotations, fence.deniedByChannel)
	}

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

func (h *Handler) classifyBanFenceError(
	c *gin.Context, err error, fence memberAuthorityFence, serverID, targetUserID string,
) (*presencehook.Failure, bool) {
	if credentialFenceRejected(c, err) {
		return nil, true
	}
	if errors.Is(err, rbac.ErrChannelAuthorityChannelLimit) {
		c.JSON(http.StatusConflict, gin.H{"error": "Channel key cleanup exceeds 500 recipients; resolve access changes in batches of 500 or fewer"})
		return nil, true
	}
	if !errors.Is(err, presencecapture.ErrPostCommitDelivery) && !isAmbiguousMemberCommit(err) && len(fence.channelIDs) > 0 {
		h.authority.FailClosedChannelAuthorityMutation(c.Request.Context(), serverID, fence.channelIDs)
	}
	return h.classifyModerationTxError(
		c, err, "Cannot ban the server owner",
		"Cannot ban a member with equal or higher role position", "Failed to ban member", errMsgFailedBanMember,
		"server_id", serverID, "user_id", targetUserID,
	)
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

type banFenceRequest struct {
	serverID        string
	targetUserID    string
	actorID         string
	credentialEpoch string
	reason          *string
	probedMember    bool
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
func (h *Handler) execBanFenceTx(
	ctx context.Context, serverID, targetUserID, actorID, credentialEpoch string, reason *string, probedMember bool,
) (memberAuthorityFence, error) {
	req := banFenceRequest{
		serverID:        serverID,
		targetUserID:    targetUserID,
		actorID:         actorID,
		credentialEpoch: credentialEpoch,
		reason:          reason,
		probedMember:    probedMember,
	}
	var fence memberAuthorityFence
	// Capture and gate decisions are separate: an unwired replica still skips
	// capture for a known non-member, while only a wired skipped gate can prove a
	// later member result was a stale probe.
	//
	// Only the second may raise ErrProbeStale. Testing `gated == nil` for
	// staleness made every ban of a real member 503 on an unwired replica, which
	// is a fail-closed refusal of a moderation action that should simply proceed.
	gated, skipCaptureForProbe, skipGateForProbe := h.banPresenceGate(req.probedMember)

	spec := presencehook.Spec{
		Family:      presencecapture.FamilyMemberBan,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: req.targetUserID,
	}

	err := presencehook.WithGatedTx(ctx, gated, h.db, h.log, spec, func(tx *sql.Tx) error {
		isMember, err := h.prepareBanFenceTx(ctx, tx, req, skipGateForProbe, &fence)
		if err != nil {
			return err
		}
		plan, err := h.captureBanPresenceTx(ctx, tx, gated, spec, skipCaptureForProbe || !isMember)
		if err != nil {
			return err
		}
		if err := h.writeBanTx(ctx, tx, gated, plan, req, isMember, &fence); err != nil {
			return err
		}
		return presencehook.Complete(ctx, gated, tx, plan)
	})
	return fence, err
}

func (h *Handler) banPresenceGate(probedMember bool) (presencecapture.GraphPresenceCapture, bool, bool) {
	skipCaptureForProbe := !probedMember
	gated := h.graphPresence
	if gated != nil && skipCaptureForProbe {
		return nil, true, true
	}
	return gated, skipCaptureForProbe, false
}

func (h *Handler) prepareBanFenceTx(
	ctx context.Context, tx *sql.Tx, req banFenceRequest, skipGateForProbe bool, fence *memberAuthorityFence,
) (bool, error) {
	ownerID, err := lockBanActorAndServerTx(ctx, tx, req)
	if err != nil {
		return false, err
	}
	isMember, err := banTargetMembershipTx(ctx, tx, req.serverID, req.targetUserID, "check")
	if err != nil {
		return false, err
	}
	if err := validateBanProbe(isMember, skipGateForProbe); err != nil {
		return false, err
	}
	if req.actorID != ownerID {
		if err := h.authorizeModerationTx(ctx, tx, req.serverID, req.actorID, req.targetUserID, rbac.PermBan); err != nil {
			return false, err
		}
	}
	return h.lockBanAuthorityTx(ctx, tx, req, isMember, fence)
}

func lockBanActorAndServerTx(ctx context.Context, tx *sql.Tx, req banFenceRequest) (string, error) {
	// The visibility serializer is the first shared lock for every moderation
	// write. Once it is held, match DeleteServer's users-before-server order so
	// a live-voice server delete cannot form a users <-> servers lock cycle.
	if err := rbac.LockServerVisibilityCapture(ctx, tx, req.serverID); err != nil {
		return "", fmt.Errorf("lock member ban server: %w", err)
	}
	if err := lockModerationUsersTx(ctx, tx, req.actorID, req.targetUserID); err != nil {
		return "", banUsersLockError(ctx, tx, req.serverID, err)
	}
	// The stronger, sorted user-pair lock prevents GuardTx's actor FOR SHARE
	// read from creating a lock-upgrade cycle before the target is locked.
	if err := credepoch.GuardTx(ctx, tx, req.actorID, req.credentialEpoch); err != nil {
		return "", err
	}
	return lockBanServerOwnerTx(ctx, tx, req.serverID, req.targetUserID)
}

func validateBanProbe(isMember, skipGateForProbe bool) error {
	// A non-member ban is a supported pre-emptive ban and has no audience to
	// reconcile. The pooled probe only selects that no-capture path; the locked
	// membership read remains authoritative. A target that left after a member
	// probe is still safe to ban pre-emptively: capture is skipped because there
	// is no audience to revoke, while refusing the durable ban lets them rejoin.
	if isMember && skipGateForProbe {
		return presencehook.ErrProbeStale
	}
	return nil
}

func (h *Handler) lockBanAuthorityTx(
	ctx context.Context, tx *sql.Tx, req banFenceRequest, isMember bool, fence *memberAuthorityFence,
) (bool, error) {
	if !isMember {
		return false, nil
	}
	if h.authority == nil {
		return false, errors.New("member authority coordinator unavailable")
	}
	channelIDs, err := h.authority.LockServerAuthorityChannelsTx(ctx, tx, req.serverID)
	if err != nil {
		return false, fmt.Errorf("lock member ban authority channels: %w", err)
	}
	fence.channelIDs = channelIDs
	isMember, err = banTargetMembershipTx(ctx, tx, req.serverID, req.targetUserID, "revalidate")
	if err != nil {
		return false, err
	}
	if !isMember {
		return false, errModerationTargetGone
	}
	return true, nil
}

// banUsersLockError keeps the ban's vanished-server answer when the users lock
// comes up short. An owner's erasure deletes their users row and, through
// servers.owner_id's ON DELETE CASCADE, the server with it, so an owner banning
// mid-erasure finds their own row gone here, before lockBanServerOwnerTx can
// see the server is. The lock reports errModerationTargetGone, the removal
// routes' vanished-server answer (404); the ban's is
// errModerationPermissionDenied (403), since a pre-emptive ban's target need
// not be a member. A short lock on a server that still exists stays the
// ordinary target-gone 404.
func banUsersLockError(ctx context.Context, tx *sql.Tx, serverID string, err error) error {
	if !errors.Is(err, errModerationTargetGone) {
		return err
	}
	var exists bool
	if probeErr := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM servers WHERE id = $1)`, serverID,
	).Scan(&exists); probeErr != nil {
		return fmt.Errorf("check member ban server after users lock: %w", probeErr)
	}
	if !exists {
		return errModerationPermissionDenied
	}
	return err
}

func lockBanServerOwnerTx(ctx context.Context, tx *sql.Tx, serverID, targetUserID string) (string, error) {
	// Nothing locked before this holds the servers row, so a server deleted
	// since the preflight arrives here with no row. The moderator is no longer
	// a member of it: errModerationPermissionDenied, the ban preflight's own
	// answer (a pre-emptive ban's target need not be a member, so the target's
	// membership says nothing). Only sql.ErrNoRows is reclassified.
	var ownerID string
	err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errModerationPermissionDenied
	}
	if err != nil {
		return "", fmt.Errorf("query current server owner for member ban: %w", err)
	}
	if ownerID == targetUserID {
		return "", errBanCurrentOwner
	}
	return ownerID, nil
}

func banTargetMembershipTx(ctx context.Context, tx *sql.Tx, serverID, targetUserID, operation string) (bool, error) {
	var isMember bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`, serverID, targetUserID,
	).Scan(&isMember); err != nil {
		return false, fmt.Errorf("%s ban target membership: %w", operation, err)
	}
	return isMember, nil
}

func (h *Handler) captureBanPresenceTx(
	ctx context.Context, tx *sql.Tx, gated presencecapture.GraphPresenceCapture, spec presencehook.Spec, skipCapture bool,
) (presencecapture.Plan, error) {
	if skipCapture {
		return nil, nil
	}
	plan, err := presencehook.Capture(ctx, gated, tx, spec)
	if err != nil {
		return nil, fmt.Errorf("capture member ban presence: %w", err)
	}
	return plan, nil
}

func (h *Handler) writeBanTx(
	ctx context.Context, tx *sql.Tx, gated presencecapture.GraphPresenceCapture, plan presencecapture.Plan,
	req banFenceRequest, isMember bool, fence *memberAuthorityFence,
) error {
	abandon := func(err error, message string) error {
		presencehook.Abandon(gated, plan, presencecapture.CauseWriteFailed)
		return fmt.Errorf("%s: %w", message, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO server_bans (server_id, user_id, banned_by, reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (server_id, user_id) DO UPDATE SET
			banned_by = EXCLUDED.banned_by,
			reason = EXCLUDED.reason,
			created_at = NOW()
	`, req.serverID, req.targetUserID, req.actorID, req.reason); err != nil {
		return abandon(err, "record server ban")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, req.serverID, req.targetUserID); err != nil {
		return abandon(err, "delete server member")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_read_states WHERE user_id = $1 AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)`, req.targetUserID, req.serverID); err != nil {
		return abandon(err, "delete channel read states")
	}
	if !isMember {
		return nil
	}
	rotations, deniedByChannel, err := h.authority.FenceKnownLossTx(ctx, tx, req.serverID, req.actorID, req.targetUserID, fence.channelIDs)
	if err != nil {
		return abandon(err, "fence banned member channel authority")
	}
	fence.rotations = rotations
	fence.deniedByChannel = deniedByChannel
	return nil
}

// execBanTx preserves the package-private test seam while the HTTP path uses
// the confirmed fence result for post-commit key delivery.
func (h *Handler) execBanTx(
	ctx context.Context, serverID, targetUserID, actorID string, reason *string, probedMember bool,
) error {
	_, err := h.execBanFenceTx(ctx, serverID, targetUserID, actorID, "", reason, probedMember)
	return err
}

// authorizeBan writes BanMember's refusal and returns false unless the actor
// holds PermBan and the target is neither the owner, nor the actor, nor at or
// above the actor in the role hierarchy.
func (h *Handler) authorizeBan(c *gin.Context, serverID, userID, targetUserID string) bool {
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermBan)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBanMember})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return false
	}

	ownerID, err := h.getServerOwnerID(c.Request.Context(), serverID)
	if errors.Is(err, errServerGone) {
		// Deleted since the permission check above, which is the only one a ban
		// makes: a pre-emptive ban's target need not be a member at all.
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return false
	}
	if err != nil {
		h.log.Error(errMsgFailedGetServerOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBanMember})
		return false
	}
	if targetUserID == ownerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot ban the server owner"})
		return false
	}
	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot ban yourself"})
		return false
	}

	if err := h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, targetUserID); err != nil {
		status, msg := h.hierarchyRefusal(c.Request.Context(), serverID, banVanished, "Cannot ban a member with equal or higher role position", err)
		c.JSON(status, gin.H{"error": msg})
		return false
	}
	return true
}

// BanMember bans a member from a server (removes + prevents rejoin)
func (h *Handler) BanMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID, targetUserID, idsOK := moderationTarget(c)
	if !idsOK {
		return
	}
	purgeCtx, purgeCancel := context.WithTimeout(c.Request.Context(), purgeOnModerationTimeout)
	defer purgeCancel()

	if !h.authorizeBan(c, serverID, userID, targetUserID) {
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

	fence, banErr := h.execBanFenceTx(c.Request.Context(), serverID, targetUserID, userID, middleware.TokenCredentialEpoch(c), reason, probedMember)
	if banErr != nil {
		// Same split as RemoveMember: a post-commit delivery failure means the
		// member IS banned, so it must not be reported as a 500 that implies
		// nothing happened — and must not skip the de-authorization below. A
		// banned user left holding a live RBAC cache entry, an intact WS
		// subscription and un-revoked channel keys is the moderation bypass this
		// whole slice exists to close.
		if isAmbiguousMemberCommit(banErr) {
			h.reconcileAmbiguousMemberDeauthorization(serverID, targetUserID, fence, "banned")
		}
		var handled bool
		banDeliveryFailure, handled = h.classifyBanFenceError(c, banErr, fence, serverID, targetUserID)
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

	if len(fence.rotations) > 0 {
		h.authority.CompleteChannelAuthorityMutationWithRotations(c.Request.Context(), serverID, fence.channelIDs, nil, fence.rotations, fence.deniedByChannel)
	}

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
	serverID, targetUserID, idsOK := moderationTarget(c)
	if !idsOK {
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
	rows, err := result.RowsAffected()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unban member"})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "User is not banned"})
		return
	}
	if h.audit != nil {
		if err := h.audit.Log(c.Request.Context(), serverID, &userID, "member_unbanned", "member", &targetUserID, nil); err != nil {
			h.log.Warn("Member unban audit write failed", "error", err)
		}
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
