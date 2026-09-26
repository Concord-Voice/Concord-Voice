package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

var (
	// ErrNotMember is returned when a user is not a member of the server
	ErrNotMember = errors.New("user is not a member of this server")

	// ErrHierarchyViolation is returned when attempting to modify a user with equal/higher role
	ErrHierarchyViolation = errors.New("cannot modify member with equal or higher role position")

	// ErrChannelNotInServer is returned when a channel-scoped resolve names a
	// channel that does not belong to the server it was asked about. It wraps
	// ErrNotMember, so every caller that already denies a non-member denies this
	// too (#2869).
	ErrChannelNotInServer = fmt.Errorf("%w: channel is not in this server", ErrNotMember)
)

// channelsInServerQuery is true iff every id in $2 names a channel of server $1.
const channelsInServerQuery = `
	SELECT NOT EXISTS (
		SELECT 1 FROM unnest($2::uuid[]) AS want(id)
		WHERE NOT EXISTS (SELECT 1 FROM channels c WHERE c.id = want.id AND c.server_id = $1)
	)`

// requireChannelsInServer is the channel half of every channel-scoped resolve.
// Base permissions come from serverID and overrides from channelID, and nothing
// else ties the two together: without this, an owner or administrator of one
// server would be handed their full base set for any channel ID a caller passed
// alongside it. Every production caller derives the server from the channel row
// today; this keeps a future caller that takes them from separate request fields
// from turning that into a cross-server grant (#2869).
func requireChannelsInServer(ctx context.Context, q rowQuerier, serverID string, channelIDs []string) error {
	var ok bool
	if err := q.QueryRowContext(ctx, channelsInServerQuery, serverID, pq.Array(channelIDs)).Scan(&ok); err != nil {
		return fmt.Errorf("failed to check channel server: %w", err)
	}
	if !ok {
		return ErrChannelNotInServer
	}
	return nil
}

const (
	// errMsgHierarchyCheckFailed is the format string for errors wrapping DB failures in CheckHierarchy.
	errMsgHierarchyCheckFailed = "hierarchy check failed: %w"
)

// Resolver computes effective permissions for users by combining RBAC and SBAC layers
type Resolver struct {
	db    *sql.DB
	cache *PermissionCache
	log   *logger.Logger

	// afterCompute, when set, runs immediately after a cache-publishing
	// compute returns, before anything else reads Redis or calls cache.Set. It
	// is a test seam for the in-flight race (#3453 I-3), set only through
	// export_test.go, and nil in production.
	afterCompute func()
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NewResolver creates a new RBAC resolver
func NewResolver(db *sql.DB, cache *PermissionCache, log *logger.Logger) *Resolver {
	return &Resolver{
		db:    db,
		cache: cache,
		log:   log,
	}
}

// cacheSet publishes a computed value tagged with the generations read BEFORE
// that compute began (see PermissionCache). A failed write costs only a
// recompute on the next read, so it is logged rather than returned.
func (r *Resolver) cacheSet(ctx context.Context, serverID, userID, channelID string, perm Permission, tags GenTags) {
	if err := r.cache.Set(ctx, serverID, userID, channelID, perm, tags); err != nil && r.log != nil {
		r.log.Warn("Failed to cache permissions", "error", err)
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
	effectivePerm, err := r.GetEffectivePermissions(ctx, serverID, userID, channelID)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			return false, nil // Not a member = no permissions (not an error condition)
		}
		return false, err
	}
	return effectivePerm.Has(perm), nil
}

// GetEffectivePermissions returns the computed permission bitfield for a user
// Useful for frontend to determine which UI elements to show
//
// It is cache-first. On a miss it computes and publishes with the generations
// Get read, which were current before the compute began.
func (r *Resolver) GetEffectivePermissions(ctx context.Context, serverID, userID, channelID string) (Permission, error) {
	cached, ok, tags := r.cache.Get(ctx, serverID, userID, channelID)
	if ok {
		return cached, nil
	}

	effectivePerm, err := r.computeEffectivePermissions(ctx, serverID, userID, channelID)
	if err != nil {
		return 0, err
	}
	r.runAfterCompute()

	r.cacheSet(ctx, serverID, userID, channelID, effectivePerm, tags)
	return effectivePerm, nil
}

