package members_test

// Regression: two member routes read a database fault on a membership check as
// "not a member". UpdateMember discarded the target read's error outright
// (`_ = …Scan(&targetExists)`), and RemoveMember folded `err != nil` into its
// requester (403) and target (404) branches, so an outage answered as a denial
// and the client was told something false about who belongs to the server.
//
// These reads are EXISTS queries: they always return a row, so a missing member
// is `false`, never sql.ErrNoRows, and any error is a fault that must be the
// route's 500. The server-deletion scenarios pin the other half: a member who is
// genuinely gone still gets the route's 403/404.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

// membershipReadFragment is checkMembership's EXISTS (and UpdateMember's
// former inline copy of it). The hook tells the actor's read from the target's
// by the user id argument.
const membershipReadFragment = `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`

type membershipReadSite struct {
	name       string
	method     string
	body       any
	readsActor bool // the hooked read asks about the actor, not the target
	denyStatus int
	denyBody   string
	faultBody  string
}

func membershipReadSites() []membershipReadSite {
	return []membershipReadSite{
		{
			name: "UpdateMember target read", method: http.MethodPatch, body: map[string]string{"role": "admin"},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to update member"}`,
		},
		{
			name: "RemoveMember requester read", method: http.MethodDelete, readsActor: true,
			denyStatus: http.StatusForbidden, denyBody: `{"error":"Not a member of this server"}`,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "RemoveMember target read", method: http.MethodDelete,
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
	}
}

func (s membershipReadSite) classify(code int, body string) string {
	switch {
	case code >= 200 && code < 300:
		return stmthook.Allowed
	case code == s.denyStatus && body == s.denyBody:
		return stmthook.Denied
	case code == http.StatusInternalServerError && body == s.faultBody:
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected %d %s", code, body)
}

// TestMemberRoutes_MembershipReadFault_IsA500NotADenial pins the oracle: a real
// fault on a membership read answers the route's 500, while a membership that
// is genuinely gone still answers the route's 403 or 404.
func TestMemberRoutes_MembershipReadFault_IsA500NotADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{membershipReadFragment}

	for _, site := range membershipReadSites() {
		for _, sc := range stmthook.Scenarios() {
			t.Run(site.name+"/"+sc.Name, func(t *testing.T) {
				f := newModerationFixture(t, ts)
				readUser := f.target.ID
				if site.readsActor {
					readUser = f.actor.ID
				}
				hook.ArmArg(sequence, readUser, sc.Between(ts.DB, f.owner.ID, f.serverID), sc.Fault)

				w := ts.DoRequest(site.method, "/api/v1/servers/"+f.serverID+"/members/"+f.target.ID,
					site.body, testhelpers.AuthHeaders(f.actor.AccessToken))

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), f.serverID)
				assert.Equal(t, sc.Want, site.classify(w.Code, w.Body.String()),
					"%s: a fault on the membership read must be a 500, and a membership that is really gone "+
						"must still be denied", site.name)
			})
		}
	}
}
