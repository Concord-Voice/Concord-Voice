package members_test

// Regression: a member-moderation request whose server was deleted after it
// was admitted answered 500. Each of these handlers reads the servers row for
// the owner id — four pooled preflight reads and the two moderation
// transactions' `… FOR UPDATE` locks — and nothing before any of them holds
// that row, so a server deleted in the gap (by DeleteServer, or by the owner's
// erasure through servers.owner_id's ON DELETE CASCADE) left the read with no
// row, and its sql.ErrNoRows was reported as a fault.
//
// Each site now answers what its handler already answers when membership
// disappears one statement earlier: a 404 "User is not a member of this
// server" where the preceding check is the target's membership (update, timeout
// and removal), a 403 where it is the actor's permission (ban, whose target need
// not be a member at all). The requests run through the real router on a
// stmthook pool; see that package for the harness and the four scenarios.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

const (
	// pooledOwnerRead is the preflight owner read (UpdateMember's inline read
	// and getServerOwnerID). Nothing earlier on these routes sends this text.
	pooledOwnerRead = `SELECT owner_id FROM servers WHERE id = $1`
	// txOwnerLock is the removal transaction's server lock when the kick
	// purges nothing.
	txOwnerLock = `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`
	// txGateLock is the server lock a ban and a purging kick take through the
	// dangerous-action gate (#3454 A-8), in txOwnerLock's place. Only the
	// gate's statements read the isolation level beside the flag.
	txGateLock = `enforce_mfa_dangerous_actions, current_setting('transaction_isolation')`
	// updateRoleWrite and timeoutWrite are UpdateMember's and TimeoutMember's
	// final writes, after every read above. Codex on #3508: a server deleted
	// after the owner read and before these left RETURNING with no row, and its
	// sql.ErrNoRows was reported as a fault.
	updateRoleWrite = `SET role = $1`
	timeoutWrite    = `UPDATE server_members SET timed_out_until = $1`
	// usersLock is the moderation transactions' actor-and-target users lock,
	// taken before the server lock. Codex on #3508: when the actor is the
	// owner and their erasure commits first, the actor's row is gone, the lock
	// comes up one row short, and a ban answered the target-gone 404 instead
	// of the vanished-server 403.
	usersLock = `WHERE id IN ($1, $2)`
	// permissionMemberRead is the resolver's membership read when a permission
	// lookup misses the cache. After the owner read it is RemoveMember's Kick
	// check. hierarchyOwnerRead is CheckHierarchy's first statement, the same
	// text as pooledOwnerRead, so a sequence names it as the second occurrence.
	// Codex on #3508: a server deleted after the owner read made these answer
	// the permission or hierarchy 403 instead of the vanished-target answer.
	permissionMemberRead = `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`
	hierarchyOwnerRead   = pooledOwnerRead
	// hierarchyPositions is CheckHierarchy's role-position comparison, after its
	// owner read. A server deleted between the two leaves both members with no
	// roles, which compares as a hierarchy violation rather than a failed read.
	hierarchyPositions = `WITH actor_max AS (`

	bodyUserNotMember = `{"error":"User is not a member of this server"}`
	bodyInsufficient  = `{"error":"insufficient permissions"}`
)

type moderationFixture struct {
	owner, actor, target testhelpers.TestUser
	serverID             string
}

// newModerationFixture seats a non-owner moderator who outranks the target.
// The actor is not the owner so the owner's erasure cannot queue behind the
// moderation transaction's users-row locks, which it holds at the hooked lock.
func newModerationFixture(t *testing.T, ts *testhelpers.TestServer) moderationFixture {
	t.Helper()
	suffix := uuid.NewString()[:8]
	f := moderationFixture{
		owner:  ts.CreateTestUser(t, "modvo"+suffix),
		actor:  ts.CreateTestUser(t, "modva"+suffix),
		target: ts.CreateTestUser(t, "modvt"+suffix),
	}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Vanishing moderation server")
	ts.AddMemberToServer(t, f.serverID, f.actor.ID, "member")
	ts.AddMemberToServer(t, f.serverID, f.target.ID, "member")
	moderator := ts.CreateTestRole(t, f.serverID, "moderator", 10,
		int64(rbac.ModeratorPermissions|rbac.PermBan|rbac.PermManageRoles|rbac.PermManageRolesAssign))
	ts.AssignRoleToUser(t, f.serverID, f.actor.ID, moderator)
	return f
}

type moderationSite struct {
	name     string
	sequence []string
	method   string
	path     func(f moderationFixture) string
	body     any
	// byOwner sends the request as the server's owner rather than the moderator.
	byOwner bool
	// denyStatus and denyBody are the site's answer for a vanished server;
	// faultBody is its 500 body.
	denyStatus int
	denyBody   string
	faultBody  string
}

