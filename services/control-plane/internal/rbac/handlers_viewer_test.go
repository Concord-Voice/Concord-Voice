package rbac_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// ListRoles — the self-scoped `viewer` block
//
// The block exists so a client can build a reorder payload the server will
// accept without enumerating the membership. That is only true if the ceiling
// REPORTED here is the same number evaluateReorderGuards ENFORCES, which is
// why both read it from one shared SQL expression (actorCeilingSelect) and why
// TestListRoles_Viewer_CeilingPredictsGuard5Boundary exists below.
// ─────────────────────────────────────────────────────────────────────────────

// defaultRoleOmittedOffset mirrors evaluateReorderGuards' positionOffset for a
// payload that leaves the default role out: position 0 is reserved for @all, so
// every assigned position shifts up by one.
const defaultRoleOmittedOffset = 1

// listRolesViewer performs GET /roles as token and returns the viewer block.
// Presence is returned rather than asserted, because ABSENCE is a meaningful
// wire state — the client reads it as "unknown" and goes read-only.
func listRolesViewer(t *testing.T, ts *testhelpers.TestServer, serverID, token string) (map[string]interface{}, bool) {
	t.Helper()
	w := ts.DoRequest("GET", rolesPath(serverID), nil, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusOK, w.Code, "ListRoles body: %s", w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	raw, present := body["viewer"]
	if !present {
		return nil, false
	}
	require.NotNil(t, raw, "viewer must never be emitted as null; absence is the unknown signal")
	viewer, isObject := raw.(map[string]interface{})
	require.True(t, isObject, "viewer must be an object, got %T", raw)
	return viewer, true
}

// requireBoundedCeiling asserts the viewer is the bounded variant and returns
// the reported ceiling.
func requireBoundedCeiling(t *testing.T, viewer map[string]interface{}) int {
	t.Helper()
	require.Equal(t, "bounded", viewer["kind"])
	raw, present := viewer["max_role_position"]
	require.True(t, present, "a bounded viewer must carry max_role_position")
	ceiling, isNumber := raw.(float64)
	require.True(t, isNumber, "max_role_position must be a number, got %T", raw)
	return int(ceiling)
}

// guard5Refuses replays the arithmetic of guard 5 in evaluateReorderGuards
// against a ceiling the CLIENT was told, which is the whole point: if the
// reported ceiling and the enforced one ever diverge, this prediction stops
// matching the server's answer.
func guard5Refuses(payloadLen, offset, ceiling int) bool {
	return payloadLen-1+offset >= ceiling
}

func TestListRoles_Viewer_Owner(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	viewer, present := listRolesViewer(t, ts, serverID, owner.AccessToken)
	require.True(t, present, "the owner must get a viewer block")

	assert.Equal(t, "owner", viewer["kind"])
	_, hasCeiling := viewer["max_role_position"]
	assert.False(t, hasCeiling,
		"the owner bypasses the hierarchy guards, so reporting a ceiling would make a client hide roles the owner can in fact move")
}

// TestListRoles_Viewer_BoundedMember also pins SELF-SCOPING: the ceiling is the
// caller's own, not the highest position on the server. A role held by someone
// else, above the caller, must not raise the number the caller is told.
func TestListRoles_Viewer_BoundedMember(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	grantPermToUser(t, ts, serverID, member.ID, 4, 0)
	grantPermToUser(t, ts, serverID, member.ID, 7, int64(rbac.PermManageRoles))
	// Held by the OWNER, well above the member. Must not leak into the
	// member's own ceiling.
	grantPermToUser(t, ts, serverID, owner.ID, 30, 0)

	viewer, present := listRolesViewer(t, ts, serverID, member.AccessToken)
	require.True(t, present, "a member must get a viewer block")

	assert.Equal(t, 7, requireBoundedCeiling(t, viewer),
		"the ceiling is MAX over the caller's own roles — not their last-granted role, and not another member's")
}

// TestListRoles_Viewer_MemberHoldingOnlyDefaultRole_IsZero covers the floor.
// @all sits at position 0, so a member holding nothing else has a ceiling of 0
// — a real answer that must be EMITTED, not omitted as a zero value. Omitting
// it would read as "unknown" and is a different statement entirely.
func TestListRoles_Viewer_MemberHoldingOnlyDefaultRole_IsZero(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)

	viewer, present := listRolesViewer(t, ts, serverID, member.AccessToken)
	require.True(t, present, "a base member must get a viewer block")

	assert.Equal(t, 0, requireBoundedCeiling(t, viewer))
}

