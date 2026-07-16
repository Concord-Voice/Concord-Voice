package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

var (
	// ErrNotMember is returned when a user is not a member of the server
	ErrNotMember = errors.New("user is not a member of this server")

	// ErrHierarchyViolation is returned when attempting to modify a user with equal/higher role
	ErrHierarchyViolation = errors.New("cannot modify member with equal or higher role position")
)

const (
	// errMsgHierarchyCheckFailed is the format string for errors wrapping DB failures in CheckHierarchy.
	errMsgHierarchyCheckFailed = "hierarchy check failed: %w"
)

// Resolver computes effective permissions for users by combining RBAC and SBAC layers
type Resolver struct {
	db    *sql.DB
	cache *PermissionCache
	log   *logger.Logger
}

// NewResolver creates a new RBAC resolver
func NewResolver(db *sql.DB, cache *PermissionCache, log *logger.Logger) *Resolver {
	return &Resolver{
		db:    db,
		cache: cache,
		log:   log,
	}
}

// HasPermission checks if a user has a specific permission in a server/channel context
// - serverID: required (all permissions are server-scoped)
// - userID: required
// - channelID: optional (empty string for server-level permissions only)
// - perm: the permission to check
//
// Returns (true, nil) if user has permission
// Returns (false, nil) if user lacks permission
// Returns (false, err) on database/system errors
func (r *Resolver) HasPermission(ctx context.Context, serverID, userID, channelID string, perm Permission) (bool, error) {
	// Check cache first
	if cached, ok := r.cache.Get(ctx, serverID, userID, channelID); ok {
		return cached.Has(perm), nil
	}

	// Compute effective permissions
	effectivePerm, err := r.computeEffectivePermissions(ctx, serverID, userID, channelID)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			return false, nil // Not a member = no permissions (not an error condition)
		}
		return false, err
	}

	// Cache result
	_ = r.cache.Set(ctx, serverID, userID, channelID, effectivePerm)

	return effectivePerm.Has(perm), nil
}

// GetEffectivePermissions returns the computed permission bitfield for a user
// Useful for frontend to determine which UI elements to show
func (r *Resolver) GetEffectivePermissions(ctx context.Context, serverID, userID, channelID string) (Permission, error) {
	// Check cache first
	if cached, ok := r.cache.Get(ctx, serverID, userID, channelID); ok {
		return cached, nil
	}

	// Compute and cache
	effectivePerm, err := r.computeEffectivePermissions(ctx, serverID, userID, channelID)
	if err != nil {
		return 0, err
	}

	_ = r.cache.Set(ctx, serverID, userID, channelID, effectivePerm)
	return effectivePerm, nil
}

// ResolveEffectivePermissionsFresh recomputes effective permissions directly
// from the database, bypassing the cache READ (it still refreshes the cache
// with the fresh value). Used by the voice PermissionEnforcer (CV-CAN-007 P1):
// an enforcement push must reflect committed DB state, never a cache entry
// that a concurrently in-flight pre-mutation compute may have repopulated
// after the mutation's invalidation.
func (r *Resolver) ResolveEffectivePermissionsFresh(ctx context.Context, serverID, userID, channelID string) (Permission, error) {
	perms, err := r.computeEffectivePermissions(ctx, serverID, userID, channelID)
	if err != nil {
		return 0, err
	}
	if r.cache != nil {
		_ = r.cache.Set(ctx, serverID, userID, channelID, perms)
	}
	return perms, nil
}

