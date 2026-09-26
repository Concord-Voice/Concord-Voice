package rbac_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Resolver-level tests of the #3453 MFA mask on an ENFORCING server. T8 owns
// the full I-1 harness; these pin the entry-point exits and the raw bypasses.

func enforceMFA(t *testing.T, ts *testhelpers.TestServer, serverID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, serverID)
	require.NoError(t, err)
}

// enrollWebAuthn gives userID an inline factor (policy P1: any WebAuthn row).
func enrollWebAuthn(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key) VALUES ($1, $2, $3)`,
		userID, []byte("cred-"+uuid.NewString()), []byte("test-public-key"),
	)
	require.NoError(t, err)
}

// countingQuerier counts the statements a resolver issues through it, and can
// rewrite one, so a Tx entry point can be observed and faulted without a
// production seam.
type countingQuerier struct {
	inner interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	}
	statements int
	rewrite    func(query string) string
}

func (q *countingQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.statements++
	if q.rewrite != nil {
		query = q.rewrite(query)
	}
	return q.inner.QueryRowContext(ctx, query, args...)
}

var serverScopedEntryPoints = []string{
	"GetEffectivePermissions/server",
	"ResolveEffectivePermissionsFresh/server",
	"ResolveEffectivePermissionsUncached/server",
	"ResolveServerPermissionsTx",
}

var channelScopedEntryPoints = []string{
	"GetEffectivePermissions/channel",
	"ResolveEffectivePermissionsFresh/channel",
	"ResolveEffectivePermissionsUncached/channel",
	"ResolveChannelPermissionsTx",
	"ResolveEffectivePermissionsForChannelsFresh",
}

func expectScopes(server, channel rbac.Permission) map[string]rbac.Permission {
	out := make(map[string]rbac.Permission, len(serverScopedEntryPoints)+len(channelScopedEntryPoints))
	for _, ep := range serverScopedEntryPoints {
		out[ep] = server
	}
	for _, ep := range channelScopedEntryPoints {
		out[ep] = channel
	}
	return out
}

// Every entry point on an enforcing server, for unenrolled and enrolled
// personas. Expected values are hand-derived literals (see mfa_mask_test.go).
//
// Kills: the mask missing at any one of the five exits (the Administrator and
// owner rows come back raw at that entry point); the mask applied before
// overrides (memberAllow's channel ALLOW of ManageAllMessages reappears); the
// Administrator bypass driven by the masked value (adminDenyWide's channel
// DENY of SendMessages|Administrator starts to apply, leaving 0x1ce3b600).
func TestMFAMask_EnforcingServerEntryPoints(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	f := buildGoldenFixture(t, ts)

	// adminDenyWide: an Administrator whose channel DENY names a
	// non-dangerous bit AND bit 62. Raw bit 62 must still bypass it.
	adminDenyWide := ts.CreateTestUser(t, "mfaadmindenywide")
	ts.AddMemberToServer(t, f.serverID, adminDenyWide.ID, "member")
	wideRole := ts.CreateTestRole(t, f.serverID, "AdminWide", 11, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, f.serverID, adminDenyWide.ID, wideRole)
	ts.CreateChannelOverride(t, f.channelID, "user", adminDenyWide.ID, 0,
		int64(rbac.PermSendMessages|rbac.PermAdministrator))

	enrolledMod := ts.CreateTestUser(t, "mfaenrolledmod")
	ts.AddMemberToServer(t, f.serverID, enrolledMod.ID, "member")
	modRole := ts.CreateTestRole(t, f.serverID, "EnrolledModerator", 6, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, f.serverID, enrolledMod.ID, modRole)
	enrollWebAuthn(t, ts, enrolledMod.ID)

	enrolledAdmin := ts.CreateTestUser(t, "mfaenrolledadmin")
	ts.AddMemberToServer(t, f.serverID, enrolledAdmin.ID, "member")
	ts.AssignRoleToUser(t, f.serverID, enrolledAdmin.ID, wideRole)
	enrollWebAuthn(t, ts, enrolledAdmin.ID)

	enforceMFA(t, ts, f.serverID)

	personas := map[string]string{
		"adminDenyWide": adminDenyWide.ID,
		"enrolledMod":   enrolledMod.ID,
		"enrolledAdmin": enrolledAdmin.ID,
	}
	for name, id := range f.personas {
		personas[name] = id
	}

	want := map[string]map[string]rbac.Permission{
		// Unenrolled owner: OwnerPermissions &^ Dangerous (U4).
		"owner": expectScopes(concreteMinusDangerous, concreteMinusDangerous),
		// Unenrolled Administrators: raw bit 62 bypasses every override, then
		// EXPAND gives every concrete bit and the dangerous ones are removed.
		"admin":         expectScopes(concreteMinusDangerous, concreteMinusDangerous),
		"adminDeny":     expectScopes(concreteMinusDangerous, concreteMinusDangerous),
		"adminDenyWide": expectScopes(concreteMinusDangerous, concreteMinusDangerous),
		// Unenrolled moderator: 0x3cfffe80 &^ (Kick | ManageAllMessages).
		"moderator": expectScopes(0x3cffbe00, 0x3cffbe00),
		// Plain members hold no dangerous bit and are unchanged.
		"member": expectScopes(0x1ce3be00, 0x1ce3be00),
		// The channel ALLOW of ManageAllMessages is masked at the exit.
		"memberAllow": expectScopes(0x1ce3be00, 0x1ce3be00),
		"memberDeny":  expectScopes(0x1ce3be00, 0x1ce3b600),
		// Enrolled members are byte-identical to the golden table.
		"enrolledMod":   expectScopes(0x3cfffe80, 0x3cfffe80),
		"enrolledAdmin": expectScopes(0x400000001ce3be00, 0x400000001ce3be00),
	}

	for persona, userID := range personas {
		t.Run(persona, func(t *testing.T) {
			expected, ok := want[persona]
			require.True(t, ok, "no expectation for persona %s", persona)
			obs := observeAll(t, r, ts, f, userID)
			for entryPoint, w := range expected {
				got, ok := obs.perms[entryPoint]
				require.True(t, ok, "entry point %s not observed", entryPoint)
				assert.Equal(t, w, got, "%s: got %#x want %#x", entryPoint, int64(got), int64(w))
			}
			for _, p := range goldenHasPermChecks {
				assert.Equal(t, expected["GetEffectivePermissions/server"].Has(p.perm), obs.hasPerm["server/"+p.name],
					"HasPermission server/%s", p.name)
				assert.Equal(t, expected["GetEffectivePermissions/channel"].Has(p.perm), obs.hasPerm["channel/"+p.name],
					"HasPermission channel/%s", p.name)
			}
		})
	}
}

// The statement-count half of I2 (plan C-1), observed through the one entry
// point that accepts a querier. A server that does not enforce issues exactly
// the statements it did before #3453; enforcing adds the one P1 read.
func TestMFAMask_StatementCounts(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "mfacountowner")
	member := ts.CreateTestUser(t, "mfacountmember")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA Count")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	count := func(userID string) int {
		t.Helper()
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, tx.Rollback()) }()
		q := &countingQuerier{inner: tx}
		_, err = r.ResolveServerPermissionsTx(ctx, q, serverID, userID)
		require.NoError(t, err)
		return q.statements
	}

	// Off: membership, owner+flag, BIT_OR for a member; no BIT_OR for the owner.
	assert.Equal(t, 3, count(member.ID), "non-enforcing member")
	assert.Equal(t, 2, count(owner.ID), "non-enforcing owner")

	enforceMFA(t, ts, serverID)
	assert.Equal(t, 4, count(member.ID), "enforcing member adds the P1 read")
	assert.Equal(t, 3, count(owner.ID), "enforcing owner adds the P1 read")

	// MaskFor alone: no statement unless enforcing.
	q := &countingQuerier{inner: ts.DB}
	mask, err := rbac.MaskFor(ctx, q, member.ID, false)
	require.NoError(t, err)
	assert.Equal(t, rbac.MFAMask{}, mask)
	assert.Zero(t, q.statements, "MaskFor must not query when the server does not enforce")

	mask, err = rbac.MaskFor(ctx, q, member.ID, true)
	require.NoError(t, err)
	assert.Equal(t, rbac.MFAMask{Enforcing: true, Enrolled: false}, mask)
	assert.Equal(t, 1, q.statements)

	enrollWebAuthn(t, ts, member.ID)
	mask, err = rbac.MaskFor(ctx, q, member.ID, true)
	require.NoError(t, err)
	assert.Equal(t, rbac.MFAMask{Enforcing: true, Enrolled: true}, mask)
}

// A P1 read failure on an enforcing server is a resolver error, never the
// unmasked value; MaskFor hands back the restrictive mask beside its error.
// Kills: an error path that degrades to the raw (permissive) result.
func TestMFAMask_EnrollmentReadErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "mfaerrowner")
	admin := ts.CreateTestUser(t, "mfaerradmin")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA Read Error")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")
	role := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, role)
	enforceMFA(t, ts, serverID)

	// Scanning NULL into bool fails, which is how the P1 read errors here.
	breakP1 := func(query string) string {
		if strings.Contains(query, "user_mfa_totp") {
			return `SELECT NULL::boolean, NULL::boolean FROM (SELECT $1::uuid) AS subject`
		}
		return query
	}

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	for _, userID := range []string{admin.ID, owner.ID} {
		perms, err := r.ResolveServerPermissionsTx(ctx, &countingQuerier{inner: tx, rewrite: breakP1}, serverID, userID)
		require.Error(t, err, "an unreadable enrollment must not resolve")
		assert.Zero(t, perms)
	}

	mask, err := rbac.MaskFor(ctx, &countingQuerier{inner: tx, rewrite: breakP1}, admin.ID, true)
	require.Error(t, err)
	assert.Equal(t, rbac.MFAMask{Enforcing: true, Enrolled: false}, mask,
		"a caller that logs the error and carries on must still mask")
}