// ResolveEffectivePermissionsFresh recomputes effective permissions directly
// from the database, bypassing the cache READ (it still refreshes the cache
// with the fresh value). Used by the voice PermissionEnforcer (CV-CAN-007 P1):
// an enforcement push must reflect committed DB state, never a cache entry
// that a concurrently in-flight pre-mutation compute may have repopulated
// after the mutation's invalidation.
//
// It reads no cached VALUE, but it must still read the generations BEFORE the
// compute: publishing with generations read afterwards would tag a value
// computed before a concurrent change with the generation that change bumped
// to, and the next cached read would serve it.
func (r *Resolver) ResolveEffectivePermissionsFresh(ctx context.Context, serverID, userID, channelID string) (Permission, error) {
	var tags GenTags
	if r.cache != nil {
		tags = r.cache.Generations(ctx, serverID, userID)
	}
	perms, err := r.computeEffectivePermissions(ctx, serverID, userID, channelID)
	if err != nil {
		return 0, err
	}
	r.runAfterCompute()
	if r.cache != nil {
		r.cacheSet(ctx, serverID, userID, channelID, perms, tags)
	}
	return perms, nil
}

func (r *Resolver) runAfterCompute() {
	if r.afterCompute != nil {
		r.afterCompute()
	}
}

// ResolveEffectivePermissionsUncached recomputes permissions from the database
// without reading or publishing a cache entry. Destructive preflight paths use
// it so an in-flight result cannot restore permissions after invalidation.
//
// The channel arm returns ResolveEffectivePermissionsForChannelsFresh's value,
// which that entry point has already masked; only the server arm masks here.
func (r *Resolver) ResolveEffectivePermissionsUncached(ctx context.Context, serverID, userID, channelID string) (Permission, error) {
	if channelID != "" {
		permsByChannel, err := r.ResolveEffectivePermissionsForChannelsFresh(ctx, serverID, userID, []string{channelID})
		if err != nil {
			return 0, err
		}
		return permsByChannel[channelID], nil
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("failed to begin permission snapshot: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			r.log.Warn("failed to rollback permission snapshot", "error", rollbackErr)
		}
	}()

	raw, _, mask, err := r.resolveServerPermissions(ctx, tx, serverID, userID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit permission snapshot: %w", err)
	}
	return mask.Apply(raw), nil
}

// ResolveServerPermissionsTx resolves a member's SERVER-scope effective
// permissions on a caller-supplied transaction, reading and publishing NO cache
// entry. The RBAC role-mutation guards (#2721) use it so the actor's permission
// set is resolved inside the same transaction as the write it authorizes.
//
// It deliberately mirrors ResolveEffectivePermissionsUncached, NOT ...Fresh.
// Fresh calls cache.Set; publishing from a transaction that then rolls back
// would seed Redis with a state that never committed. It also does not open its
// own REPEATABLE READ snapshot the way ...Uncached does — the caller's
// transaction IS the snapshot, which is the entire point of taking a rowQuerier.
//
// Channel scope is intentionally unsupported: every caller authorizes a
// server-level roles.permissions bitfield. For channel scope use
// ResolveEffectivePermissionsForChannelsFresh instead.
func (r *Resolver) ResolveServerPermissionsTx(
	ctx context.Context, q rowQuerier, serverID, userID string,
) (Permission, error) {
	raw, _, mask, err := r.resolveServerPermissions(ctx, q, serverID, userID)
	if err != nil {
		return 0, err
	}
	return mask.Apply(raw), nil
}