// TestListRoles_Viewer_CeilingPredictsGuard5Boundary is the reason the shared
// actorCeilingSelect constant exists.
//
// It takes the ceiling the server REPORTED to the client, replays guard 5's
// arithmetic against it to predict a verdict, then asks the server for real —
// on BOTH sides of the boundary. Bracketing is what makes this an equality
// test rather than an inequality one:
//
//   - a reported ceiling HIGHER than the enforced one makes the at-the-boundary
//     payload predict "accepted" while the server refuses it;
//   - a reported ceiling LOWER makes the one-below payload predict "refused"
//     while the server accepts it.
//
// Either drift between the two copies of the expression fails this test.
func TestListRoles_Viewer_CeilingPredictsGuard5Boundary(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	grantPermToUser(t, ts, serverID, member.ID, 3, int64(rbac.PermManageRoles))

	roleA := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "CeilingA", 0)
	roleB := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "CeilingB", 0)
	roleC := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "CeilingC", 0)
	// CreateRole lands new roles at MAX+1, i.e. ABOVE their creator's actor.
	// Park them below the member so guard 4 is out of the picture and guard 5
	// is the only thing this test can trip.
	_, err := ts.DB.Exec(`UPDATE roles SET position = 1 WHERE id IN ($1, $2, $3)`, roleA, roleB, roleC)
	require.NoError(t, err)

	viewer, present := listRolesViewer(t, ts, serverID, member.AccessToken)
	require.True(t, present)
	ceiling := requireBoundedCeiling(t, viewer)
	require.Equal(t, 3, ceiling, "sanity: the reported ceiling is the granted role's position")

	// The default role is deliberately omitted from both payloads, so the
	// offset is 1 and the largest position either would assign is len-1+1.
	cases := []struct {
		name    string
		payload []string
	}{
		{"payload reaching the ceiling", []string{roleA, roleB, roleC}},
		{"payload stopping one below the ceiling", []string{roleA, roleB}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			predictedRefusal := guard5Refuses(len(tc.payload), defaultRoleOmittedOffset, ceiling)

			body := map[string]interface{}{"role_ids": tc.payload}
			w := ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))

			if predictedRefusal {
				require.Equal(t, http.StatusForbidden, w.Code,
					"the reported ceiling %d predicted a refusal the server did not make — reported and enforced ceilings have drifted; body: %s",
					ceiling, w.Body.String())

				var resp map[string]interface{}
				testhelpers.ParseJSON(t, w, &resp)
				assert.Equal(t, "Reorder would create roles at or above your position", resp["error"],
					"the refusal must come from guard 5, not from some other guard that happens to also 403")
				return
			}

			require.Equal(t, http.StatusOK, w.Code,
				"the reported ceiling %d predicted this payload was acceptable but the server refused it — reported and enforced ceilings have drifted; body: %s",
				ceiling, w.Body.String())
		})
	}
}

// TestListRoles_Viewer_CeilingPredictsGuard4Boundary brackets the other guard
// that consumes the same expression: a role sitting AT the reported ceiling is
// refused, the same role one position below is accepted.
func TestListRoles_Viewer_CeilingPredictsGuard4Boundary(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageRoles))
	target := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Guard4Target", 0)

	viewer, present := listRolesViewer(t, ts, serverID, member.AccessToken)
	require.True(t, present)
	ceiling := requireBoundedCeiling(t, viewer)
	require.Equal(t, 5, ceiling)

	body := map[string]interface{}{"role_ids": []string{target}}

	_, err := ts.DB.Exec(`UPDATE roles SET position = $1 WHERE id = $2`, ceiling, target)
	require.NoError(t, err)
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code,
		"a role AT the reported ceiling must be refused; body: %s", w.Body.String())

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Cannot reorder roles at or above your own position", resp["error"])

	_, err = ts.DB.Exec(`UPDATE roles SET position = $1 WHERE id = $2`, ceiling-1, target)
	require.NoError(t, err)
	w = ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code,
		"a role one position BELOW the reported ceiling must be accepted; body: %s", w.Body.String())
}