func moderationSites() []moderationSite {
	member := func(f moderationFixture) string {
		return "/api/v1/servers/" + f.serverID + "/members/" + f.target.ID
	}
	ban := func(f moderationFixture) string {
		return "/api/v1/servers/" + f.serverID + "/bans/" + f.target.ID
	}
	return []moderationSite{
		{
			name: "UpdateMember preflight (handlers.go:881)", sequence: []string{pooledOwnerRead},
			method: http.MethodPatch, path: member, body: map[string]string{"role": "admin"},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to update member"}`,
		},
		{
			name: "TimeoutMember preflight (handlers.go:945)", sequence: []string{pooledOwnerRead},
			method: http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body:       map[string]int64{"duration_seconds": 600},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to timeout member"}`,
		},
		{
			name: "UpdateMember final write", sequence: []string{pooledOwnerRead, updateRoleWrite},
			method: http.MethodPatch, path: member, body: map[string]string{"role": "admin"},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to update member"}`,
		},
		{
			name: "TimeoutMember final write", sequence: []string{pooledOwnerRead, timeoutWrite},
			method: http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body:       map[string]int64{"duration_seconds": 600},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to timeout member"}`,
		},
		{
			name: "RemoveMember preflight (handlers.go:1399)", sequence: []string{pooledOwnerRead},
			method: http.MethodDelete, path: member,
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "RemoveMember transaction (handlers.go:1230)", sequence: []string{txOwnerLock},
			method: http.MethodDelete, path: member,
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "BanMember preflight (handlers.go:1801)", sequence: []string{pooledOwnerRead},
			method:     http.MethodPost,
			path:       ban,
			denyStatus: http.StatusForbidden, denyBody: bodyInsufficient,
			faultBody: `{"error":"Failed to ban member"}`,
		},
		{
			name: "BanMember users lock, owner acting", sequence: []string{usersLock}, byOwner: true,
			method:     http.MethodPost,
			path:       ban,
			denyStatus: http.StatusForbidden, denyBody: bodyInsufficient,
			faultBody: `{"error":"Failed to ban member"}`,
		},
		{
			// Control: removal's vanished-server answer is the 404, which the
			// same missing row already gives it. A ban-only fix must leave it.
			name: "RemoveMember users lock, owner acting", sequence: []string{usersLock}, byOwner: true,
			method: http.MethodDelete, path: member,
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "RemoveMember permission read", sequence: []string{pooledOwnerRead, permissionMemberRead},
			method: http.MethodDelete, path: member,
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "RemoveMember hierarchy read", sequence: []string{pooledOwnerRead, hierarchyOwnerRead},
			method: http.MethodDelete, path: member,
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "TimeoutMember hierarchy read", sequence: []string{pooledOwnerRead, hierarchyOwnerRead},
			method: http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body:       map[string]int64{"duration_seconds": 600},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to timeout member"}`,
		},
		{
			name: "BanMember hierarchy read", sequence: []string{pooledOwnerRead, hierarchyOwnerRead},
			method:     http.MethodPost,
			path:       ban,
			denyStatus: http.StatusForbidden, denyBody: bodyInsufficient,
			faultBody: `{"error":"Failed to ban member"}`,
		},
		{
			name: "RemoveMember hierarchy positions", sequence: []string{pooledOwnerRead, hierarchyOwnerRead, hierarchyPositions},
			method: http.MethodDelete, path: member,
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "TimeoutMember hierarchy positions", sequence: []string{pooledOwnerRead, hierarchyOwnerRead, hierarchyPositions},
			method: http.MethodPost, path: func(f moderationFixture) string { return member(f) + "/timeout" },
			body:       map[string]int64{"duration_seconds": 600},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to timeout member"}`,
		},
		{
			name: "BanMember hierarchy positions", sequence: []string{pooledOwnerRead, hierarchyOwnerRead, hierarchyPositions},
			method:     http.MethodPost,
			path:       ban,
			denyStatus: http.StatusForbidden, denyBody: bodyInsufficient,
			faultBody: `{"error":"Failed to ban member"}`,
		},
		{
			name: "RemoveMember purging transaction (gate lock)", sequence: []string{txGateLock},
			method: http.MethodDelete, path: member, body: map[string]bool{"purge_messages": true},
			denyStatus: http.StatusNotFound, denyBody: bodyUserNotMember,
			faultBody: `{"error":"Failed to remove member"}`,
		},
		{
			name: "BanMember transaction (gate lock)", sequence: []string{txGateLock},
			method:     http.MethodPost,
			path:       ban,
			denyStatus: http.StatusForbidden, denyBody: bodyInsufficient,
			faultBody: `{"error":"Failed to ban member"}`,
		},
	}
}

