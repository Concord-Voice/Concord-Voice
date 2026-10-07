package rbac_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// mfaRestricted reads GET /servers/:id/permissions as the given user and
// reports the mfa_restricted key: whether it is present and its value. The
// permissions field must survive alongside it.
func mfaRestricted(t *testing.T, ts *testhelpers.TestServer, serverID, token string) (present bool, value any) {
	t.Helper()
	w := ts.DoRequest(http.MethodGet, serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	raw, ok := body["permissions"].(string)
	require.True(t, ok, "permissions stays a decimal string")
	perms, err := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, err)
	value, present = body["mfa_restricted"]
	// The bitfield beside the flag is the masked one: a restricted caller holds
	// no dangerous bit, whatever its raw value.
	if present {
		assert.Zero(t, rbac.Permission(perms)&(rbac.DangerousPermissions|rbac.PermAdministrator),
			"a restricted caller's permissions must be masked")
	}
	return present, value
}

// mfa_restricted (#3454 X7, X18): present and true exactly while enforcement
// withholds a server-scope permission from the caller; absent, never false,
// for everyone else.
//
// Kills: the flag hardcoded either way; omitempty dropped (absent rows answer
// false); a predicate that reads the dangerous bits but not raw Administrator
// (admin row); the bitfield returned unmasked beside a true flag; one that
// reads channel overrides (channelAllow row, X18); the
// enforcement or enrollment conjunct dropped (notEnforcing, enrolled rows).
func TestGetMyServerPermissions_MFARestricted(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "mfarestrictowner")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA restricted")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	member := func(name string, bits rbac.Permission) testhelpers.TestUser {
		u := ts.CreateTestUser(t, name)
		ts.AddMemberToServer(t, serverID, u.ID, "member")
		if bits != 0 {
			ts.AssignRoleToUser(t, serverID, u.ID, ts.CreateTestRole(t, serverID, name, 5, int64(bits)))
		}
		return u
	}
	admin := member("mfarestrictadmin", rbac.PermAdministrator)
	channelManager := member("mfarestrictchanmgr", rbac.PermManageChannels)
	plain := member("mfarestrictplain", 0)
	channelAllow := member("mfarestrictallow", 0)
	ts.CreateChannelOverride(t, channelID, "user", channelAllow.ID, int64(rbac.PermManageChannels), 0)
	enrolledAdmin := member("mfarestrictenrolled", rbac.PermAdministrator)
	testhelpers.EnrollInlineTOTP(t, ts.DB, enrolledAdmin.ID)

	otherOwner := ts.CreateTestUser(t, "mfarestrictother")
	notEnforcingID := ts.CreateTestServer(t, otherOwner.ID, "Not enforcing")

	testhelpers.SetServerMFAEnforcement(t, ts.DB, serverID, true)

	cases := []struct {
		name     string
		serverID string
		token    string
		want     bool
	}{
		{"unenrolled owner", serverID, owner.AccessToken, true},
		{"unenrolled raw Administrator", serverID, admin.AccessToken, true},
		{"unenrolled raw dangerous bit", serverID, channelManager.AccessToken, true},
		{"plain member", serverID, plain.AccessToken, false},
		{"dangerous bit only as a channel ALLOW", serverID, channelAllow.AccessToken, false},
		{"enrolled raw Administrator", serverID, enrolledAdmin.AccessToken, false},
		{"unenrolled owner, not enforcing", notEnforcingID, otherOwner.AccessToken, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, value := mfaRestricted(t, ts, tc.serverID, tc.token)
			if tc.want {
				assert.True(t, present, "mfa_restricted absent")
				assert.Equal(t, true, value)
				return
			}
			assert.False(t, present, "mfa_restricted must be ABSENT, not false, got %v", value)
		})
	}
}
