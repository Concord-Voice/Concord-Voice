package servers_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// GET /api/v1/servers carries the caller's effective SERVER-scope permission
// bitfield for every row (#2372).
//
// The renderer's invite picker reads this field and nothing else for a server it
// is not currently viewing: `permissionStore` is populated only by MainView's
// `activeServerId` effect, and MainView is not mounted in the DM view where the
// picker lives. Before this field existed, the picker could see exactly one
// server. So a regression here is not a missing number in a JSON body — it is a
// user who cannot invite anyone to any server but the last one they opened.
//
// The three rows below are the three arms of `resolveServerPermissions` at
// server scope: owner short-circuit, BIT_OR over roles, and BIT_OR picking up a
// role the default one does not carry.
func TestListServersIncludesEffectivePermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "permlistowner")
	member := ts.CreateTestUser(t, "permlistmember")
	inviter := ts.CreateTestUser(t, "permlistinviter")

	serverID := ts.CreateTestServer(t, owner.ID, "Perm List Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ts.AddMemberToServer(t, serverID, inviter.ID, "member")

	// A second role carrying PermInvite, assigned to `inviter` alone.
	//
	// It exists because rbac.BasePermissions deliberately EXCLUDES PermInvite —
	// without a role like this, no ordinary member could ever hold the bit, and
	// the difference between "BIT_OR ran and found nothing extra" and "BIT_OR
	// never ran" would be invisible.
	roleID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'Inviter', 1, $3)`,
		roleID, serverID, int64(rbac.PermInvite),
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, inviter.ID, roleID,
	)
	require.NoError(t, err)

	permsFor := func(t *testing.T, user testhelpers.TestUser) int64 {
		t.Helper()
		w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		servers := testhelpers.JSONField[[]interface{}](t, body, "servers")
		require.Len(t, servers, 1)
		row := testhelpers.JSONElem[map[string]interface{}](t, servers, 0)

		// Asserted as a decimal STRING, not a JSON number. The bitfield reaches
		// bit 62, and a number silently loses precision above 2^53 — parsing it
		// here is what pins the encoding, so do not relax this to a numeric
		// comparison against the decoded value.
		raw := testhelpers.JSONField[string](t, row, "permissions")
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		require.NoError(t, parseErr, "permissions must be a decimal int64 string, got %q", raw)
		return parsed
	}

	assert.Equal(t, int64(rbac.OwnerPermissions), permsFor(t, owner),
		"the owner short-circuits to OwnerPermissions and never reaches the BIT_OR")

	memberPerms := permsFor(t, member)
	assert.Equal(t, int64(rbac.BasePermissions), memberPerms,
		"a plain member gets exactly the @all role's bitfield")
	assert.Zero(t, memberPerms&int64(rbac.PermInvite),
		"BasePermissions must not carry PermInvite — the invite picker's whole filter rests on this")

	inviterPerms := permsFor(t, inviter)
	assert.Equal(t, int64(rbac.BasePermissions|rbac.PermInvite), inviterPerms,
		"BIT_OR unions every assigned role, not just the default one")
}

// A user who is not a member sees no row at all, so there is no permissions
// value to leak. The membership filter predates this change; the assertion is
// here because the new column is the first thing in this response derived from
// the CALLER rather than from the server, which makes a widened WHERE clause a
// disclosure rather than merely a wrong list.
func TestListServersOmitsPermissionsForNonMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "permlistoutsideowner")
	stranger := ts.CreateTestUser(t, "permliststranger")
	ts.CreateTestServer(t, owner.ID, "Private Perm Server")

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(stranger.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Empty(t, testhelpers.JSONField[[]interface{}](t, body, "servers"))
}

// The COALESCE(..., 0) arm, which nothing covered.
//
// Every member in every other test here holds at least one role, because
// AddMemberToServer assigns the is_default role unconditionally — so BIT_OR
// never returns NULL and the COALESCE is never exercised. That matters more
// than a wrong bitfield would: BIT_OR over zero rows returns NULL, and the
// handler's scan target was a plain int64, so a NULL made rows.Scan error and
// the loop `continue` — dropping THE WHOLE SERVER from the response behind an
// HTTP 200, with only a log line. fetchServers commits a whole-array replace,
// so purgeMissingServerState would then tear down that server's channel state.
//
// The require.Len below is therefore the load-bearing assertion, not the
// permission value: it is what catches the row-drop.
func TestListServersReturnsZeroPermissionsForARolelessMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "permzeroowner")
	member := ts.CreateTestUser(t, "permzeromember")

	serverID := ts.CreateTestServer(t, owner.ID, "Perm Zero Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Strip every role, reaching the state AddMemberToServer cannot produce.
	_, err := ts.DB.Exec(
		`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2`,
		serverID, member.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := testhelpers.JSONField[[]interface{}](t, body, "servers")

	// The row-drop guard. A roleless member must still SEE the server.
	require.Len(t, servers, 1,
		"a roleless member must still receive the server row — a NULL permission "+
			"must never remove a server from the sidebar")

	row := testhelpers.JSONElem[map[string]interface{}](t, servers, 0)
	// "0", explicitly: distinguished from absent (JSONField fails on a missing
	// key) and from any non-zero arm.
	assert.Equal(t, "0", testhelpers.JSONField[string](t, row, "permissions"),
		"a member holding no roles resolves to exactly zero, which denies invite")
}

// The precision property the decimal-string encoding exists for.
//
// Asserted with PermAdministrator OR'd with the default role's bits, never with
// PermAdministrator alone: 1<<62 is a power of two and round-trips through a
// float64 exactly, so it would not catch a numeric encoding. The low bits are
// what make this bite — a JSON number rounds them away.
func TestListServersPreservesPermissionBitsAbove2Pow53(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "permhighowner")
	admin := ts.CreateTestUser(t, "permhighadmin")

	serverID := ts.CreateTestServer(t, owner.ID, "Perm High Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")

	roleID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'HighBit', 1, $3)`,
		roleID, serverID, int64(rbac.PermAdministrator),
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, admin.ID, roleID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := testhelpers.JSONField[[]interface{}](t, body, "servers")
	require.Len(t, servers, 1)
	row := testhelpers.JSONElem[map[string]interface{}](t, servers, 0)

	want := int64(rbac.BasePermissions | rbac.PermAdministrator)
	raw := testhelpers.JSONField[string](t, row, "permissions")

	// The hazard, stated rather than assumed: this value is NOT exactly
	// representable as a float64, so a numeric encoding loses the low bits.
	require.NotEqual(t, want, int64(float64(want)),
		"fixture must exceed float64's exact-integer range, or it proves nothing")

	assert.Equal(t, strconv.FormatInt(want, 10), raw,
		"every bit up to 62 must survive the wire unchanged")
}
