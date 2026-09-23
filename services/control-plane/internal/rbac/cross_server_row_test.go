package rbac_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Migration 000144 makes a cross-server member_roles row unrepresentable, and
// every reader that turns a role_id into authority keeps a same-server predicate
// anyway (#2869), for rows that reach the table by a route the FK does not
// cover: a replica ahead of the migration, a pre-000144 restore, direct SQL.
//
// This test is what enforces that rule. A count of qualified sites was tried
// first and was wrong twice, and a text scan of the SQL cannot see the second
// reader shape -- a user_roles CTE that matches override target_id against a
// bare role_id list and never touches roles at all. That shape leaked a hidden
// channel through both visible-channel resolvers after every join was already
// qualified, and was found only by planting a row and asking.
//
// So: plant the row past the FK, then ask every entry point. Each negative is
// paired with a same-server CONTROL through the same path that must be
// honoured, so no case can pass by resolving nothing for everyone.
func TestCrossServerMemberRoleRowGrantsNothing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	res := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))

	ownerA := ts.CreateTestUser(t, "xsrvowner")
	holder := ts.CreateTestUser(t, "xsrvholder")
	serverA := ts.CreateTestServer(t, ownerA.ID, "Cross-server victim")
	serverB := ts.CreateTestServer(t, holder.ID, "Cross-server source")

	// The holder joins A with exactly ONE role and no @all, so every value the
	// resolver reports for them in A is attributable to that role or to nothing.
	// ownPerms is non-zero so the server-aggregate case has something to equal.
	ownPerms := rbac.PermKick | rbac.PermManageRolesAssign | rbac.PermManageRoles
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverA, holder.ID)
	require.NoError(t, err)
	ownRole := ts.CreateTestRole(t, serverA, "Own", 10, int64(ownPerms))
	ts.AssignRoleToUser(t, serverA, holder.ID, ownRole)

	// Positive, so the bitfield CHECKs are not what keeps it out, and carrying
	// PermAdministrator plus everything the cases below would detect.
	foreignPerms := rbac.PermAdministrator | rbac.PermViewTextChannels | rbac.PermBan | rbac.PermManageRoles
	foreignRole := ts.CreateTestRole(t, serverB, "Foreign", 999, int64(foreignPerms))

	// Planted past the FK the way a pre-000144 replica, a restore or direct SQL
	// would. SET LOCAL confines replica mode to this transaction, so it cannot
	// leak into a pooled connection the rest of the test reuses.
	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'replica'`)
	if err == nil {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
			serverA, holder.ID, foreignRole)
	}
	if err != nil {
		_ = tx.Rollback()
		require.NoError(t, err, "planting past the FK needs a superuser test role; skipping would hide this test")
	}
	require.NoError(t, tx.Commit())

	// Liveness: read WITHOUT the server predicate, the planted row imports
	// PermAdministrator. If it did not, every negative below would pass for the
	// wrong reason.
	var unqualified int64
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT COALESCE(BIT_OR(r.permissions), 0)
		FROM member_roles mr JOIN roles r ON r.id = mr.role_id
		WHERE mr.server_id = $1 AND mr.user_id = $2`, serverA, holder.ID).Scan(&unqualified))
	require.NotZero(t, unqualified&int64(rbac.PermAdministrator), "the planted row must be live")

	t.Run("server aggregate holds only the same-server role's bits", func(t *testing.T) {
		p, err := res.GetEffectivePermissions(ctx, serverA, holder.ID, "")
		require.NoError(t, err)
		assert.Equal(t, ownPerms, p)
	})

	t.Run("hierarchy ceiling ignores the foreign role's position", func(t *testing.T) {
		above := ts.CreateTestUser(t, "xsrvabove")
		ts.AddMemberToServer(t, serverA, above.ID, "member")
		ts.AssignRoleToUser(t, serverA, above.ID, ts.CreateTestRole(t, serverA, "Above", 500, 0))
		assert.ErrorIs(t, res.CheckHierarchy(ctx, serverA, holder.ID, above.ID), rbac.ErrHierarchyViolation,
			"position 999 must not lift the holder over a position-500 member")

		below := ts.CreateTestUser(t, "xsrvbelow")
		ts.AddMemberToServer(t, serverA, below.ID, "member")
		assert.NoError(t, res.CheckHierarchy(ctx, serverA, holder.ID, below.ID),
			"CONTROL: position 10 outranks an @all-only member")
		assert.NoError(t, res.CheckHierarchy(ctx, serverA, above.ID, holder.ID),
			"target side: the foreign 999 must not make the holder immune to a position-500 member")
	})

	t.Run("ListRoles reports the same-server ceiling", func(t *testing.T) {
		viewer, present := listRolesViewer(t, ts, serverA, holder.AccessToken)
		require.True(t, present, "the holder has PermManageRoles, so a viewer block is expected")
		assert.Equal(t, 10, requireBoundedCeiling(t, viewer),
			"actorCeilingSelect feeds both this report and the reorder guard")
	})

	// target_id has no FK, so an override in A may legitimately name B's role.
	// Only a reader that trusts mr.server_id as the role's server honours it.
	hidden := ts.CreateTestChannel(t, serverA, "xsrv-hidden")
	visible := ts.CreateTestChannel(t, serverA, "xsrv-control")
	ts.CreateChannelOverride(t, hidden, "role", foreignRole, int64(rbac.PermViewTextChannels), 0)
	ts.CreateChannelOverride(t, visible, "role", ownRole, int64(rbac.PermViewTextChannels), 0)

	t.Run("GetVisibleChannelIDs", func(t *testing.T) {
		ids, err := res.GetVisibleChannelIDs(ctx, serverA, holder.ID)
		require.NoError(t, err)
		assert.Contains(t, ids, visible)
		assert.NotContains(t, ids, hidden)
	})

	t.Run("GetAllVisibleChannelIDs", func(t *testing.T) {
		ids, err := res.GetAllVisibleChannelIDs(ctx, holder.ID)
		require.NoError(t, err)
		assert.Contains(t, ids, visible)
		assert.NotContains(t, ids, hidden)
	})

	t.Run("HasPermission at channel scope", func(t *testing.T) {
		ok, err := res.HasPermission(ctx, serverA, holder.ID, visible, rbac.PermViewTextChannels)
		require.NoError(t, err)
		assert.True(t, ok)
		ok, err = res.HasPermission(ctx, serverA, holder.ID, hidden, rbac.PermViewTextChannels)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("ResolveEffectivePermissionsForChannelsFresh", func(t *testing.T) {
		perms, err := res.ResolveEffectivePermissionsForChannelsFresh(ctx, serverA, holder.ID, []string{hidden, visible})
		require.NoError(t, err)
		assert.True(t, perms[visible].Has(rbac.PermViewTextChannels))
		assert.False(t, perms[hidden].Has(rbac.PermViewTextChannels))
	})

	t.Run("FilterVisibleUserIDsForChannelFresh", func(t *testing.T) {
		ids, err := res.FilterVisibleUserIDsForChannelFresh(ctx, serverA, visible, []string{holder.ID})
		require.NoError(t, err)
		assert.Contains(t, ids, holder.ID)
		ids, err = res.FilterVisibleUserIDsForChannelFresh(ctx, serverA, hidden, []string{holder.ID})
		require.NoError(t, err)
		assert.NotContains(t, ids, holder.ID)
	})

	t.Run("ListServers advertises only same-server bits", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(holder.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)
		var body struct {
			Servers []struct {
				ID          string `json:"id"`
				Permissions string `json:"permissions"`
			} `json:"servers"`
		}
		testhelpers.ParseJSON(t, w, &body)
		for _, s := range body.Servers {
			if s.ID == serverA {
				assert.Equal(t, strconv.FormatInt(int64(ownPerms), 10), s.Permissions)
				return
			}
		}
		t.Fatalf("server A missing from the holder's server list")
	})

	t.Run("member list shows only same-server roles", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/servers/"+serverA+"/members", nil, testhelpers.AuthHeaders(ownerA.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), ownRole)
		assert.NotContains(t, w.Body.String(), foreignRole)
	})

	t.Run("role guard evaluates the holder without the foreign row", func(t *testing.T) {
		// The holder has PermManageRolesAssign, so the route admits both requests
		// and the guard is what decides. With the foreign row counted, position
		// 999 and its bits would clear both checks for the position-900 prize.
		assign := func(roleID string) (int, string) {
			w := ts.DoRequest("POST", fmt.Sprintf("/api/v1/servers/%s/members/%s/roles", serverA, holder.ID),
				map[string]string{"role_id": roleID}, testhelpers.AuthHeaders(holder.AccessToken))
			return w.Code, w.Body.String()
		}
		lower := ts.CreateTestRole(t, serverA, "Lower", 5, int64(rbac.PermKick))
		code, _ := assign(lower)
		assert.Equal(t, http.StatusOK, code, "CONTROL: a lower, subset role is assignable")

		// Prize bits are a subset of the holder's own, so only the hierarchy half
		// can deny it; the message pins that it did.
		prize := ts.CreateTestRole(t, serverA, "Prize", 900, int64(rbac.PermKick))
		code, body := assign(prize)
		assert.Equal(t, http.StatusForbidden, code)
		assert.Contains(t, body, "equal or higher position than your own",
			"the guard must see ceiling 10, not the foreign role's 999")

		// Below the holder, so the hierarchy half passes and only the subset check
		// can refuse; PermBan is held by the foreign role and not by the holder.
		// The pre-check refuses this first. The in-transaction check behind it
		// gives the same answer, so this pins the pair; each join is pinned on
		// its own elsewhere (server aggregate above, and the r3 pre-check test).
		foreignBit := ts.CreateTestRole(t, serverA, "Foreign Bit", 5, int64(rbac.PermBan))
		code, body = assign(foreignBit)
		assert.Equal(t, http.StatusForbidden, code)
		assert.Contains(t, body, "Cannot grant permissions you do not have",
			"the escalation check must not count the foreign role's PermBan")
	})

	// Last: resolveNewRolePosition shifts existing positions.
	t.Run("CreateRole places the new role below the holder's same-server ceiling", func(t *testing.T) {
		w := ts.DoRequest("POST", "/api/v1/servers/"+serverA+"/roles",
			map[string]interface{}{"name": "Created", "permissions": "0"}, testhelpers.AuthHeaders(holder.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var body struct {
			Role struct {
				Position int `json:"position"`
			} `json:"role"`
		}
		testhelpers.ParseJSON(t, w, &body)
		var ownPos int
		require.NoError(t, ts.DB.QueryRowContext(ctx, `SELECT position FROM roles WHERE id = $1`, ownRole).Scan(&ownPos))
		assert.Less(t, body.Role.Position, ownPos, "with the foreign 999 counted, the new role would land near 998")
	})
}
