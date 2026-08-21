package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
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
	errMsgCannotReorderManaged    = "Cannot reorder managed roles"
	errMsgDefaultRoleMustBeLowest = "The default role must remain the lowest role"
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
	errMsgNoLegalRolePosition     = "No available role position below your own"
	errMsgInvalidColorFormat      = "Invalid color format (expected #RRGGBB)"
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
	// voiceEnforcer pushes recomputed permissions to voice-connected members
	// after a mutation (CV-CAN-007 review P1). Wired via SetVoiceEnforcer;
	// nil (dev/test default) means no push — join-snapshot behavior.
	voiceEnforcer VoiceEnforcer
	// presenceRecheck reconciles active Server Voice Rich Presence with the
	// pre-mutation authorized audience captured inside the authority
	// transaction (#2445). Wired via SetPresenceRecheck; nil (dev/test default)
	// means no capture and no clear.
	presenceRecheck PresenceRecheck
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

// ReorderRolesRequest represents a request to reorder roles (changes position values).
//
// `dive,uuid` validates every ELEMENT, not just the slice, and closes CodeQL
// go/log-injection (alert 1262) at the boundary rather than at the sink. Without
// it a non-UUID entry reached PostgreSQL as `roles.id = ANY($3)`, raised
// `invalid input syntax for type uuid: "<attacker string>"`, and that error —
// carrying the attacker's string and any newlines in it — was written to the
// log by the handler's default error branch. The same value also turned what
// should be a 400 into a 500. One boundary check removes all three symptoms.
//
// `max` and `unique` are load-bearing for the WRITE, not merely hygiene, which
// is why they live here rather than waiting for #2841. `applyRolePositions`
// holds a SERVER-WIDE advisory lock until COMMIT, so an unbounded list let one
// request monopolise every role mutation on the server; and the batched UPDATE
// verifies `RowsAffected == len(RoleIDs)`, a comparison that is only exact when
// no id appears twice. #2841 still owns the wider input policy.
type ReorderRolesRequest struct {
	RoleIDs []string `json:"role_ids" binding:"required,min=1,max=500,unique,dive,uuid"`
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

// validateHexColor checks an optional role colour is #RRGGBB.
//
// It validates the hex DIGITS, not just the leading '#' and the length: the
// previous check accepted "#ZZZZZZ". The value is rendered into a React style
// prop, and while CSSOM drops an invalid colour rather than executing it, a
// validator that admits arbitrary characters is not one worth having.
//
// Returns false once a response has been written; the caller must return.
func validateHexColor(c *gin.Context, color *string) bool {
	if color == nil || len(*color) == 0 {
		return true
	}
	v := *color
	if len(v) != 7 || v[0] != '#' {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidColorFormat})
		return false
	}
	for i := 1; i < len(v); i++ {
		d := v[i]
		isHex := (d >= '0' && d <= '9') || (d >= 'a' && d <= 'f') || (d >= 'A' && d <= 'F')
		if !isHex {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidColorFormat})
			return false
		}
	}
	return true
}

