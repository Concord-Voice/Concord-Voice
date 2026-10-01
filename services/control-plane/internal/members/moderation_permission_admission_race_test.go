package members_test

// Codex on #3508: UpdateMember and TimeoutMember check permission before any
// owner read, and a missing grant answered 403 even when the server had gone.
// Whether a deletion reached that branch depended on the permission cache: a
// warm grant carried the request on to the target check and its 404, while a
// cold or zeroed one stopped it at the 403.
//
// The hook deletes the server while RequireMembership is still resolving the
// actor — after its membership and owner reads, before its role read — so the
// request is admitted and the handler's permission check finds no grant.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

// rolePermissionsRead is rbac.RawRolePermissions' aggregate: for a non-owner,
// the last statement of RequireMembership's resolution.
const rolePermissionsRead = `SELECT COALESCE(BIT_OR(r.permissions), 0) AS total_permissions`

func permissionCheckSites() []moderationSite {
	member := func(f moderationFixture) string {
		return "/api/v1/servers/" + f.serverID + "/members/" + f.target.ID
	}
	return []moderationSite{
		{
			name: "UpdateMember", sequence: []string{rolePermissionsRead},
			method: http.MethodPatch, path: member, body: map[string]string{"role": "admin"},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to update member"}`,
		},
		{
			name: "TimeoutMember", sequence: []string{rolePermissionsRead},
			method: http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body:       map[string]int64{"duration_seconds": 600},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to timeout member"}`,
		},
	}
}

func TestModerationPermissionCheck_ServerGoneDuringAdmission_IsTheVanishedAnswer(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	for _, site := range permissionCheckSites() {
		for _, sc := range stmthook.Scenarios() {
			if sc.Fault != nil {
				continue // a fault at this read is RequireMembership's answer, not the handler's
			}
			t.Run(site.name+"/"+sc.Name, func(t *testing.T) {
				f := newModerationFixture(t, ts)
				hook.Arm(site.sequence, sc.Between(ts.DB, f.owner.ID, f.serverID), sc.Fault)

				w := ts.DoRequest(site.method, site.path(f), site.body, testhelpers.AuthHeaders(f.actor.AccessToken))

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(site.sequence), f.serverID)
				assert.Equal(t, sc.Want, site.classify(w.Code, w.Body.String()))
			})
		}
	}
}

// Control: on a live server a missing grant is still the 403. A fix that
// answered every refusal at this check with the vanished 404 fails here.
func TestModerationPermissionCheck_NoGrantOnALiveServer_StaysForbidden(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	for _, site := range permissionCheckSites() {
		t.Run(site.name, func(t *testing.T) {
			f := newModerationFixture(t, ts)
			bystander := ts.CreateTestUser(t, "modpb"+f.serverID[:8])
			ts.AddMemberToServer(t, f.serverID, bystander.ID, "member")

			w := ts.DoRequest(site.method, site.path(f), site.body, testhelpers.AuthHeaders(bystander.AccessToken))

			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, bodyInsufficient, w.Body.String())
		})
	}
}