// computeEffectivePermissions implements the two-layer permission resolution model:
// 1. RBAC: OR together permissions from all user's roles
// 2. SBAC: Apply channel-specific overrides (deny > allow)
func (r *Resolver) computeEffectivePermissions(ctx context.Context, serverID, userID, channelID string) (Permission, error) {
	// Step 1: Verify server membership
	var isMember bool
	memberQuery := `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`
	if err := r.db.QueryRowContext(ctx, memberQuery, serverID, userID).Scan(&isMember); err != nil {
		return 0, fmt.Errorf("failed to check membership: %w", err)
	}
	if !isMember {
		return 0, ErrNotMember
	}

	// Step 2: Check if user is server owner (owner bypasses RBAC, gets OwnerPermissions)
	var ownerID string
	ownerQuery := `SELECT owner_id FROM servers WHERE id = $1`
	if err := r.db.QueryRowContext(ctx, ownerQuery, serverID).Scan(&ownerID); err != nil {
		return 0, fmt.Errorf("failed to fetch server owner: %w", err)
	}
	if ownerID == userID {
		// Owner gets all permissions — immune to channel overrides
		// Owner bypasses SBAC layer entirely (cannot be restricted per-channel)
		return OwnerPermissions, nil
	}

	// Step 3: Compute base permissions from roles (OR all role permissions together)
	basePerms, err := r.computeRolePermissions(ctx, serverID, userID)
	if err != nil {
		return 0, fmt.Errorf("failed to compute role permissions: %w", err)
	}

	// Step 4: If no channel specified, return base permissions
	if channelID == "" {
		return basePerms, nil
	}

	// Step 5: Apply channel-specific overrides (SBAC layer)
	finalPerms, err := r.applyChannelOverrides(ctx, channelID, userID, basePerms)
	if err != nil {
		return 0, fmt.Errorf("failed to apply channel overrides: %w", err)
	}

	return finalPerms, nil
}

// computeRolePermissions computes base permissions by OR'ing all user's role permissions
func (r *Resolver) computeRolePermissions(ctx context.Context, serverID, userID string) (Permission, error) {
	query := `
		SELECT COALESCE(BIT_OR(r.permissions), 0) AS total_permissions
		FROM member_roles mr
		INNER JOIN roles r ON mr.role_id = r.id
		WHERE mr.server_id = $1 AND mr.user_id = $2
	`

	var totalPerms int64
	if err := r.db.QueryRowContext(ctx, query, serverID, userID).Scan(&totalPerms); err != nil {
		// COALESCE(BIT_OR(...), 0) always returns a row, so ErrNoRows is unreachable.
		// Any error here is a genuine database failure.
		return 0, err
	}

	return Permission(totalPerms), nil
}

// applyChannelOverrides applies channel-specific permission overrides (SBAC layer)
// Applied in order (each step modifies the result of the previous):
// 1. Base permissions (from roles)
// 2. Role-specific allow (grant additional permissions)
// 3. Role-specific deny (revoke permissions)
// 4. User-specific allow (grant additional permissions, overrides role deny)
// 5. User-specific deny (highest priority, final authority)
func (r *Resolver) applyChannelOverrides(ctx context.Context, channelID, userID string, basePerms Permission) (Permission, error) {
	// Administrator bypass: channel overrides cannot restrict administrators
	if basePerms.Has(PermAdministrator) {
		return basePerms, nil
	}

	// Fetch all applicable overrides (user + user's roles)
	query := `
		SELECT target_type, allow, deny
		FROM channel_permission_overrides
		WHERE channel_id = $1
		  AND (
		      (target_type = 'user' AND target_id = $2)
		      OR (target_type = 'role' AND target_id IN (
		          SELECT mr.role_id FROM member_roles mr
		          INNER JOIN channels c ON c.server_id = mr.server_id
		          WHERE mr.user_id = $2 AND c.id = $1
		      ))
		  )
		ORDER BY target_type DESC -- 'user' before 'role' (user overrides have priority)
	`

	rows, err := r.db.QueryContext(ctx, query, channelID, userID)
	if err != nil {
		return 0, err
	}
	defer rows.Close() //nolint:errcheck

	var userAllow, userDeny, roleAllow, roleDeny Permission

	for rows.Next() {
		var targetType string
		var allow, deny int64
		if err := rows.Scan(&targetType, &allow, &deny); err != nil {
			return 0, err
		}

		if targetType == "user" {
			userAllow |= Permission(allow)
			userDeny |= Permission(deny)
		} else {
			roleAllow |= Permission(allow)
			roleDeny |= Permission(deny)
		}
	}

	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Apply overrides in order: base → role allow → role deny → user allow → user deny
	finalPerms := basePerms
	finalPerms |= roleAllow // Add role-allowed permissions
	finalPerms &^= roleDeny // Remove role-denied permissions
	finalPerms |= userAllow // Add user-allowed permissions
	finalPerms &^= userDeny // Remove user-denied permissions (final authority)

	return finalPerms, nil
}

// InvalidateChannel clears cached permission entries for every user in a channel.
// It is a thin public passthrough to the cache's InvalidateChannel (cache.go), used by
// the voice package after a temporary-SBAC grant/revoke so that the next permission
// resolution reflects the changed override (#487).
func (r *Resolver) InvalidateChannel(ctx context.Context, serverID, channelID string) error {
	return r.cache.InvalidateChannel(ctx, serverID, channelID)
}