// ResolveChannelPermissionsTx resolves one channel's effective permissions in
// the caller's transaction. It deliberately avoids the cache and a nested
// transaction because callers use it to authorize a concurrent mutation.
func (r *Resolver) ResolveChannelPermissionsTx(
	ctx context.Context, tx *sql.Tx, serverID, userID, channelID string,
) (Permission, error) {
	basePerms, isOwner, mask, err := r.resolveServerPermissions(ctx, tx, serverID, userID)
	if err != nil {
		return 0, err
	}
	if err := requireChannelsInServer(ctx, tx, serverID, []string{channelID}); err != nil {
		return 0, err
	}
	// The bypass reads the RAW base: an unenrolled Administrator still ignores
	// channel overrides, and is masked only at the exit below.
	raw := basePerms
	if !isOwner && !basePerms.Has(PermAdministrator) {
		permsByChannel := map[string]Permission{channelID: basePerms}
		if err := r.applyBatchedChannelOverrides(ctx, tx, []string{channelID}, serverID, userID, basePerms, permsByChannel); err != nil {
			return 0, err
		}
		raw = permsByChannel[channelID]
	}
	return mask.Apply(raw), nil
}

// ResolveEffectivePermissionsForChannelsFresh resolves a member's effective
// permissions for channels from one server in one fresh database pass. It keeps
// per-channel SBAC allow/deny semantics while avoiding an N+1 preflight loop.
func (r *Resolver) ResolveEffectivePermissionsForChannelsFresh(ctx context.Context, serverID, userID string, channelIDs []string) (map[string]Permission, error) {
	permsByChannel := make(map[string]Permission, len(channelIDs))
	if len(channelIDs) == 0 {
		return permsByChannel, nil
	}

	// One repeatable-read transaction prevents a concurrent membership or role
	// mutation from mixing base RBAC with a later SBAC override snapshot.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("failed to begin permission snapshot: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			r.log.Warn("failed to rollback permission snapshot", "error", rollbackErr)
		}
	}()

	basePerms, isOwner, mask, err := r.resolveServerPermissions(ctx, tx, serverID, userID)
	if err != nil {
		return nil, err
	}
	if err := requireChannelsInServer(ctx, tx, serverID, channelIDs); err != nil {
		return nil, err
	}
	for _, channelID := range channelIDs {
		permsByChannel[channelID] = basePerms
	}
	// The bypass reads the RAW base; each channel is masked once, at the exit.
	if !isOwner && !basePerms.Has(PermAdministrator) {
		if err := r.applyBatchedChannelOverrides(ctx, tx, channelIDs, serverID, userID, basePerms, permsByChannel); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit permission snapshot: %w", err)
	}
	for channelID, raw := range permsByChannel {
		permsByChannel[channelID] = mask.Apply(raw)
	}
	return permsByChannel, nil
}

func (r *Resolver) applyBatchedChannelOverrides(
	ctx context.Context, tx *sql.Tx, channelIDs []string, serverID, userID string,
	basePerms Permission, permsByChannel map[string]Permission,
) error {
	rows, err := tx.QueryContext(ctx, batchChannelOverrideQuery, pq.Array(channelIDs), serverID, userID)
	if err != nil {
		return fmt.Errorf("failed to resolve channel overrides: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			r.log.Warn("failed to close permission override rows", "error", closeErr)
		}
	}()

	for rows.Next() {
		var channelID string
		var roleAllow, roleDeny, userAllow, userDeny int64
		if err := rows.Scan(&channelID, &roleAllow, &roleDeny, &userAllow, &userDeny); err != nil {
			return fmt.Errorf("scan channel override row: %w", err)
		}
		perms := basePerms
		perms |= Permission(roleAllow)
		perms &^= Permission(roleDeny)
		perms |= Permission(userAllow)
		perms &^= Permission(userDeny)
		permsByChannel[channelID] = perms
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate channel override rows: %w", err)
	}
	return nil
}

// computeEffectivePermissions implements the two-layer permission resolution model:
// 1. RBAC: OR together permissions from all user's roles
// 2. SBAC: Apply channel-specific overrides (deny > allow)
//
// and then the MFA mask, once, at its single exit (#3453). Every branch above
// the exit — the owner short-circuit and applyChannelOverrides' Administrator
// bypass — reads the raw value.
func (r *Resolver) computeEffectivePermissions(ctx context.Context, serverID, userID, channelID string) (Permission, error) {
	basePerms, isOwner, mask, err := r.resolveServerPermissionsFresh(ctx, serverID, userID)
	if err != nil {
		return 0, err
	}

	raw := basePerms
	if channelID != "" {
		// Before the owner short-circuit, which would otherwise return the
		// owner's full set for a channel of any server.
		if err := requireChannelsInServer(ctx, r.db, serverID, []string{channelID}); err != nil {
			return 0, err
		}
		// The owner bypasses the SBAC layer; everyone else gets overrides.
		if !isOwner {
			raw, err = r.applyChannelOverrides(ctx, channelID, userID, basePerms)
			if err != nil {
				return 0, fmt.Errorf("failed to apply channel overrides: %w", err)
			}
		}
	}
	return mask.Apply(raw), nil
}