// resolveNewRolePosition returns the position a newly created role should take,
// making room for it inside tx when the actor is not the server owner.
//
// Non-owners get a slot strictly below their own highest role. CreateRole
// previously used a server-wide MAX(position)+1, which placed every created role
// above the entire hierarchy including Admin (#2359) — durable authority rather
// than a display artifact, since roles.position is the axis every role and member
// mutation compares against.
//
// It SHIFTS rather than clamps. A clamp to actorMaxPosition-1 always returns that
// same value (the actor's roles are a subset of the server's, so MAX(position)+1
// can never be the smaller operand), so every role an actor created collided on
// one position. Searching for a free slot below the actor does not help either:
// applyRolePositions repacks positions densely as len-i-1 on every reorder, so a
// server that has been reordered once has no gaps at all. Raising everything at or
// above the actor by one is the only approach that yields a distinct position in a
// densely-packed server. Relative order is preserved and no authority changes.
//
// The actorMaxPosition < 2 guard keeps the shift floor at 2 or higher, so the
// default @everyone role at position 0 is never moved.
//
// The owner check is identity on servers.owner_id, matching checkRoleOwnerAndHierarchy
// and AssignRole — never PermAdministrator.
//
// Returns (position, true) on success. On failure it has already written the
// response and returns (0, false); the caller must return immediately.
// Returns the position, and whether other roles were SHIFTED to make room --
// the caller must tell clients when they were, since a shift moves roles the
// role_created broadcast does not name.
func (h *Handler) resolveNewRolePosition(ctx context.Context, tx *sql.Tx, serverID, userID string) (int, bool, error) {
	// One query, one snapshot. Three sequential reads would observe three
	// different snapshots of a hierarchy another writer may be mutating.
	//
	// No FOR SHARE here, unlike roleGuardQuery: this statement names no target
	// role to lock, and the ceiling is an aggregate, which PostgreSQL refuses to
	// lock at all. Snapshot coherence plus the advisory lock the caller already
	// holds is the available mechanism, and it is the same one role_guard.go
	// relies on.
	const snapshotQuery = `
		SELECT
			(SELECT owner_id FROM servers WHERE id = $1),
			(SELECT COALESCE(MAX(position), 0) FROM roles WHERE server_id = $1),
			(SELECT COALESCE(MAX(r.position), 0) FROM member_roles mr
			   INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = mr.server_id
			   WHERE mr.server_id = $1 AND mr.user_id = $2)
	`
	var ownerID string
	var maxPosition, actorMaxPosition int
	if err := tx.QueryRowContext(ctx, snapshotQuery, serverID, userID).
		Scan(&ownerID, &maxPosition, &actorMaxPosition); err != nil {
		return 0, false, fmt.Errorf("create role: resolve position snapshot: %w", err)
	}

	// Owner bypass is identity on servers.owner_id, matching the role guard --
	// never PermAdministrator.
	if ownerID == userID {
		return maxPosition + 1, false, nil
	}

	// No branch here for "the actor holds no roles at all": it is UNREACHABLE.
	// RequirePermission(PermManageRoles) gates this route and permissions derive
	// solely from member_roles, so a member with no rows resolves to zero
	// permissions and is refused before this handler runs -- and an owner has
	// already returned above. Defensive code for an impossible state is worse
	// than none; TestCreateRole_ActorWithNoRoles_RefusedByMiddleware pins which
	// layer actually refuses.

	// Strictly below the actor, and never at the default role's slot.
	//
	// Only actorMaxPosition == 0 has no legal slot: the shift below is
	// `position >= actorMaxPosition`, so shifting at 0 would raise the default
	// role off position 0 -- the one thing that must never happen, since every
	// member holds it and raising it lifts the whole server's ceiling at once.
	//
	// At 1 there IS a legal slot: the shift moves the actor 1 -> 2 and leaves
	// position 0 untouched, freeing slot 1 strictly below them. An earlier
	// revision refused `< 2` here, which looked equivalent and was not: a fresh
	// server holds only the default role at 0, so the owner's FIRST created role
	// lands at 1 -- and granting that role is the ordinary way to delegate Manage
	// Roles. Refusing at 1 therefore locked the first delegated admin out of role
	// creation permanently, through the product's own happy path, with only the
	// owner able to undo it. The test that covered the refusal seeded position 1
	// directly and so never surfaced how a real server reaches it.
	if actorMaxPosition < 1 {
		return 0, false, fmt.Errorf("create role: no legal position below %d: %w", actorMaxPosition, errHierarchyDenied)
	}

	// SHIFT rather than clamp. A clamp to actorMaxPosition-1 always returns that
	// one value -- the actor's roles are a subset of the server's, so
	// MAX(position)+1 is never the smaller operand -- and every role an actor
	// created would collide on it, in an axis with no unique constraint.
	// Searching for a free slot below the actor does not help either, because
	// applyRolePositions repacks densely as len-i-1 on every reorder, so a server
	// reordered even once has no gaps. Raising everything at or above the actor
	// by one is the only approach yielding a distinct position in a dense server.
	// Relative order is preserved and no authority relationship changes.
	//
	// This makes CreateRole a MULTI-ROW roles.position writer, which is only safe
	// because the caller takes LockServerVisibilityCapture as its first statement
	// and applyRolePositions now does the same (#2861) -- the two families are
	// totally ordered and cannot interleave. Before #2856 this shift was an
	// escalation primitive: it let an actor raise their own ceiling between a
	// guard's two unsynchronized reads. roleGuardQuery closed that by making the
	// ceiling structurally unable to be newer than the target.
	if _, err := tx.ExecContext(ctx,
		`UPDATE roles SET position = position + 1, updated_at = NOW() WHERE server_id = $1 AND position >= $2`,
		serverID, actorMaxPosition,
	); err != nil {
		return 0, false, fmt.Errorf("create role: shift role positions: %w", err)
	}

	return actorMaxPosition, true, nil
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

	// The privilege-escalation check moved INSIDE the transaction below (#2721).
	// It previously ran here against h.resolver.GetEffectivePermissions, which is
	// CACHE-FIRST with a 5-minute TTL — while UpdateRole and AssignRole both
	// resolved cache-bypassing. CreateRole writes a roles.permissions bitfield,
	// the most durable authority in the model, so a stale cache entry let a
	// just-demoted actor mint a permanent over-privileged role that the
	// role_created audit entry then recorded as legitimate.

	if !validateHexColor(c, req.Color) {
		return
	}

	roleID := uuid.New().String()

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
	role.Permissions = req.Permissions
	role.Mentionable = req.Mentionable
	role.DisplaySeparately = req.DisplaySeparately

	// Set inside the transaction below; read after it commits.
	shifted := false

	// CreateRole deliberately does NOT use withAuthorityCapture: a memberless new
	// role changes nobody's Rich Presence visibility, so there is no pre-mutation
	// audience to capture. It uses the same lighter seam the category-override
	// paths already use — BeginTx -> LockServerVisibilityCapture -> work -> Commit
	// — so it joins the per-server advisory total order without adding a fifth
	// transaction pattern (#2721).
	err := func() error {
		ctx := c.Request.Context()
		tx, txErr := h.db.BeginTx(ctx, nil)
		if txErr != nil {
			return fmt.Errorf("begin create role transaction: %w", txErr)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		// FIRST statement, and the only advisory key this transaction takes.
		if lockErr := LockServerVisibilityCapture(ctx, tx, serverID); lockErr != nil {
			return fmt.Errorf("lock server for create role: %w", lockErr)
		}
		if toErr := applyGuardLockTimeout(ctx, tx); toErr != nil {
			return toErr
		}

		// Cache-free and in-transaction. There is deliberately NO owner bypass
		// here: an owner's effective set is OwnerPermissions, which EXCLUDES bit
		// 62, so this is what actually keeps PermAdministrator out of a server.
		// Do not add a bypass for symmetry with UpdateRole/AssignRole.
		actorPerms, permErr := h.resolver.ResolveServerPermissionsTx(ctx, tx, serverID, userID)
		if permErr != nil {
			// %w, not %v: errors.Is unwraps, so ErrNotMember stays matchable and
			// mapGuardError still renders its 403 rather than the default 500.
			return fmt.Errorf("create role: resolve actor permissions: %w", permErr)
		}
		// The route decorator's HasPermission is cache-first with a 5-minute TTL,
		// and CreateRole has no hierarchy check to fail closed behind it — so a
		// fully-demoted actor riding a stale entry could still mint a
		// permissions=0 role, complete with a role_created audit entry and
		// broadcast attributed to someone who no longer holds ManageRoles. The
		// other four handlers are covered by the ceiling dropping to 0. This is
		// one comparison on a value already resolved above; it costs no query.
		if actorPerms&PermManageRoles == 0 {
			return errEscalationDenied
		}
		if Permission(req.Permissions)&^actorPerms != 0 {
			return errEscalationDenied
		}

		// Non-owners get a slot strictly below their own highest role, shifting
		// everything at or above them up by one to make room (#2359). The owner
		// is unclamped at MAX(position)+1. This is the replacement #2856 shaped
		// this transaction to hold.
		position, didShift, posErr := h.resolveNewRolePosition(ctx, tx, serverID, userID)
		if posErr != nil {
			return posErr
		}
		role.Position = position
		shifted = didShift

		if insErr := tx.QueryRowContext(ctx,
			query, roleID, serverID, req.Name, req.Color, req.Emoji,
			role.Position, req.Permissions, req.Mentionable, req.DisplaySeparately,
		).Scan(&role.CreatedAt, &role.UpdatedAt); insErr != nil {
			// %w, not %v: errors.As unwraps, so the call site's
			// errors.As(err, &pqErr) still sees the 23505 and renders the 409.
			return fmt.Errorf("create role: insert: %w", insErr)
		}
		return tx.Commit()
	}()

	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "A role with that name already exists in this server"})
			return
		}
		// errHierarchyDenied IS reachable here as of #2359: resolveNewRolePosition
		// returns it when the actor's highest role sits at position 0 or 1, so
		// there is no slot strictly below them and above the default role.
		h.mapGuardError(c, err, errMsgNoLegalRolePosition, errMsgFailedCreateRole)
		return
	}

	h.announceRoleCreated(c, serverID, userID, roleID, req, role, shifted)
	c.JSON(http.StatusCreated, gin.H{"role": role})
}

