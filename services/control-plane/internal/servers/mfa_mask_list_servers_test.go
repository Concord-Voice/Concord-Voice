package servers_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// concreteMinusDangerous is 0x3FFFFFFF &^ 0x0200419B — what an unenrolled
// Administrator or owner is masked to on an enforcing server (rbac
// package's own mfa_mask_test.go pins the same literal). Local copy: this
// package cannot import an unexported rbac-package test constant.
const concreteMinusDangerous = int64(0x3DFFBE64)

// mfaListServersEnforceServer flips servers.enforce_mfa_dangerous_actions
// (#3453), mirroring rbac_test's enforceMFA helper for this package.
func mfaListServersEnforceServer(t *testing.T, ts *testhelpers.TestServer, serverID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, serverID)
	require.NoError(t, err)
}

// mfaListServersEnroll gives userID an inline MFA factor (policy P1: any
// WebAuthn credential row), mirroring rbac_test's enrollWebAuthn helper.
func mfaListServersEnroll(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key) VALUES ($1, $2, $3)`,
		userID, []byte("cred-"+uuid.NewString()), []byte("test-public-key"),
	)
	require.NoError(t, err)
}

func mfaListServersPerms(t *testing.T, row map[string]interface{}) int64 {
	t.Helper()
	raw := testhelpers.JSONField[string](t, row, "permissions")
	parsed, err := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, err, "permissions must be a decimal int64 string, got %q", raw)
	return parsed
}

// TestListServersMFAMask covers the #3453 second enforcement surface end to
// end over HTTP: owner, Administrator and member rows, on an enforcing
// server and a non-enforcing one, each unenrolled and (on the enforcing
// server) enrolled. Kills: the mask missing at any exit; the mask skipped
// for the owner row; the Administrator row masked from raw bit 62 rather
// than from the EXPANDED set; a leaked enforce_mfa_dangerous_actions key.
func TestListServersMFAMask(t *testing.T) {
	ts := setupTS(t)

	// --- Non-enforcing server: byte-identical to the pre-#3453 output,
	// regardless of enrollment (I2). ---
	offOwner := ts.CreateTestUser(t, "mfalsoffowner")
	offAdmin := ts.CreateTestUser(t, "mfalsoffadmin")
	offMember := ts.CreateTestUser(t, "mfalsoffmember")
	offServerID := ts.CreateTestServer(t, offOwner.ID, "MFA ListServers Off")
	ts.AddMemberToServer(t, offServerID, offAdmin.ID, "member")
	ts.AddMemberToServer(t, offServerID, offMember.ID, "member")
	offAdminRole := ts.CreateTestRole(t, offServerID, "off-admin-"+uuid.NewString()[:8], 5, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, offServerID, offAdmin.ID, offAdminRole)
	offMemberRole := ts.CreateTestRole(t, offServerID, "off-kick-"+uuid.NewString()[:8], 4, int64(rbac.PermKick))
	ts.AssignRoleToUser(t, offServerID, offMember.ID, offMemberRole)

	assert.Equal(t, int64(rbac.OwnerPermissions), mfaListServersPerms(t, mfaListServersRowFor(t, ts, offOwner, offServerID)),
		"non-enforcing owner: byte-identical to golden (I2)")
	// AddMemberToServer also assigns the default @all role, so the raw
	// BIT_OR carries BasePermissions alongside bit 62 — the admin role does
	// not replace it.
	assert.Equal(t, int64(rbac.PermAdministrator|rbac.BasePermissions), mfaListServersPerms(t, mfaListServersRowFor(t, ts, offAdmin, offServerID)),
		"non-enforcing Administrator: raw bit 62 plus @all's BasePermissions, never expanded")
	assert.Equal(t, int64(rbac.BasePermissions|rbac.PermKick), mfaListServersPerms(t, mfaListServersRowFor(t, ts, offMember, offServerID)),
		"non-enforcing member: raw BasePermissions|Kick, unmasked")

	// --- Enforcing server, unenrolled: every persona is masked. ---
	onOwner := ts.CreateTestUser(t, "mfalsonowner")
	onAdmin := ts.CreateTestUser(t, "mfalsonadmin")
	onMember := ts.CreateTestUser(t, "mfalsonmember")
	onServerID := ts.CreateTestServer(t, onOwner.ID, "MFA ListServers On")
	ts.AddMemberToServer(t, onServerID, onAdmin.ID, "member")
	ts.AddMemberToServer(t, onServerID, onMember.ID, "member")
	onAdminRole := ts.CreateTestRole(t, onServerID, "on-admin-"+uuid.NewString()[:8], 5, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, onServerID, onAdmin.ID, onAdminRole)
	onMemberRole := ts.CreateTestRole(t, onServerID, "on-kick-"+uuid.NewString()[:8], 4, int64(rbac.PermKick))
	ts.AssignRoleToUser(t, onServerID, onMember.ID, onMemberRole)
	mfaListServersEnforceServer(t, ts, onServerID)

	assert.Equal(t, concreteMinusDangerous, mfaListServersPerms(t, mfaListServersRowFor(t, ts, onOwner, onServerID)),
		"enforcing unenrolled owner: OwnerPermissions &^ Dangerous (U4), masked exactly like the resolver's owner branch")
	assert.Equal(t, concreteMinusDangerous, mfaListServersPerms(t, mfaListServersRowFor(t, ts, onAdmin, onServerID)),
		"enforcing unenrolled Administrator: raw bit 62 EXPANDED to ConcretePermissions, then minus Dangerous")
	assert.Equal(t, int64(rbac.BasePermissions), mfaListServersPerms(t, mfaListServersRowFor(t, ts, onMember, onServerID)),
		"enforcing unenrolled member: Kick (dangerous) stripped, BasePermissions untouched")

	// --- Same enforcing server, now enrolled: byte-identical to the raw
	// (off-server) values — I2's enrolled-equals-golden clause. ---
	mfaListServersEnroll(t, ts, onOwner.ID)
	mfaListServersEnroll(t, ts, onAdmin.ID)
	mfaListServersEnroll(t, ts, onMember.ID)

	assert.Equal(t, int64(rbac.OwnerPermissions), mfaListServersPerms(t, mfaListServersRowFor(t, ts, onOwner, onServerID)),
		"enforcing ENROLLED owner: unmasked, byte-identical to golden")
	assert.Equal(t, int64(rbac.PermAdministrator|rbac.BasePermissions), mfaListServersPerms(t, mfaListServersRowFor(t, ts, onAdmin, onServerID)),
		"enforcing ENROLLED Administrator: raw bit 62 plus @all's BasePermissions, never expanded")
	assert.Equal(t, int64(rbac.BasePermissions|rbac.PermKick), mfaListServersPerms(t, mfaListServersRowFor(t, ts, onMember, onServerID)),
		"enforcing ENROLLED member: Kick is not stripped")
}

// TestListServersMFAMask_EnrollmentReadErrorFailsClosed: when the caller's P1
// read fails, every enforcing row is masked as if the caller were unenrolled,
// so the fault narrows what the UI advertises and never widens it. The caller
// here IS enrolled, so the unmasked value is what a fail-open branch would
// return; the log line carries the fixed failure_class and nothing about the
// caller's enrollment. Kills: the error branch keeping the zero (non-binding)
// mask.
func TestListServersMFAMask_EnrollmentReadErrorFailsClosed(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mfalserrowner")
	member := ts.CreateTestUser(t, "mfalserrmember")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA ListServers Read Error")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	kickRole := ts.CreateTestRole(t, serverID, "err-kick-"+uuid.NewString()[:8], 4, int64(rbac.PermKick))
	ts.AssignRoleToUser(t, serverID, member.ID, kickRole)
	mfaListServersEnforceServer(t, ts, serverID)
	mfaListServersEnroll(t, ts, member.ID)

	// Control: with the real reader the enrolled member keeps Kick.
	require.Equal(t, int64(rbac.BasePermissions|rbac.PermKick),
		mfaListServersPerms(t, mfaListServersRowFor(t, ts, member, serverID)),
		"control: an enrolled member of an enforcing server is unmasked")

	errP1 := errors.New("injected P1 read failure")
	servers.SetListServersMFAMaskReaderForTest(t,
		func(context.Context, *sql.DB, string) (rbac.MFAMask, error) { return rbac.MFAMask{}, errP1 })
	logs := ts.CaptureLogs(t)

	assert.Equal(t, int64(rbac.BasePermissions),
		mfaListServersPerms(t, mfaListServersRowFor(t, ts, member, serverID)),
		"a P1 read error must mask the enforcing row as unenrolled (fail closed)")
	assert.Contains(t, logs.String(), "list_servers_mfa_state_unavailable")
	assert.NotContains(t, logs.String(), member.ID,
		"the failure line must not name the caller")
}

// mfaListServersRowFor fetches GET /api/v1/servers as u and returns the row
// for serverID, asserting it is present and that no
// enforce_mfa_dangerous_actions key ever reaches the response (spec L1):
// that flag is a local per-row bool inside the handler, never a field on
// models.ServerWithRole or any other response struct.
func mfaListServersRowFor(t *testing.T, ts *testhelpers.TestServer, u testhelpers.TestUser, serverID string) map[string]interface{} {
	t.Helper()
	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(u.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := testhelpers.JSONField[[]interface{}](t, body, "servers")
	for _, elem := range servers {
		row, ok := elem.(map[string]interface{})
		require.True(t, ok)
		_, leaked := row["enforce_mfa_dangerous_actions"]
		assert.False(t, leaked, "enforce_mfa_dangerous_actions must never reach a ListServers row")
		if id, _ := row["id"].(string); id == serverID {
			return row
		}
	}
	t.Fatalf("server %s not found in ListServers response for %s", serverID, u.ID)
	return nil
}