// sbacChannelOverrideColumns is the SELECT column list, shared verbatim by the
// channel_overrides CTE in GetVisibleChannelIDs and GetAllVisibleChannelIDs, that
// aggregates the SBAC role/user allow/deny bitfields for a channel. Both callers
// then derive effective visibility from these four columns using the identical
// formula ((base | role_allow) & ~role_deny | user_allow) & ~user_deny, so the
// aggregation must stay byte-for-byte identical between them; keeping it in one
// constant guarantees the two queries can never silently drift apart.
//
// It is a compile-time string constant concatenated into each query (no fmt.Sprintf
// and no concatenation of runtime values), so it adds no dynamic-SQL surface and is
// not flagged by gosec G201/G202 or the concord-go-sql-sprintf semgrep rule.
const sbacChannelOverrideColumns = `
				COALESCE(BIT_OR(cpo.allow) FILTER (WHERE cpo.target_type = 'role'), 0) AS role_allow,
				COALESCE(BIT_OR(cpo.deny)  FILTER (WHERE cpo.target_type = 'role'), 0) AS role_deny,
				COALESCE(BIT_OR(cpo.allow) FILTER (WHERE cpo.target_type = 'user'), 0) AS user_allow,
				COALESCE(BIT_OR(cpo.deny)  FILTER (WHERE cpo.target_type = 'user'), 0) AS user_deny`

// GetVisibleChannelIDs returns a list of channel IDs that the user can view.
// Visibility is type-aware: text/bulletin channels require PermViewTextChannels,
// voice channels require PermViewVoiceChannels. This allows RBAC roles (and SBAC
// overrides) to independently control visibility for each channel type.
//
// Optimized to resolve visibility for ALL channels in a single SQL query,
// replicating the RBAC+SBAC resolution logic (base | role_allow &^ role_deny
// | user_allow &^ user_deny) in SQL rather than looping per-channel.
func (r *Resolver) GetVisibleChannelIDs(ctx context.Context, serverID, userID string) ([]string, error) {
	// Membership gate: non-members see nothing, even if stale overrides exist
	var isMember bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID).Scan(&isMember); err != nil {
		return nil, fmt.Errorf("failed to check membership: %w", err)
	}
	if !isMember {
		return []string{}, nil
	}

	// Fast path: server owner sees everything
	var ownerID string
	if err := r.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		return nil, fmt.Errorf("failed to fetch server owner: %w", err)
	}
	if ownerID == userID {
		return r.getAllChannelIDs(ctx, serverID)
	}

	// Compute base role permissions (single query, same as computeRolePermissions)
	var basePerms int64
	roleQuery := `
		SELECT COALESCE(BIT_OR(r.permissions), 0)
		FROM member_roles mr
		INNER JOIN roles r ON mr.role_id = r.id
		WHERE mr.server_id = $1 AND mr.user_id = $2
	`
	if err := r.db.QueryRowContext(ctx, roleQuery, serverID, userID).Scan(&basePerms); err != nil {
		return nil, fmt.Errorf("failed to compute role permissions: %w", err)
	}

	// Fast path: administrators see everything (SBAC cannot restrict them)
	if Permission(basePerms).Has(PermAdministrator) {
		return r.getAllChannelIDs(ctx, serverID)
	}

	// Single query: for each channel, compute effective permissions with SBAC
	// overrides, then check the appropriate view permission bit based on channel type.
	//
	// The SBAC resolution mirrors applyChannelOverrides exactly:
	//   effective = ((base | role_allow) & ~role_deny | user_allow) & ~user_deny
	//
	// Channel type mapping:
	//   text, bulletin → PermViewTextChannels ($4)
	//   voice          → PermViewVoiceChannels ($5)
	query := `
		WITH user_roles AS (
			SELECT mr.role_id
			FROM member_roles mr
			WHERE mr.server_id = $1 AND mr.user_id = $2
		),
		channel_overrides AS (
			SELECT
				cpo.channel_id,` + sbacChannelOverrideColumns + `
			FROM channel_permission_overrides cpo
			WHERE cpo.channel_id IN (SELECT id FROM channels WHERE server_id = $1)
			  AND (
			      (cpo.target_type = 'role' AND cpo.target_id IN (SELECT role_id FROM user_roles))
			      OR (cpo.target_type = 'user' AND cpo.target_id = $2)
			  )
			GROUP BY cpo.channel_id
		)
		SELECT c.id
		FROM channels c
		LEFT JOIN channel_overrides co ON co.channel_id = c.id
		WHERE c.server_id = $1
		  AND (
		    -- Compute effective permissions via SBAC bitfield math
		    (
		      (
		        ($3::bigint | COALESCE(co.role_allow, 0)) & ~COALESCE(co.role_deny, 0)
		        | COALESCE(co.user_allow, 0)
		      ) & ~COALESCE(co.user_deny, 0)
		    ) &
		    -- Check the appropriate view permission bit based on channel type
		    CASE WHEN c.type = 'voice' THEN $5::bigint ELSE $4::bigint END
		    != 0
		  )
	`

	return r.queryChannelIDs(ctx, "failed to query visible channels", query,
		serverID, userID, basePerms, int64(PermViewTextChannels), int64(PermViewVoiceChannels))
}