// resolveServerPermissionsFresh checks current membership and resolves the
// server-level RBAC bitfield without consulting the cache.
func (r *Resolver) resolveServerPermissionsFresh(ctx context.Context, serverID, userID string) (Permission, bool, MFAMask, error) {
	return r.resolveServerPermissions(ctx, r.db, serverID, userID)
}

// resolveServerPermissions returns the member's RAW server-scope permissions,
// whether they own the server, and the MFA mask their entry point applies at
// its exit. It never masks: the caller's bypass decisions read the raw value.
//
// The enforcement flag rides the owner query, so a server that does not
// enforce costs exactly the statements it did before #3453; MaskFor then runs
// on the SAME querier, which is the caller's transaction or snapshot.
func (r *Resolver) resolveServerPermissions(ctx context.Context, db rowQuerier, serverID, userID string) (Permission, bool, MFAMask, error) {
	// Verify server membership.
	var isMember bool
	memberQuery := `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`
	if err := db.QueryRowContext(ctx, memberQuery, serverID, userID).Scan(&isMember); err != nil {
		return 0, false, MFAMask{}, fmt.Errorf("failed to check membership: %w", err)
	}
	if !isMember {
		return 0, false, MFAMask{}, ErrNotMember
	}

	// Server owner bypasses RBAC and SBAC.
	var ownerID string
	var enforcing bool
	ownerQuery := `SELECT owner_id, enforce_mfa_dangerous_actions FROM servers WHERE id = $1`
	if err := db.QueryRowContext(ctx, ownerQuery, serverID).Scan(&ownerID, &enforcing); err != nil {
		return 0, false, MFAMask{}, fmt.Errorf("failed to fetch server owner: %w", err)
	}
	mask, err := MaskFor(ctx, db, userID, enforcing)
	if err != nil {
		return 0, false, MFAMask{}, err
	}
	if ownerID == userID {
		// Owner gets all permissions — immune to channel overrides
		// Owner bypasses SBAC layer entirely (cannot be restricted per-channel)
		return OwnerPermissions, true, mask, nil
	}

	// Compute base permissions from roles (OR all role permissions together).
	basePerms, err := RawRolePermissions(ctx, db, serverID, userID)
	if err != nil {
		return 0, false, MFAMask{}, fmt.Errorf("failed to compute role permissions: %w", err)
	}
	return basePerms, false, mask, nil
}

// computeRolePermissions is RawRolePermissions; see that function's comment.
func (r *Resolver) computeRolePermissions(ctx context.Context, db rowQuerier, serverID, userID string) (Permission, error) {
	return RawRolePermissions(ctx, db, serverID, userID)
}