// announceRoleCreated performs every post-commit side effect of a successful
// create: cache invalidation, the audit entry, the client broadcasts, and the
// log line. Extracted from CreateRole, which had grown past the cognitive
// complexity limit -- and these are one coherent unit, all of them "the write
// committed, now tell everyone", none of them able to fail the request.
func (h *Handler) announceRoleCreated(
	c *gin.Context, serverID, userID, roleID string, req CreateRoleRequest, role Role, shifted bool,
) {
	_ = h.cache.InvalidateServer(c.Request.Context(), serverID)

	if h.audit != nil {
		_ = h.audit.Log(c.Request.Context(), serverID, &userID, "role_created", "role", &roleID,
			map[string]interface{}{"role_name": req.Name, "permissions": req.Permissions})
	}

	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "role_created",
		Data: map[string]interface{}{
			"server_id": serverID,
			"role":      role,
		},
	})

	// A non-owner create SHIFTS the actor's own role and everything above it up
	// by one, so role_created alone leaves every other client rendering from
	// cached positions with a stale order -- the new role and the actor's role
	// both appearing at the same slot until something forces a refetch. Authority
	// is unaffected (every guard re-reads position fresh), so this is display
	// only, but ReorderRoles already emits roles_reordered when it moves roles
	// and CreateRole now moves roles too. Reusing that event rather than adding a
	// type keeps the client contract at one "positions changed, refetch" signal.
	if shifted {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "roles_reordered",
			Data: map[string]interface{}{"server_id": serverID},
		})
	}

	h.log.Info("Role created", "role_id", roleID, "server_id", serverID, "name", req.Name)
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

	// Cheap pre-check only: rejects a missing or managed role without paying
	// PrepareCapture's O(#senders) cost on the common denial. NOT authoritative —
	// the position it returns is deliberately discarded, because the guard inside
	// the write transaction re-reads it under FOR SHARE (#2721).
	if _, err := h.validateRoleModifiable(c, roleID, serverID); err != nil {
		return // Response already written
	}

	guardMode, guardRequested := confersNothing, int64(0)
	if req.Permissions != nil {
		guardMode, guardRequested = confersRequested, *req.Permissions
	}

	// Cheap, non-authoritative hierarchy denial BEFORE the transaction, so a
	// certain 403 never pays PrepareCapture, never takes the advisory lock, and
	// can never observe ErrPresenceCaptureLimited (#2721 red-team F1/F2/F3).
	if preErr := h.preCheckRoleMutation(c.Request.Context(), serverID, userID, roleID, guardMode, guardRequested); preErr != nil {
		h.mapGuardError(c, preErr, "Cannot modify a role at or above your own position", errMsgFailedUpdateRole)
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
	// nil channelIDs = server scope: every voice channel with active senders.
	// nil onlyUserID = the full candidate set (a role edit can change any
	// member's visibility).
	plan, err := h.withAuthorityCapture(c.Request.Context(), serverID, nil, nil,
		func(ctx context.Context, tx *sql.Tx) error {
			if lockErr := applyGuardLockTimeout(ctx, tx); lockErr != nil {
				return lockErr
			}
			// Authoritative guard: same transaction, same snapshot, one row lock.
			// Must stay BELOW capturePresenceVisibility (run by withAuthorityCapture
			// before this closure) — capture is the exact pre-write audience (#2445).
			res, guardErr := h.authorizeRoleMutationTx(
				ctx, tx, serverID, userID, roleID, guardMode, guardRequested,
			)
			if guardErr != nil {
				return guardErr
			}
			if flagErr := rejectRoleFlags(res, "Cannot modify managed roles", ""); flagErr != nil {
				return flagErr
			}
			return tx.QueryRowContext(ctx, query, args...).Scan(
				&role.ID, &role.ServerID, &role.Name, &role.Color, &role.Emoji,
				&role.Position, &role.Permissions, &role.IsDefault, &role.IsManaged,
				&role.Mentionable, &role.DisplaySeparately, &role.CreatedAt, &role.UpdatedAt,
			)
		},
	)
	if h.mapGuardError(c, err,
		"Cannot modify a role at or above your own position", errMsgFailedUpdateRole) {
		return
	}

	_ = h.cache.InvalidateServer(c.Request.Context(), serverID)
	h.recheckVoiceServer(serverID)
	h.presenceExecute(plan)
	h.revalidateServerSubscribers(serverID)
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
	if errors.Is(err, sql.ErrNoRows) {
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
	// Same format check as CreateRole. NOTE the inverted polarity of this
	// method: it returns true when it has WRITTEN a response.
	if !validateHexColor(c, color) {
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

// execRequiringRow runs an authority write and reports sql.ErrNoRows when the
// statement matched no rows, so the caller can tell "not found" apart from a
// transaction failure.
func execRequiringRow(ctx context.Context, tx *sql.Tx, query string, args ...interface{}) error {
	result, execErr := tx.ExecContext(ctx, query, args...)
	if execErr != nil {
		return execErr
	}
	rowsAffected, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// loadDeletableRole loads the role's hierarchy position after rejecting managed
// and default roles, which cannot be deleted. Returns false once a response has
// been written.
func (h *Handler) loadDeletableRole(c *gin.Context, roleID, serverID string) (int, bool) {
	var isManaged, isDefault bool
	var rolePosition int
	err := h.db.QueryRow(
		`SELECT is_managed, is_default, position FROM roles WHERE id = $1 AND server_id = $2`,
		roleID, serverID,
	).Scan(&isManaged, &isDefault, &rolePosition)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return 0, false
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryRole, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteRole})
		return 0, false
	}

	if isManaged {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot delete managed roles"})
		return 0, false
	}
	if isDefault {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot delete default roles"})
		return 0, false
	}
	return rolePosition, true
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

	// Cheap pre-check only: rejects managed/default roles without paying
	// PrepareCapture's cost. NOT authoritative — the position it returns is
	// deliberately discarded; the in-transaction guard re-reads it (#2721).
	if _, ok := h.loadDeletableRole(c, roleID, serverID); !ok {
		return
	}
	if preErr := h.preCheckRoleMutation(c.Request.Context(), serverID, userID, roleID, confersNothing, 0); preErr != nil {
		h.mapGuardError(c, preErr, "Cannot delete a role at or above your own position", errMsgFailedDeleteRole)
		return
	}

	// Delete role (CASCADE will remove member_roles entries)
	// nil channelIDs = server scope; nil onlyUserID = the full candidate set.
	plan, err := h.withAuthorityCapture(c.Request.Context(), serverID, nil, nil,
		func(ctx context.Context, tx *sql.Tx) error {
			if lockErr := applyGuardLockTimeout(ctx, tx); lockErr != nil {
				return lockErr
			}
			// confersNothing: a delete grants no bits.
			res, guardErr := h.authorizeRoleMutationTx(
				ctx, tx, serverID, userID, roleID, confersNothing, 0,
			)
			if guardErr != nil {
				return guardErr
			}
			if flagErr := rejectRoleFlags(res,
				"Cannot delete managed roles", "Cannot delete default roles"); flagErr != nil {
				return flagErr
			}
			return execRequiringRow(ctx, tx,
				`DELETE FROM roles WHERE id = $1 AND server_id = $2`, roleID, serverID)
		},
	)
	if h.mapGuardError(c, err,
		"Cannot delete a role at or above your own position", errMsgFailedDeleteRole) {
		return
	}

	// Invalidate cache
	_ = h.cache.InvalidateServer(c.Request.Context(), serverID)
	h.recheckVoiceServer(serverID)
	h.presenceExecute(plan)
	h.revalidateServerSubscribers(serverID)

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

// reorderVerdict is the outcome of one evaluation of the ReorderRoles guards.
// It carries no *gin.Context and writes no response, which is what lets the
// identical evaluation run both on the pooled connection and inside the write
// transaction.
type reorderVerdict struct {
	denied bool
	reason string
	// positionOffset shifts every assigned position upward so that position 0
	// belongs to the default role and to nothing else. It is 0 when the default
	// role is named (it takes 0 itself) and 1 when it is absent, RESERVING 0.
	//
	// Without it, a reorder that simply omits @all assigns the last named role
	// position 0 while @all also sits at 0 — a tie, so @all is no longer
	// strictly the lowest role, and RowsAffected still matches so the write
	// commits. Verified by probe before this was added.
	positionOffset int
}

// reorderDeniedError carries an in-transaction guard denial back out of
// applyRolePositions so ReorderRoles can map it to 403 rather than a generic 500.
type reorderDeniedError struct{ reason string }

func (e *reorderDeniedError) Error() string { return "reorder denied: " + e.reason }

// reorderGuardQuery reads every operand of the reorder decision in ONE
// statement so they share a snapshot. Three separate autocommit reads is the
// incoherence #2721 found across the sibling handlers: under READ COMMITTED
// each read takes its own snapshot, so `position >= actorMaxPosition` can
// compare operands captured at different points in time.
//
// It carries NO locking clause, and cannot: the actor ceiling is an aggregate
// and PostgreSQL rejects a locking clause at a query level carrying
// aggregation. That constraint is load-bearing at the pre-transaction call
// site, where no SET LOCAL lock_timeout applies and a locking clause could
// therefore block indefinitely — recreating #2721's F1 by another route.
//
// COALESCE on is_managed is deliberate: roles.is_managed is NULLABLE
// (000035_rbac_system.up.sql declares BOOLEAN DEFAULT FALSE with no NOT NULL),
// and the write filter COALESCEs identically so the guard's verdict and the
// write's filter cannot disagree about what NULL means.
const reorderGuardQuery = `
WITH actor AS (
    SELECT COALESCE(MAX(r.position), 0) AS max_position
    FROM member_roles mr
    INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = $1
    WHERE mr.server_id = $1 AND mr.user_id = $2
), named AS (
    SELECT r.id,
           r.position,
           COALESCE(r.is_managed, FALSE) AS is_managed,
           COALESCE(r.is_default, FALSE) AS is_default
    FROM roles r
    WHERE r.server_id = $1 AND r.id = ANY($3)
)
SELECT s.owner_id,
       a.max_position,
       (SELECT COUNT(*) FROM named n WHERE n.position >= a.max_position),
       (SELECT COUNT(*) FROM named n WHERE n.is_managed AND NOT n.is_default),
       (SELECT d.id::text FROM roles d
         WHERE d.server_id = $1 AND COALESCE(d.is_default, FALSE) LIMIT 1)
FROM servers s
CROSS JOIN actor a
WHERE s.id = $1`

// evaluateReorderGuards decides whether an actor may apply the requested role
// order. It replaces checkReorderHierarchy and checkRolePositionViolations,
// which read on h.db in autocommit while the write ran in its own transaction.
//
// The CALLER decides what a denial means. ReorderRoles runs this once on the
// pooled connection as a cheap fast path that FAILS OPEN, and once inside the
// write transaction where it is authoritative.
func evaluateReorderGuards(
	ctx context.Context, q rowQuerier, serverID, userID string, roleIDs []string,
) (reorderVerdict, error) {
	var (
		ownerID          string
		actorMaxPosition int
		violations       int
		managedNamed     int
		serverDefaultID  sql.NullString
	)
	if err := q.QueryRowContext(ctx, reorderGuardQuery, serverID, userID, pq.Array(roleIDs)).
		Scan(&ownerID, &actorMaxPosition, &violations, &managedNamed, &serverDefaultID); err != nil {
		return reorderVerdict{}, err
	}

	// Managed roles are refused for EVERY caller, the server owner included:
	// this is an integrity rule, not a hierarchy one, matching
	// loadDeletableRole's refusal to delete managed roles. Placing it ABOVE the
	// owner bypass is therefore deliberate.
	//
	// Before this, the flag was enforced only by the UPDATE's WHERE clause, so
	// a named managed role passed the guard and was then filtered out of the
	// write while every other role still moved — 200 OK for a partial write
	// that left two roles sharing a position. That is #2721's F4 shape: a flag
	// read by nothing authoritative.
	//
	// The DEFAULT role is excluded from this refusal on purpose. `@all` is
	// created `is_default = TRUE, is_managed = TRUE` on every server
	// (internal/servers/handlers.go), so refusing every managed role would
	// reject any FULL-LIST reorder — which is exactly the payload the role
	// hierarchy UI sends. The default role is governed by the stricter rule
	// below instead.
	if managedNamed > 0 {
		return reorderVerdict{denied: true, reason: errMsgCannotReorderManaged}, nil
	}

	// The default role is ALWAYS the lowest role in the hierarchy and can never
	// be promoted above anything — by anyone, the server owner included.
	//
	// This is not stylistic. Every member of the server holds `@all`, so it is
	// the term that sets the floor of `COALESCE(MAX(r.position), 0)` for the
	// entire membership. Raising it by one reorder would raise EVERY member's
	// hierarchy ceiling simultaneously, handing the whole server authority over
	// every role beneath the new position — a mass privilege change disguised
	// as a drag-and-drop.
	//
	// TWO distinct ways to break the invariant, so there are two defences:
	//
	//  1. NAMING it somewhere other than last. Positions run high-to-low, so
	//     any earlier slot resolves above 0. Refused rather than silently
	//     corrected — a caller asking to move it is asking for something that
	//     must not happen and should be told so.
	//
	//  2. OMITTING it. The last named role would then take position 0 while the
	//     default role also sits at 0 — a tie, and no longer STRICTLY the
	//     lowest. `positionOffset` reserves 0 instead. This is why the query
	//     resolves the server's default role independently of the payload: a
	//     lookup scoped to the named set cannot see a role that was left out.
	positionOffset := 0
	if serverDefaultID.Valid {
		namedDefault := false
		for _, id := range roleIDs {
			if strings.EqualFold(id, serverDefaultID.String) {
				namedDefault = true
				break
			}
		}
		switch {
		case namedDefault && !strings.EqualFold(roleIDs[len(roleIDs)-1], serverDefaultID.String):
			return reorderVerdict{denied: true, reason: errMsgDefaultRoleMustBeLowest}, nil
		case !namedDefault:
			positionOffset = 1
		}
	}

	if ownerID == userID {
		return reorderVerdict{positionOffset: positionOffset}, nil
	}
	if violations > 0 {
		return reorderVerdict{denied: true, reason: "Cannot reorder roles at or above your own position"}, nil
	}
	// Compare the HIGHEST position this request would actually assign, which is
	// offset-dependent — not `len(roleIDs)-1`, which understates it by one
	// whenever position 0 is being reserved.
	if len(roleIDs)-1+positionOffset >= actorMaxPosition {
		return reorderVerdict{denied: true, reason: "Reorder would create roles at or above your position"}, nil
	}
	return reorderVerdict{positionOffset: positionOffset}, nil
}

// ReorderRoles updates position values for roles (for role hierarchy).
//
// Authorization is evaluated TWICE, deliberately. The call below is a cheap
// fast path; the authoritative decision runs inside applyRolePositions'
// transaction, under the per-server advisory lock. See evaluateReorderGuards.
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

	ctx := c.Request.Context()

	// Cheap NON-AUTHORITATIVE fast path on the pooled connection. It exists only
	// so a request that is certainly going to be denied does not have to open a
	// transaction and take the per-server advisory lock to learn that (#2721
	// F1/F2). It FAILS OPEN by construction — an error falls through to the
	// authoritative evaluation inside applyRolePositions — so it can only ever
	// save work, never grant it.
	//
	// Do NOT make this authoritative, do NOT deny on its error, and do NOT add a
	// locking clause to its query.
	if verdict, guardErr := evaluateReorderGuards(ctx, h.db, serverID, userID, req.RoleIDs); guardErr == nil && verdict.denied {
		c.JSON(http.StatusForbidden, gin.H{"error": verdict.reason})
		return
	}

	if err := h.applyRolePositions(ctx, serverID, userID, req.RoleIDs); err != nil {
		var denied *reorderDeniedError
		switch {
		case errors.As(err, &denied):
			c.JSON(http.StatusForbidden, gin.H{"error": denied.reason})
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		default:
			h.log.Error("Failed to apply role positions", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReorderRoles})
		}
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

// applyRolePositions writes the requested order under the per-server advisory
// lock, re-evaluating authorization inside the transaction.
//
// The lock is the transaction's FIRST statement, per authority_tx.go. It does
// two jobs at once. It gives the guard re-evaluation below meaning — nothing
// can move the hierarchy between the decision and the write, which is the
// CWE-367 straddle issue #2851 proves with a PoC. And it stops two concurrent
// reorders from taking row locks in opposite CLIENT-SUPPLIED orders, which was
// a 40P01 deadlock reproducible on main with no attacker and no privilege.
func (h *Handler) applyRolePositions(ctx context.Context, serverID, userID string, roleIDs []string) error {
	// READ COMMITTED is REQUIRED, not incidental, and is pinned rather than
	// inherited from the server default. The lock is the first statement, so
	// under READ COMMITTED the guard SELECT below takes its snapshot AFTER the
	// lock is granted and therefore observes everything the previous holder
	// committed — which is the entire reason the re-evaluation is
	// authoritative. Under REPEATABLE READ the snapshot would be registered at
	// the first statement, i.e. before the lock is granted, and the guard would
	// read the pre-lock world: the CWE-367 straddle would silently reopen with
	// every test in this package still green. `sql.LevelRepeatableRead` is
	// already used elsewhere in this package (resolver.go), so the idiom is
	// live and the drift is plausible.
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin reorder transaction: %w", err)
	}
	defer func() {
		// discard: Rollback is a no-op after a successful Commit, and the
		// failure paths already return an error.
		_ = tx.Rollback()
	}()

	if err := LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return err
	}

	// AUTHORITATIVE. The pre-transaction call in ReorderRoles is a fast path
	// only; this is the decision.
	verdict, err := evaluateReorderGuards(ctx, tx, serverID, userID, roleIDs)
	if err != nil {
		return fmt.Errorf("evaluate reorder guards: %w", err)
	}
	if verdict.denied {
		return &reorderDeniedError{reason: verdict.reason}
	}

	// ONE statement, not one per role. The advisory lock is held until COMMIT,
	// so a per-role loop kept a SERVER-WIDE lock — the same key that serializes
	// every other role-authority write and internal/voice's temporary-SBAC
	// revoke — across N round trips, with N supplied by the client. Collapsing
	// the loop bounds the hold to a single statement regardless of N, which
	// makes that class structurally hard to reintroduce rather than merely
	// bounded by an input check.
	//
	// `WITH ORDINALITY` numbers the array from 1, so `cardinality - ord`
	// reproduces the previous `len(roleIDs) - i - 1` exactly.
	//
	// The managed-role predicate mirrors the guard's: refuse managed roles that
	// are NOT default, so `@all` — created is_default AND is_managed — is
	// written normally while integration-owned roles are excluded. Guard and
	// write derive from the same rule and cannot disagree.
	result, err := tx.ExecContext(ctx,
		`UPDATE roles AS r
		 SET position = u.new_position, updated_at = NOW()
		 FROM (
		     SELECT t.id, (cardinality($1::uuid[]) - t.ord + $3)::int AS new_position
		     FROM unnest($1::uuid[]) WITH ORDINALITY AS t(id, ord)
		 ) AS u
		 WHERE r.id = u.id
		   AND r.server_id = $2
		   AND NOT (COALESCE(r.is_managed, FALSE) AND NOT COALESCE(r.is_default, FALSE))`,
		pq.Array(roleIDs), serverID, verdict.positionOffset,
	)
	if err != nil {
		return fmt.Errorf("apply role positions: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read reorder rows affected: %w", err)
	}
	// Every named role must have matched. A shortfall means an id names no role
	// on this server (or a managed one slipped past the guard), and the whole
	// reorder rolls back rather than applying a partial order the caller never
	// asked for. `unique` on the binding tag guarantees no id is counted twice,
	// so this comparison is exact.
	if affected != int64(len(roleIDs)) {
		return fmt.Errorf("reorder matched %d of %d roles: %w", affected, len(roleIDs), sql.ErrNoRows)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reorder transaction: %w", err)
	}
	return nil
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

	// Cheap, non-authoritative hierarchy denial before the transaction, so a
	// certain 403 never pays PrepareCapture and never takes the advisory lock
	// (#2721 red-team F1/F2/F3).
	if preErr := h.preCheckRoleMutation(c.Request.Context(), serverID, actorID, req.RoleID, confersTargetRole, 0); preErr != nil {
		h.mapGuardError(c, preErr,
			"Cannot assign a role with equal or higher position than your own", errMsgFailedAssignRole)
		return
	}

	// The role's position, its permission bitfield, the actor's ceiling and the
	// server owner are ALL read by the in-transaction guard below, in one
	// statement under one snapshot (#2721). They were previously four separate
	// autocommit reads whose comparison straddled four snapshots.
	//
	// Position bounds WHICH roles the actor may assign; the subset check bounds
	// WHICH BITS they may confer (CWE-269). Both are required — a lower-positioned
	// role can carry a bit the actor does not hold. Neither substitutes for the
	// other; do not remove one as "redundant".

	// Assign role
	// nil channelIDs = server scope; the phase-2 visibility-filter input is
	// bounded to the one affected user, because only that user's permission
	// inputs changed. Candidate SETS are never pruned by mutation shape.
	plan, err := h.withAuthorityCapture(c.Request.Context(), serverID, nil, &targetUserID,
		func(ctx context.Context, tx *sql.Tx) error {
			if lockErr := applyGuardLockTimeout(ctx, tx); lockErr != nil {
				return lockErr
			}
			// confersTargetRole: an assignment confers whatever the role CURRENTLY
			// carries, so the subset check must use the bitfield the guard re-read
			// under FOR SHARE — the same value this INSERT commits against.
			// AssignRole rejects neither flag, matching its pre-#2721 behaviour.
			if _, guardErr := h.authorizeRoleMutationTx(
				ctx, tx, serverID, actorID, req.RoleID, confersTargetRole, 0,
			); guardErr != nil {
				return guardErr
			}
			_, execErr := tx.ExecContext(ctx,
				`INSERT INTO member_roles (server_id, user_id, role_id, assigned_by)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT (server_id, user_id, role_id) DO NOTHING`,
				serverID, targetUserID, req.RoleID, actorID,
			)
			return execErr
		},
	)
	if h.mapGuardError(c, err,
		"Cannot assign a role with equal or higher position than your own",
		errMsgFailedAssignRole) {
		return
	}

	// Invalidate cache
	_ = h.cache.Invalidate(c.Request.Context(), serverID, targetUserID)
	h.recheckVoiceUser(serverID, targetUserID)
	h.presenceExecute(plan)
	h.revalidateServerSubscribers(serverID)

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

// loadUnassignableRole loads the role's hierarchy position after rejecting
// default roles, which cannot be unassigned. Returns false once a response has
// been written.
func (h *Handler) loadUnassignableRole(c *gin.Context, roleID, serverID string) (int, bool) {
	var isDefault bool
	var rolePosition int
	err := h.db.QueryRow(`SELECT is_default, position FROM roles WHERE id = $1 AND server_id = $2`, roleID, serverID).Scan(&isDefault, &rolePosition)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
		return 0, false
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryRole, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnassignRole})
		return 0, false
	}
	if isDefault {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot unassign default roles"})
		return 0, false
	}
	return rolePosition, true
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

	// Cheap pre-check only: rejects default roles without paying PrepareCapture's
	// cost. NOT authoritative — the position it returns is deliberately discarded;
	// the in-transaction guard re-reads it under FOR SHARE (#2721).
	if _, ok := h.loadUnassignableRole(c, roleID, serverID); !ok {
		return
	}
	if preErr := h.preCheckRoleMutation(c.Request.Context(), serverID, actorID, roleID, confersNothing, 0); preErr != nil {
		h.mapGuardError(c, preErr,
			"Cannot unassign a role with equal or higher position than your own", errMsgFailedUnassignRole)
		return
	}

	// Remove role assignment
	// nil channelIDs = server scope; the phase-2 visibility-filter input is
	// bounded to the one affected user.
	plan, err := h.withAuthorityCapture(c.Request.Context(), serverID, nil, &targetUserID,
		func(ctx context.Context, tx *sql.Tx) error {
			if lockErr := applyGuardLockTimeout(ctx, tx); lockErr != nil {
				return lockErr
			}
			// confersNothing: removal grants no bits AT SERVER SCOPE. The channel
			// -scope exception (DENY subtraction makes removal a widening) is #2724
			// and is deliberately not addressed here — see conferredMode's doc.
			res, guardErr := h.authorizeRoleMutationTx(
				ctx, tx, serverID, actorID, roleID, confersNothing, 0,
			)
			if guardErr != nil {
				return guardErr
			}
			if flagErr := rejectRoleFlags(res, "", "Cannot unassign default roles"); flagErr != nil {
				return flagErr
			}
			return execRequiringRow(ctx, tx,
				`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`,
				serverID, targetUserID, roleID,
			)
		},
	)
	// ORDER MATTERS. execRequiringRow returns a bare sql.ErrNoRows for "no such
	// ASSIGNMENT", while a missing ROLE surfaces as errRoleGone, which does NOT
	// wrap sql.ErrNoRows. Checking sql.ErrNoRows first therefore keeps the two
	// 404 bodies distinct; mapGuardError alone would collapse both to "Role not
	// found" and drift the wire contract.
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Role assignment not found"})
		return
	}
	if h.mapGuardError(c, err,
		"Cannot unassign a role with equal or higher position than your own",
		errMsgFailedUnassignRole) {
		return
	}

	// Invalidate cache
	_ = h.cache.Invalidate(c.Request.Context(), serverID, targetUserID)
	h.recheckVoiceUser(serverID, targetUserID)
	h.presenceExecute(plan)
	h.revalidateServerSubscribers(serverID)

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
	if errors.Is(err, sql.ErrNoRows) {
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
	if errors.Is(err, sql.ErrNoRows) {
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
	// Channel scope: only this channel's overrides changed. nil onlyUserID =
	// the full candidate set (an override can change any member's visibility).
	plan, err := h.withAuthorityCapture(c.Request.Context(), serverID, []string{channelID}, nil,
		func(ctx context.Context, tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, query,
				overrideID, channelID, req.TargetType, req.TargetID, req.Allow, req.Deny,
			).Scan(&override.ID, &override.CreatedAt, &override.UpdatedAt, &isInsert)
		},
	)
	if err != nil {
		h.log.Error("Failed to upsert override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSaveOverride})
		return
	}

	// Invalidate cache for affected channel
	_ = h.cache.InvalidateChannel(c.Request.Context(), serverID, channelID)
	h.recheckVoiceChannel(serverID, channelID)
	h.presenceExecute(plan)
	h.revalidateChannelSubscribers(serverID, channelID)

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
	if errors.Is(err, sql.ErrNoRows) {
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
	// Channel scope; nil onlyUserID = the full candidate set.
	plan, err := h.withAuthorityCapture(c.Request.Context(), serverID, []string{channelID}, nil,
		func(ctx context.Context, tx *sql.Tx) error {
			result, execErr := tx.ExecContext(ctx,
				`DELETE FROM channel_permission_overrides WHERE id = $1 AND channel_id = $2`,
				overrideID, channelID,
			)
			if execErr != nil {
				return execErr
			}
			rowsAffected, affectedErr := result.RowsAffected()
			if affectedErr != nil {
				return affectedErr
			}
			if rowsAffected == 0 {
				return sql.ErrNoRows
			}
			return nil
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Override not found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to delete override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

	// Invalidate cache
	_ = h.cache.InvalidateChannel(c.Request.Context(), serverID, channelID)
	h.recheckVoiceChannel(serverID, channelID)
	h.presenceExecute(plan)
	h.revalidateChannelSubscribers(serverID, channelID)

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
	if errors.Is(err, sql.ErrNoRows) {
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

func (h *Handler) revalidateChannelSubscribers(serverID, channelID string) {
	serverUUID, serverErr := uuid.Parse(serverID)
	channelUUID, channelErr := uuid.Parse(channelID)
	if h.hub == nil || serverErr != nil || channelErr != nil {
		return
	}
	h.hub.RevalidateChannelSubscriptions(serverUUID, channelUUID)
}

func (h *Handler) revalidateServerSubscribers(serverID string) {
	serverUUID, err := uuid.Parse(serverID)
	if h.hub == nil || err != nil {
		return
	}
	h.hub.RevalidateServerSubscriptions(serverUUID)
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
	if errors.Is(err, sql.ErrNoRows) {
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
	if errors.Is(err, sql.ErrNoRows) {
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
	if errors.Is(err, sql.ErrNoRows) {
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
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Override not found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

	// Delete the override and its mirrored child rows under one capture.
	// The error body is byte-identical to the query path above — a #1794
	// disclosure invariant, not a style choice.
	if err := h.deleteCategoryOverrideWithCapture(
		c.Request.Context(), serverID, categoryID, overrideID, targetType, targetID,
	); err != nil {
		h.log.Error("Failed to delete category override", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteOverride})
		return
	}

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
	if errors.Is(err, sql.ErrNoRows) {
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

	// NOT hooked for #2445 presence capture: this write is visibility-INERT.
	// filterVisibleUserIDsForChannel reads neither sync_permissions nor
	// category_permission_overrides, so flipping the flag alone changes no
	// viewer set. The visibility-changing work is copyCategoryOverridesToChannel
	// below, which carries its own lock + capture.
	if _, err = h.db.Exec(`UPDATE channels SET sync_permissions = $1 WHERE id = $2`, req.SyncPermissions, channelID); err != nil {
		h.log.Error("Failed to update sync flag", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateSync})
		return
	}

	if req.SyncPermissions && groupID != nil {
		if err := h.copyCategoryOverridesToChannel(c.Request.Context(), serverID, channelID, *groupID); err != nil {
			h.log.Error("Failed to sync category overrides to channel", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateSync})
			return
		}
		_ = h.cache.InvalidateChannel(c.Request.Context(), serverID, channelID)
		h.recheckVoiceChannel(serverID, channelID)
		h.revalidateChannelSubscribers(serverID, channelID)
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

// copyCategoryOverridesToChannel replaces one channel's overrides with its
// category's. It is reached from SetChannelPermissionSync, NOT from the
// category-override cascade. Phase 1 runs before BeginTx; the advisory lock and
// phase 2 are the transaction's first statements (#2445).
// deleteCategoryOverrideWithCapture removes a category override and the rows it
// mirrored onto every synced child channel, binding a pre-mutation Server Voice
// Rich Presence capture to the write (#2445).
//
// The cascade DELETE is a visibility-changing write:
// filterVisibleUserIDsForChannel reads channel_permission_overrides for both
// role and user targets, so removing an allow-override strictly narrows the
// viewer set on every synced child. It was unhooked and its error discarded
// (`_, _ = h.db.Exec`), so a viewer whose only sight of an occupied voice
// channel came from the category-synced allow kept the sender's live "in Server
// Voice" state until the ≤90 s ActivityStateTTL expired. The #2445 review's
// adversarial pass proved that with a proof-of-concept whose control arm — the
// identical narrowing routed through the already-hooked DeleteChannelOverride —
// did capture the victim, so the silence was a real gap and not a dead fixture.
//
// Both deletes now share one transaction, so the parent row and its mirrored
// children can no longer diverge: previously a failed cascade left the parent
// deleted, the children stale, and the API answering 200.
//
// Unlike syncCategoryOverridesToChannels, nothing has committed when phase 1
// runs here, so a capture failure legitimately blocks the write per design §8
// row 1 — rollback, nothing changed, nothing disclosed, retryable.
func (h *Handler) deleteCategoryOverrideWithCapture(
	ctx context.Context,
	serverID, categoryID, overrideID, targetType, targetID string,
) error {
	// Capture scope is the voice subset; the cascade DELETE still targets every
	// synced child via its own subquery.
	_, voiceChannelIDs, err := h.syncedChannelsForCategory(ctx, categoryID)
	if err != nil {
		return fmt.Errorf("enumerate synced channels: %w", err)
	}

	// PHASE 1 — pre-transaction, outside the advisory lock.
	plan, err := h.preparePresenceCapture(ctx, serverID, voiceChannelIDs, nil)
	if err != nil {
		return fmt.Errorf("prepare category override presence capture: %w", err)
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if err := LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return err
	}
	// PHASE 2 — under the lock, before the writes.
	if err := h.capturePresenceVisibility(ctx, tx, plan); err != nil {
		return fmt.Errorf("capture category override presence audience: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM category_permission_overrides WHERE id = $1 AND category_id = $2`,
		overrideID, categoryID,
	); err != nil {
		return fmt.Errorf("delete category override: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM channel_permission_overrides
		WHERE channel_id IN (SELECT id FROM channels WHERE group_id = $1 AND sync_permissions = TRUE)
		  AND target_type = $2 AND target_id = $3
	`, categoryID, targetType, targetID); err != nil {
		return fmt.Errorf("cascade delete synced channel overrides: %w", err)
	}

	if err := tx.Commit(); err != nil {
		h.presenceAbandon(plan, "ambiguous_commit")
		return err
	}
	h.presenceExecute(plan)
	return nil
}

func (h *Handler) copyCategoryOverridesToChannel(
	ctx context.Context,
	serverID, channelID, categoryID string,
) error {
	// PHASE 1 — pre-transaction, outside the advisory lock.
	plan, err := h.preparePresenceCapture(ctx, serverID, []string{channelID}, nil)
	if err != nil {
		return fmt.Errorf("prepare channel presence capture: %w", err)
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if err := LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return err
	}
	// PHASE 2 — under the lock, before the write.
	if err := h.capturePresenceVisibility(ctx, tx, plan); err != nil {
		return fmt.Errorf("capture channel presence audience: %w", err)
	}

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

	if err := tx.Commit(); err != nil {
		h.presenceAbandon(plan, "ambiguous_commit")
		return err
	}
	h.presenceExecute(plan)
	return nil
}

// syncedChannelsForCategory returns every sync_permissions child of a category
// and, separately, the voice subset. Callers rewrite overrides on all of them
// but capture Rich Presence on only the voice ones.
//
// Errors propagate. The previous inline loop dropped per-row Scan errors and
// never checked rows.Err(), so a mid-iteration failure yielded a silently
// truncated list — the dropped channels kept their stale, more-permissive
// overrides while the caller reported success, defeating the "all channels
// update or none do" property the transaction below exists to provide.
func (h *Handler) syncedChannelsForCategory(
	ctx context.Context,
	categoryID string,
) (all []string, voice []string, err error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, type = 'voice' FROM channels
		WHERE group_id = $1 AND sync_permissions = TRUE
	`, categoryID)
	if err != nil {
		return nil, nil, fmt.Errorf("query synced channels: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only scan; Err() is checked below

	for rows.Next() {
		var (
			chID    string
			isVoice bool
		)
		if scanErr := rows.Scan(&chID, &isVoice); scanErr != nil {
			return nil, nil, fmt.Errorf("scan synced channel: %w", scanErr)
		}
		all = append(all, chID)
		if isVoice {
			voice = append(voice, chID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate synced channels: %w", err)
	}
	return all, voice, nil
}

// syncCategoryOverridesToChannels copies all category overrides to synced child
// channels. The hook captures exactly the synced-channel list it rewrites, so
// all N channel deltas are atomic with all N writes under one commit (#2445).
// Phase 1 runs before BeginTx so the cascade's O(#senders) candidate resolution
// never holds the advisory lock.
func (h *Handler) syncCategoryOverridesToChannels(ctx context.Context, serverID, categoryID string) {
	// Two lists, deliberately. The WRITE loop below must touch every synced
	// child channel; the presence capture only has work on voice channels
	// (Executor.activeSenders filters c.type = 'voice'). Feeding the unfiltered
	// list to the capture made text channels consume presenceCaptureMaxChannels,
	// so a category of 65 synced text channels — zero presence work — tripped
	// the voice fan-out bound (#2445 review).
	channelIDs, voiceChannelIDs, err := h.syncedChannelsForCategory(ctx, categoryID)
	if err != nil {
		h.log.Error("Failed to query synced channels", "error", err)
		return
	}

	if len(channelIDs) == 0 {
		return
	}

	// PHASE 1 — pre-transaction, outside the advisory lock.
	//
	// A capture failure must NEVER veto the permission write. This function is
	// called after UpsertCategoryOverride/DeleteCategoryOverride already
	// committed the parent category_permission_overrides row, and the child
	// channel_permission_overrides rows ARE the enforcement — the visibility
	// resolver never reads the category table. Returning here left the parent
	// persisted and every child stale while the API answered 200, so tightening
	// a category override silently no-opped while loosening one still worked:
	// the wrong direction to fail, and a regression this PR introduced (the
	// pre-#2445 function had no capture-shaped abort).
	//
	// So degrade instead: drop the plan, keep the writes. Presence then falls
	// back to the ≤90 s ActivityStateTTL, which is exactly the pre-#2445
	// baseline for this path — never worse than before — while access control
	// stays correct. Diverges from design §8 row 1 ("pre-tx resolution errors
	// block the write") deliberately: row 1 governs the six authority handlers,
	// whose write has not happened yet. Here it has.
	plan, err := h.preparePresenceCapture(ctx, serverID, voiceChannelIDs, nil)
	if err != nil {
		h.log.Error("Cascade presence capture unavailable; applying overrides without it",
			"failure_class", "cascade_capture_unavailable", "error", err)
		plan = nil
	}

	// Wrap in transaction for atomicity — all channels update or none do
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin sync transaction", "error", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if err := LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		h.log.Error("Failed to lock sync transaction", "error", err)
		return
	}
	// PHASE 2 — under the lock, before the writes. One query per channel.
	// Same rule as phase 1: the capture does not get to block the write. A
	// partially-populated plan is fail-closed via Abandon (disconnect what was
	// captured — a disconnect discloses nothing) rather than executed, since a
	// partial plan would otherwise produce a partial clear.
	if err := h.capturePresenceVisibility(ctx, tx, plan); err != nil {
		h.log.Error("Cascade presence capture failed; applying overrides without it",
			"failure_class", "cascade_capture_failed", "error", err)
		h.presenceAbandon(plan, "cascade_capture_failed")
		plan = nil
	}

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
		h.presenceAbandon(plan, "ambiguous_commit")
		return
	}
	h.presenceExecute(plan)
}

// invalidateSyncedChannelCaches invalidates caches for all channels in a category.
//
// NOT hooked for #2445 presence capture: it performs NO write, and it iterates
// ALL channels in the category including unsynced ones. Capturing here would be
// over-broad AND pointless. It keeps its existing recheckVoiceChannel loop.
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
			h.recheckVoiceChannel(serverID, chID)
			h.revalidateChannelSubscribers(serverID, chID)
		}
	}
}