// GetAllVisibleChannelIDs returns every channel ID the user can view across all
// servers they belong to, resolving visibility in a single SQL round-trip.
//
// This is the cross-server counterpart to GetVisibleChannelIDs. Calling
// GetVisibleChannelIDs once per server issues 1+N queries (and up to ~4 per
// server internally), which regresses hot paths like GetServerUnreadStatus for
// users in many servers. This query folds membership, the owner/administrator
// fast paths, and the per-channel SBAC bitfield math into one statement, using
// the exact same resolution rules:
//
//	effective = ((base | role_allow) & ~role_deny | user_allow) & ~user_deny
//
// A channel is visible when the caller owns the server, is an administrator of
// it, or the effective permissions include the type-appropriate view bit
// (PermViewTextChannels for text/bulletin, PermViewVoiceChannels for voice).
func (r *Resolver) GetAllVisibleChannelIDs(ctx context.Context, userID string) ([]string, error) {
	// One statement across every server the user is a member of:
	//   - memberships: gates results to the user's servers (non-members see nothing)
	//   - base_perms:  BIT_OR of the user's role permissions per server (owner/admin fast paths)
	//   - user_roles / channel_overrides: SBAC allow/deny bitfields for the user's roles + user,
	//     with role overrides scoped to the channel's server (a role the user holds in one server
	//     must not satisfy an override on a channel in another server)
	// The final predicate mirrors GetVisibleChannelIDs: owner OR administrator OR
	// the SBAC effective permissions include the type-appropriate view bit.
	query := `
		WITH memberships AS (
			SELECT server_id
			FROM server_members
			WHERE user_id = $1
		),
		user_roles AS (
			SELECT mr.role_id, mr.server_id
			FROM member_roles mr
			WHERE mr.user_id = $1
		),
		base_perms AS (
			SELECT mr.server_id,
				COALESCE(BIT_OR(r.permissions), 0) AS perms
			FROM member_roles mr
			INNER JOIN roles r ON mr.role_id = r.id
			WHERE mr.user_id = $1
			GROUP BY mr.server_id
		),
		channel_overrides AS (
			SELECT
				cpo.channel_id,` + sbacChannelOverrideColumns + `
			FROM channel_permission_overrides cpo
			JOIN channels ch ON ch.id = cpo.channel_id
			WHERE (
				-- Role overrides apply only when the targeted role belongs to the same
				-- server as the channel. user_roles spans every server the user is in,
				-- so without this server scoping a role the user holds in server X could
				-- satisfy an override on a channel in server Y (target_id is only
				-- UUID-validated), diverging from the per-server resolver which scopes
				-- role overrides to the channel's server.
				(cpo.target_type = 'role' AND cpo.target_id IN (
					SELECT ur.role_id FROM user_roles ur WHERE ur.server_id = ch.server_id
				))
				OR (cpo.target_type = 'user' AND cpo.target_id = $1)
			)
			GROUP BY cpo.channel_id
		)
		SELECT c.id
		FROM channels c
		INNER JOIN memberships m ON m.server_id = c.server_id
		INNER JOIN servers s ON s.id = c.server_id
		LEFT JOIN base_perms bp ON bp.server_id = c.server_id
		LEFT JOIN channel_overrides co ON co.channel_id = c.id
		WHERE
			-- Owner fast path: owner sees every channel
			s.owner_id = $1
			-- Administrator fast path: SBAC cannot restrict administrators
			OR (COALESCE(bp.perms, 0) & $2::bigint) != 0
			-- Otherwise: per-channel SBAC bitfield math against the view bit
			OR (
				(
					(
						(COALESCE(bp.perms, 0) | COALESCE(co.role_allow, 0)) & ~COALESCE(co.role_deny, 0)
						| COALESCE(co.user_allow, 0)
					) & ~COALESCE(co.user_deny, 0)
				) &
				CASE WHEN c.type = 'voice' THEN $4::bigint ELSE $3::bigint END
				!= 0
			)
	`

	return r.queryChannelIDs(ctx, "failed to query all visible channels", query,
		userID, int64(PermAdministrator), int64(PermViewTextChannels), int64(PermViewVoiceChannels))
}