// RawRolePermissions is the server-qualified BIT_OR of userID's role
// permissions in serverID: RAW, with no owner short-circuit and no MFA mask.
// It is the one copy of that aggregate for callers outside this package that
// need raw bit 62 — Administrator identity is raw, never masked (#3453 I5).
//
// NOTE: this server-scope derivation is MIRRORED in raw SQL by
// servers.ListServers (internal/servers/handlers.go), which inlines the same
// owner short-circuit and BIT_OR to avoid N round-trips on a login-path query.
// The two are byte-equivalent today and nothing enforces that. If you add a
// term here — a member-timeout gate, a server-level SBAC tier, an epoch fence —
// add it there too, or the API will report permissions the enforcer does not
// grant. That copy fails in the PERMISSIVE direction for the UI.
//
// #2869 is the first change to test that warning, and it held: the server
// predicate below was added to BOTH copies in the same commit. Do not treat the
// mirror as documentation — it is a second enforcement surface.
//
// #3453 adds a term the mirror must also carry, and it does not live in this
// function: the MFA mask, which every resolver entry point applies to its
// result (MFAMask.Apply). ListServers must select
// servers.enforce_mfa_dangerous_actions and, when any row enforces, apply
// MaskFor's mask to each row in Go, failing closed to the unenrolled mask when
// enrollment cannot be read. Never mirror the mask's arithmetic in SQL: a
// second copy of EXPAND is a second place for it to drift.
//
// Every member_roles read in this file that turns a role_id into authority joins
// roles with `AND r.server_id = mr.server_id` (#2869) -- including the user_roles
// lists matched against override target_id, which need no column from roles and
// join it for the predicate alone, because mr.server_id names the membership's
// server, not the role's. Do NOT delete one as redundant now that migration
// 000144's composite FK makes a cross-server row unrepresentable: the predicate is
// what keeps a read correct for a row that reaches the table by a route the FK
// does not cover -- a replica running ahead of the migration, a restore from a
// pre-000144 dump, or direct database access. It costs one comparison on a row
// the join already fetched. cross_server_row_test.go plants such a row and asks
// every entry point. This aggregate feeds authorizeRoleMutationTx's actorPerms, so
// a foreign role's bits landing here open the escalation guard rather than merely
// widening a read.
func RawRolePermissions(ctx context.Context, db rowQuerier, serverID, userID string) (Permission, error) {
	query := `
		SELECT COALESCE(BIT_OR(r.permissions), 0) AS total_permissions
		FROM member_roles mr
		INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = mr.server_id
		WHERE mr.server_id = $1 AND mr.user_id = $2
	`

	var totalPerms int64
	if err := db.QueryRowContext(ctx, query, serverID, userID).Scan(&totalPerms); err != nil {
		// COALESCE(BIT_OR(...), 0) always returns a row, so ErrNoRows is unreachable.
		// Any error here is a genuine database failure.
		return 0, err
	}

	return Permission(totalPerms), nil
}

// applyChannelOverrides applies channel-specific permission overrides (SBAC layer)
// to the RAW base; its Administrator bypass is therefore driven by raw bit 62.
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
		          INNER JOIN roles r ON r.id = mr.role_id AND r.server_id = mr.server_id
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

// InvalidateUser clears cached server and channel permissions for one user.
func (r *Resolver) InvalidateUser(ctx context.Context, serverID, userID string) error {
	return r.cache.Invalidate(ctx, serverID, userID)
}

// BumpUserPermissionGeneration makes every cached permission of userID, on
// every server, miss on its next read. Call it AFTER the commit that changed
// one of the user's permission inputs (an MFA factor, #3453): bumping before
// the commit lets a compute that reads the pre-commit state publish under the
// new generation.
//
// It retries the bump once, then falls back to deleting the user's entries.
// It returns an error only when all three fail; the caller logs it. The
// fallback leaves a residual the bump does not (spec RS2): a compute already
// in flight can still publish under the unchanged generation.
func (r *Resolver) BumpUserPermissionGeneration(ctx context.Context, userID string) error {
	if r.cache == nil {
		return nil // nothing is cached, so nothing can be stale
	}
	return bumpWithFallback(ctx, userID, r.cache.BumpUser, r.cache.InvalidateUser)
}

// BumpServerPermissionGeneration makes every cached permission on serverID
// miss on its next read. Call it AFTER the commit that changed a server-wide
// input (the MFA enforcement flag, #3453). Same retry, fallback and error
// contract as BumpUserPermissionGeneration.
func (r *Resolver) BumpServerPermissionGeneration(ctx context.Context, serverID string) error {
	if r.cache == nil {
		return nil // nothing is cached, so nothing can be stale
	}
	return bumpWithFallback(ctx, serverID, r.cache.BumpServer, r.cache.InvalidateServer)
}

// permissionBumpTimeout bounds one bump with its retry and fallback.
const permissionBumpTimeout = 10 * time.Second