// classify normalizes an answer by status AND exact body, so a 403, 404 or 500
// from anywhere else is not mistaken for this site's.
func (s moderationSite) classify(code int, body string) string {
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

// TestMemberModeration_ServerVanishingBeforeOwnerRead_IsADenial pins the
// oracle: when the server is deleted after a moderation request was admitted
// and before one of its owner reads, the handler denies (the 404 or 403 above)
// instead of reporting a fault (500), while a real fault at that same read is
// still reported as one.
func TestMemberModeration_ServerVanishingBeforeOwnerRead_IsADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	for _, site := range moderationSites() {
		for _, sc := range stmthook.Scenarios() {
			t.Run(site.name+"/"+sc.Name, func(t *testing.T) {
				f := newModerationFixture(t, ts)
				hook.Arm(site.sequence, sc.Between(ts.DB, f.owner.ID, f.serverID), sc.Fault)

				token := f.actor.AccessToken
				if site.byOwner {
					token = f.owner.AccessToken
				}
				w := ts.DoRequest(site.method, site.path(f), site.body, testhelpers.AuthHeaders(token))

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(site.sequence), f.serverID)
				assert.Equal(t, sc.Want, site.classify(w.Code, w.Body.String()),
					"%s, with the server deleted before that read, must deny rather than report a fault; "+
						"a real fault at that read must still be reported", site.name)
			})
		}
	}
}

// Control for the owner-acting ban site: a users lock that comes up short by
// the TARGET, on a server that still exists, keeps the ordinary target-gone
// 404. A fix that read every short lock as a vanished server would answer 403.
func TestMemberBan_TargetErasedBeforeUsersLock_IsTargetGone(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	f := newModerationFixture(t, ts)
	hook.Arm([]string{usersLock}, func() error {
		_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, f.target.ID)
		return err
	}, nil)

	w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+f.serverID+"/bans/"+f.target.ID, nil,
		testhelpers.AuthHeaders(f.owner.AccessToken))

	seen, betweenErr := hook.Report()
	require.Equal(t, 1, seen, "the hook must fire at the users lock")
	require.NoError(t, betweenErr)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, bodyUserNotMember, w.Body.String())
}

// A refusal reached after the owner read re-reads the servers row before it
// stands, so a server deleted in between gets the vanished answer. That re-read
// failing is a fault, never the refusal: the cases above cannot show it,
// because the hook fires once and they spend it on the deletion. Here the
// refusal is real — an actor without Kick, and a target who outranks the
// moderator — and the fault lands on the re-read itself. Each control runs the
// same refusal with no fault and keeps the 403.
func TestModerationRefusalRecheck_AFailedRecheckIsAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	cases := []struct {
		name     string
		sequence []string
		arrange  func(t *testing.T, f moderationFixture) string // returns the acting token
		method   string
		path     func(f moderationFixture) string
		body     any
		refusal  string
		fault    string
	}{
		{
			name: "RemoveMember by an actor without Kick",
			// The preflight owner read, then the re-read after the refusal.
			sequence: []string{pooledOwnerRead, pooledOwnerRead},
			arrange: func(t *testing.T, f moderationFixture) string {
				bystander := ts.CreateTestUser(t, "modrb"+uuid.NewString()[:8])
				ts.AddMemberToServer(t, f.serverID, bystander.ID, "member")
				return bystander.AccessToken
			},
			method:  http.MethodDelete,
			path:    func(f moderationFixture) string { return "/api/v1/servers/" + f.serverID + "/members/" + f.target.ID },
			refusal: bodyInsufficient,
			fault:   `{"error":"Failed to remove member"}`,
		},
		{
			name: "TimeoutMember of a member who outranks the moderator",
			// The preflight owner read, CheckHierarchy's owner read, then the
			// re-read after the violation.
			sequence: []string{pooledOwnerRead, hierarchyOwnerRead, pooledOwnerRead},
			arrange: func(t *testing.T, f moderationFixture) string {
				senior := ts.CreateTestRole(t, f.serverID, "senior", 20, 0)
				ts.AssignRoleToUser(t, f.serverID, f.target.ID, senior)
				return f.actor.AccessToken
			},
			method: http.MethodPost,
			path: func(f moderationFixture) string {
				return "/api/v1/servers/" + f.serverID + "/members/" + f.target.ID + "/timeout"
			},
			body:    map[string]int64{"duration_seconds": 600},
			refusal: `{"error":"Cannot timeout a member with equal or higher role position"}`,
			fault:   `{"error":"Failed to timeout member"}`,
		},
	}
	for _, tc := range cases {
		for _, faulted := range []bool{false, true} {
			name := tc.name + "/control: the re-read succeeds"
			var fault error
			wantStatus, wantBody := http.StatusForbidden, tc.refusal
			if faulted {
				name = tc.name + "/the re-read fails"
				fault = stmthook.ErrInjected
				wantStatus, wantBody = http.StatusInternalServerError, tc.fault
			}
			t.Run(name, func(t *testing.T) {
				f := newModerationFixture(t, ts)
				token := tc.arrange(t, f)
				hook.Arm(tc.sequence, nil, fault)

				w := ts.DoRequest(tc.method, tc.path(f), tc.body, testhelpers.AuthHeaders(token))

				seen, _ := hook.Report()
				require.Equal(t, len(tc.sequence), seen, "the hook must reach the re-read")
				assert.Equal(t, wantStatus, w.Code)
				assert.Equal(t, wantBody, w.Body.String())
			})
		}
	}
}