// getAllChannelIDs returns all channel IDs for a server (used for owner/admin fast path)
func (r *Resolver) getAllChannelIDs(ctx context.Context, serverID string) ([]string, error) {
	return r.queryChannelIDs(ctx, "failed to query channels",
		`SELECT id FROM channels WHERE server_id = $1`, serverID)
}

// queryChannelIDs runs a query whose rows each carry a single channel-id column and
// collects them into a slice, wrapping any query error with wrapMsg. A nil result is
// normalized to a non-nil empty slice so callers can return it directly. The channel
// visibility resolvers (GetVisibleChannelIDs, GetAllVisibleChannelIDs) and the
// owner/admin fast path (getAllChannelIDs) share this so the row-scan/collect
// boilerplate lives in exactly one place.
//
// query is always a caller-supplied constant SQL string with values bound as
// positional parameters (never interpolated), so this introduces no dynamic-SQL
// surface for gosec G201/G202.
func (r *Resolver) queryChannelIDs(ctx context.Context, wrapMsg, query string, args ...interface{}) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", wrapMsg, err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// CheckHierarchy verifies that actorID has a higher role position than targetID
// Used to enforce "you can only kick/ban users with lower roles than you"
// Returns nil if actor outranks target, ErrHierarchyViolation otherwise
//
// Bypass rules (checked before position comparison):
// - Server owner always outranks everyone (owner gets permissions via owner-id bypass, not via roles)
// - Users with PermAdministrator always outrank non-administrators
func (r *Resolver) CheckHierarchy(ctx context.Context, serverID, actorID, targetID string) error {
	// Server owner bypasses hierarchy — owner can moderate anyone
	var ownerID string
	if err := r.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		return fmt.Errorf(errMsgHierarchyCheckFailed, err)
	}
	if actorID == ownerID {
		return nil // Owner outranks everyone
	}

	// Check if actor has PermAdministrator (bypasses position-based hierarchy)
	actorPerms, err := r.computeRolePermissions(ctx, serverID, actorID)
	if err != nil {
		return fmt.Errorf(errMsgHierarchyCheckFailed, err)
	}
	if actorPerms.Has(PermAdministrator) {
		// Administrator bypasses hierarchy unless target is also owner
		if targetID == ownerID {
			return ErrHierarchyViolation // Cannot moderate the server owner
		}
		return nil
	}

	// Owner is immune to moderation by non-owners (position comparison could
	// incorrectly allow this since owners may only have the @all role at position 0)
	if targetID == ownerID {
		return ErrHierarchyViolation
	}

	// Fall back to position-based comparison for non-owner, non-admin actors
	query := `
		WITH actor_max AS (
			SELECT COALESCE(MAX(r.position), 0) AS pos
			FROM member_roles mr
			INNER JOIN roles r ON mr.role_id = r.id
			WHERE mr.server_id = $1 AND mr.user_id = $2
		),
		target_max AS (
			SELECT COALESCE(MAX(r.position), 0) AS pos
			FROM member_roles mr
			INNER JOIN roles r ON mr.role_id = r.id
			WHERE mr.server_id = $1 AND mr.user_id = $3
		)
		SELECT actor_max.pos > target_max.pos AS can_modify
		FROM actor_max, target_max
	`

	var canModify bool
	if err := r.db.QueryRowContext(ctx, query, serverID, actorID, targetID).Scan(&canModify); err != nil {
		return fmt.Errorf(errMsgHierarchyCheckFailed, err)
	}

	if !canModify {
		return ErrHierarchyViolation
	}

	return nil
}