// bumpWithFallback runs detached from the caller's cancellation: the bump is a
// post-commit obligation, and a client that hangs up must not leave the cache
// serving the state that commit replaced. Detached is not unbounded — it gets
// its own deadline (the CompleteChannelAuthorityMutationWithRotations shape).
func bumpWithFallback(ctx context.Context, id string, bump, fallback func(context.Context, string) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), permissionBumpTimeout)
	defer cancel()
	err := bump(ctx, id)
	if err == nil {
		return nil
	}
	if err = bump(ctx, id); err == nil {
		return nil
	}
	if fallbackErr := fallback(ctx, id); fallbackErr != nil {
		return fmt.Errorf("bump permission generation: %w", errors.Join(err, fallbackErr))
	}
	return nil
}

// sbacChannelOverrideColumns is the SELECT column list, shared verbatim by the
// batched resolver and visibility queries, that aggregates the SBAC role/user
// allow/deny bitfields for a channel. Callers derive effective permissions from
// these four columns using the identical
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

const batchChannelOverrideQuery = `
	WITH user_roles AS (
		SELECT mr.role_id
		FROM member_roles mr
		INNER JOIN roles r ON r.id = mr.role_id AND r.server_id = mr.server_id
		WHERE mr.server_id = $2 AND mr.user_id = $3
	)
	SELECT cpo.channel_id,` + sbacChannelOverrideColumns + `
	FROM channel_permission_overrides cpo
	WHERE cpo.channel_id = ANY($1::uuid[])
	  -- A channel from another server must not borrow $2's role overrides (#2869).
	  AND cpo.channel_id IN (SELECT id FROM channels WHERE server_id = $2)
	  AND (
	      (cpo.target_type = 'role' AND cpo.target_id IN (SELECT role_id FROM user_roles))
	      OR (cpo.target_type = 'user' AND cpo.target_id = $3)
	  )
	GROUP BY cpo.channel_id`

// GetVisibleChannelIDs returns a list of channel IDs that the user can view.
// Visibility is type-aware: text/bulletin channels require PermViewTextChannels,
// voice channels require PermViewVoiceChannels. This allows RBAC roles (and SBAC
// overrides) to independently control visibility for each channel type.
//
// Optimized to resolve visibility for ALL channels in a single SQL query,
// replicating the RBAC+SBAC resolution logic (base | role_allow &^ role_deny
// | user_allow &^ user_deny) in SQL rather than looping per-channel.
func (r *Resolver) GetVisibleChannelIDs(ctx context.Context, serverID, userID string) ([]string, error) {
	return r.visibleChannelIDs(ctx, serverID, userID, 0)
}

// GetReadableChannelIDs returns the channel IDs in one server that the user can
// both SEE and read message history in: the type-appropriate view bit AND
// PermReadMessageHistory.
//
// The two bits are separately deniable and answer different questions. The view
// bit answers "does this channel exist for you" — the channel list — and a
// history denial must never take a channel out of it. PermReadMessageHistory
// answers "may you read what was said in it", and it is what every path that
// returns message content already enforces (messages.checkChannelAccess, message
// reactions, attachment delivery).
//
// Anything DERIVED from message content must resolve its channels through this
// method rather than GetVisibleChannelIDs. An unread count is such a derivation:
// polled, it is a live measure of message volume and timing, so serving one for a
// view-only channel hands a member a side channel on a channel they are forbidden
// to read (CWE-863).
func (r *Resolver) GetReadableChannelIDs(ctx context.Context, serverID, userID string) ([]string, error) {
	return r.visibleChannelIDs(ctx, serverID, userID, PermReadMessageHistory)
}

// visibleChannelIDs is the shared implementation of GetVisibleChannelIDs and
// GetReadableChannelIDs. alsoRequired is a bitfield of permissions a channel must
// grant IN ADDITION to the type-appropriate view bit; 0 means plain visibility.
//
// alsoRequired is folded into the per-type required mask in Go rather than added
// as a second SQL predicate, so the query keeps one shape for every caller: a
// channel qualifies when its effective permissions contain EVERY bit of the
// required mask. With alsoRequired == 0 the mask is a single view bit and the test
// is equivalent to the "view bit is set" test it generalizes.
func (r *Resolver) visibleChannelIDs(ctx context.Context, serverID, userID string, alsoRequired Permission) ([]string, error) {
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

	// Fast path: server owner sees everything.
	//
	// The owner bypasses alsoRequired too, and must: resolveServerPermissions
	// short-circuits an owner to OwnerPermissions before SBAC is consulted, so the
	// per-request check an extra bit stands in for (HasPermission) grants it
	// regardless of any override. Withholding it here would only make this
	// resolver disagree with the endpoints it feeds.
	var ownerID string
	if err := r.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		return nil, fmt.Errorf("failed to fetch server owner: %w", err)
	}
	if ownerID == userID {
		return r.getAllChannelIDs(ctx, serverID)
	}

	// Compute base role permissions (single query, same as computeRolePermissions,
	// including its server-qualifying join predicate -- see that function's comment)
	var basePerms int64
	roleQuery := `
		SELECT COALESCE(BIT_OR(r.permissions), 0)
		FROM member_roles mr
		INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = mr.server_id
		WHERE mr.server_id = $1 AND mr.user_id = $2
	`
	if err := r.db.QueryRowContext(ctx, roleQuery, serverID, userID).Scan(&basePerms); err != nil {
		return nil, fmt.Errorf("failed to compute role permissions: %w", err)
	}

	// Fast path: administrators see everything (SBAC cannot restrict them), and
	// for the same reason they bypass alsoRequired — applyChannelOverrides returns
	// early for an administrator and Permission.Has reports true for every bit once
	// PermAdministrator is set.
	if Permission(basePerms).Has(PermAdministrator) {
		return r.getAllChannelIDs(ctx, serverID)
	}

	// Single query: for each channel, compute effective permissions with SBAC
	// overrides, then check the appropriate view permission bit based on channel type.
	//
	// The SBAC resolution mirrors applyChannelOverrides exactly:
	//   effective = ((base | role_allow) & ~role_deny | user_allow) & ~user_deny
	//
	// Required-mask mapping ($4/$5 are the type-appropriate view bit OR'd with
	// alsoRequired, combined in Go so the SQL carries one predicate shape):
	//   text, bulletin → PermViewTextChannels | alsoRequired ($4)
	//   voice          → PermViewVoiceChannels | alsoRequired ($5)
	query := `
		WITH user_roles AS (
			SELECT mr.role_id
			FROM member_roles mr
			INNER JOIN roles r ON r.id = mr.role_id AND r.server_id = mr.server_id
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
		    -- Keep only the bits this channel type requires...
		    CASE WHEN c.type = 'voice' THEN $5::bigint ELSE $4::bigint END
		    -- ...and require ALL of them, not merely one: the mask carries the
		    -- type-appropriate view bit plus any caller-supplied extra bits, so a
		    -- channel granting the view bit but not the extra one is excluded.
		    = CASE WHEN c.type = 'voice' THEN $5::bigint ELSE $4::bigint END
		  )
	`

	return r.queryChannelIDs(ctx, "failed to query visible channels", query,
		serverID, userID, basePerms,
		int64(PermViewTextChannels|alsoRequired), int64(PermViewVoiceChannels|alsoRequired))
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
	return r.allVisibleChannelIDs(ctx, userID, 0)
}

// GetAllReadableChannelIDs is the cross-server counterpart to
// GetReadableChannelIDs: every channel the user can both see AND read message
// history in, across all their servers, in one round-trip. See
// GetReadableChannelIDs for why the two permissions must not be conflated.
func (r *Resolver) GetAllReadableChannelIDs(ctx context.Context, userID string) ([]string, error) {
	return r.allVisibleChannelIDs(ctx, userID, PermReadMessageHistory)
}

// allVisibleChannelIDs is the shared implementation of GetAllVisibleChannelIDs
// and GetAllReadableChannelIDs. alsoRequired carries the same meaning as in
// visibleChannelIDs — extra bits a channel must grant on top of the
// type-appropriate view bit — and is folded into the per-type required mask in
// Go, so the two resolvers stay predicate-for-predicate identical.
func (r *Resolver) allVisibleChannelIDs(ctx context.Context, userID string, alsoRequired Permission) ([]string, error) {
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
			INNER JOIN roles r ON r.id = mr.role_id AND r.server_id = mr.server_id
			WHERE mr.user_id = $1
		),
		-- This CTE spans EVERY server the user is in and groups by mr.server_id, so
		-- an unqualified join would OR a foreign role's bits into the wrong server's
		-- row -- the multi-server analogue of the scoping the channel_overrides CTE
		-- below already documents for role overrides (#2869).
		base_perms AS (
			SELECT mr.server_id,
				COALESCE(BIT_OR(r.permissions), 0) AS perms
			FROM member_roles mr
			INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = mr.server_id
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
			-- Owner fast path: owner sees every channel. As in visibleChannelIDs this
			-- bypasses alsoRequired, because resolveServerPermissions grants an owner
			-- OwnerPermissions before SBAC is consulted.
			s.owner_id = $1
			-- Administrator fast path: SBAC cannot restrict administrators, and
			-- Permission.Has reports true for every bit once PermAdministrator is set.
			OR (COALESCE(bp.perms, 0) & $2::bigint) != 0
			-- Otherwise: per-channel SBAC bitfield math against the required mask,
			-- which must be satisfied in FULL ($3/$4 carry the type-appropriate view
			-- bit OR'd with alsoRequired).
			OR (
				(
					(
						(COALESCE(bp.perms, 0) | COALESCE(co.role_allow, 0)) & ~COALESCE(co.role_deny, 0)
						| COALESCE(co.user_allow, 0)
					) & ~COALESCE(co.user_deny, 0)
				) &
				CASE WHEN c.type = 'voice' THEN $4::bigint ELSE $3::bigint END
				= CASE WHEN c.type = 'voice' THEN $4::bigint ELSE $3::bigint END
			)
	`

	return r.queryChannelIDs(ctx, "failed to query all visible channels", query,
		userID, int64(PermAdministrator),
		int64(PermViewTextChannels|alsoRequired), int64(PermViewVoiceChannels|alsoRequired))
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
//   - Server owner always outranks everyone (owner gets permissions via owner-id bypass, not via roles)
//   - Users with PermAdministrator outrank every non-owner, other administrators
//     included - position is not consulted, so a lower-positioned administrator
//     may moderate a higher one (recorded as intended in #3407)
func (r *Resolver) CheckHierarchy(ctx context.Context, serverID, actorID, targetID string) error {
	return r.checkHierarchy(ctx, r.db, serverID, actorID, targetID)
}

// CheckHierarchyTx verifies hierarchy through a caller-supplied transaction.
// It reads and publishes no cache entry, so an authorization decision shares
// the caller's serialization with the write it permits.
func (r *Resolver) CheckHierarchyTx(ctx context.Context, q rowQuerier, serverID, actorID, targetID string) error {
	return r.checkHierarchy(ctx, q, serverID, actorID, targetID)
}

func (r *Resolver) checkHierarchy(ctx context.Context, q rowQuerier, serverID, actorID, targetID string) error {
	// Server owner bypasses hierarchy — owner can moderate anyone
	var ownerID string
	if err := q.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID); err != nil {
		return fmt.Errorf(errMsgHierarchyCheckFailed, err)
	}
	if actorID == ownerID {
		return nil // Owner outranks everyone
	}

	// Check if actor has PermAdministrator (bypasses position-based hierarchy)
	actorPerms, err := r.computeRolePermissions(ctx, q, serverID, actorID)
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
			INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = mr.server_id
			WHERE mr.server_id = $1 AND mr.user_id = $2
		),
		target_max AS (
			SELECT COALESCE(MAX(r.position), 0) AS pos
			FROM member_roles mr
			INNER JOIN roles r ON mr.role_id = r.id AND r.server_id = mr.server_id
			WHERE mr.server_id = $1 AND mr.user_id = $3
		)
		SELECT actor_max.pos > target_max.pos AS can_modify
		FROM actor_max, target_max
	`

	var canModify bool
	if err := q.QueryRowContext(ctx, query, serverID, actorID, targetID).Scan(&canModify); err != nil {
		return fmt.Errorf(errMsgHierarchyCheckFailed, err)
	}

	if !canModify {
		return ErrHierarchyViolation
	}

	return nil
}
