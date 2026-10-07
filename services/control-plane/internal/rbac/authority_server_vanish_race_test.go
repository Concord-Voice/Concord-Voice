package rbac_test

// Regression: an RBAC authority write whose server was deleted after the
// request was admitted answered 500. withAuthorityCapture locks the server row
// (`SELECT id FROM servers WHERE id = $1 FOR UPDATE`) after the route's
// middleware and the handler's pooled preflight have both seen the server, and
// nothing before that statement holds the row: the visibility advisory lock and
// the principals' users-row locks do not stop DeleteServer, nor the owner's
// erasure cascading through servers.owner_id. A server deleted in that gap left
// the lock with no row, and its sql.ErrNoRows reached every caller as a fault.
//
// The hook commits the DELETE on another pool immediately before the parent
// lock is sent — the state a DeleteServer that finished just before this
// request's transaction began leaves behind. Two callers used to answer 404
// here rather than 500 (UnassignRole and DeleteChannelOverride, whose
// errors.Is(err, sql.ErrNoRows) arms meant their OWN row and matched the
// wrapped parent-lock error by accident); every caller now answers the 403 it
// already gives a non-member.

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// authorityParentLockFragment is withAuthorityCapture's server-parent lock.
const authorityParentLockFragment = `SELECT id FROM servers WHERE id = $1 FOR UPDATE`

// authorityPrincipalLockFragment is LockAuthorityPrincipalsTx's users lock,
// which withAuthorityCapture takes before the server-parent lock.
const authorityPrincipalLockFragment = `SELECT id FROM users WHERE id = ANY($1::uuid[])`

// authorityFixture holds what every covered writer needs. The actor is a
// non-owner manager, so the owner's erasure cannot queue behind the actor's
// users-row lock, which the capture transaction holds at the hooked statement.
type authorityFixture struct {
	owner, actor, member testhelpers.TestUser
	serverID             string
	doomedRoleID         string // a role the actor may delete
	wornRoleID           string // a role assigned to member, which the actor may unassign
	channelID            string // carries overrideID, targeting member
	overrideID           string
	syncChannelID        string // in a category, with sync_permissions = TRUE
}

func newAuthorityFixture(t *testing.T, ts *testhelpers.TestServer) authorityFixture {
	t.Helper()
	suffix := uuid.NewString()[:8]
	f := authorityFixture{
		owner:  ts.CreateTestUser(t, "authvo"+suffix),
		actor:  ts.CreateTestUser(t, "authva"+suffix),
		member: ts.CreateTestUser(t, "authvm"+suffix),
	}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Vanishing authority server")
	ts.AddMemberToServer(t, f.serverID, f.actor.ID, "member")
	ts.AddMemberToServer(t, f.serverID, f.member.ID, "member")
	manager := ts.CreateTestRole(t, f.serverID, "manager", 10,
		int64(rbac.PermManageRoles|rbac.PermManageRolesAssign|rbac.PermManageChannels))
	ts.AssignRoleToUser(t, f.serverID, f.actor.ID, manager)
	f.doomedRoleID = ts.CreateTestRole(t, f.serverID, "doomed", 1, 0)
	f.wornRoleID = ts.CreateTestRole(t, f.serverID, "worn", 2, 0)
	ts.AssignRoleToUser(t, f.serverID, f.member.ID, f.wornRoleID)

	f.channelID = ts.CreateTestChannel(t, f.serverID, "vanish-overrides")
	f.overrideID = uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'user', $3, 0, 0)`, f.overrideID, f.channelID, f.member.ID)
	require.NoError(t, err)

	categoryID := uuid.NewString()
	_, err = ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'vanish-category', 0)`,
		categoryID, f.serverID)
	require.NoError(t, err)
	f.syncChannelID = ts.CreateTestChannel(t, f.serverID, "vanish-sync")
	_, err = ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = TRUE WHERE id = $2`, categoryID, f.syncChannelID)
	require.NoError(t, err)
	return f
}

type authorityWriter struct {
	name string
	// request returns the route params and JSON body.
	request   func(f authorityFixture) (gin.Params, any)
	call      func(h *rbac.Handler, c *gin.Context)
	faultBody string
}

func authorityWriters() []authorityWriter {
	return []authorityWriter{
		{
			// mapGuardError: compares sql.ErrNoRows bare, so the wrapped
			// parent-lock error fell to its 500 default.
			name: "DeleteRole",
			request: func(f authorityFixture) (gin.Params, any) {
				// An empty object, not nil: invokeStaleRBACMutation marshals nil to
				// a literal null, which DeleteRole's optional step-up body reader
				// refuses with 400 since #3454 (A-6). {} carries no code, which is
				// what this case always sent.
				return gin.Params{{Key: "id", Value: f.serverID}, {Key: "role_id", Value: f.doomedRoleID}}, map[string]any{}
			},
			call:      func(h *rbac.Handler, c *gin.Context) { h.DeleteRole(c) },
			faultBody: `{"error":"Failed to delete role"}`,
		},
		{
			// Checks errors.Is(err, sql.ErrNoRows) first, for a missing
			// ASSIGNMENT: the wrapped parent-lock error matched it (404).
			name: "UnassignRole",
			request: func(f authorityFixture) (gin.Params, any) {
				return gin.Params{
					{Key: "id", Value: f.serverID}, {Key: "user_id", Value: f.member.ID}, {Key: "role_id", Value: f.wornRoleID},
				}, nil
			},
			call:      func(h *rbac.Handler, c *gin.Context) { h.UnassignRole(c) },
			faultBody: `{"error":"Failed to unassign role"}`,
		},
		{
			name: "UpsertChannelOverride",
			request: func(f authorityFixture) (gin.Params, any) {
				return gin.Params{{Key: "id", Value: f.channelID}}, map[string]any{
					// Bitfields cross JSON as decimal strings (#3473); a JSON number is
					// refused with 400 before the handler reaches the parent lock.
					"target_type": "user", "target_id": f.member.ID, "allow": "0", "deny": bitsJSON(int64(rbac.PermSendMessages)),
				}
			},
			call:      func(h *rbac.Handler, c *gin.Context) { h.UpsertChannelOverride(c) },
			faultBody: `{"error":"Failed to save override"}`,
		},
		{
			// Checks errors.Is(err, sql.ErrNoRows) for a missing OVERRIDE, after
			// ErrNotMember: the wrapped parent-lock error matched it (404).
			name: "DeleteChannelOverride",
			request: func(f authorityFixture) (gin.Params, any) {
				return gin.Params{{Key: "id", Value: f.channelID}, {Key: "override_id", Value: f.overrideID}}, nil
			},
			call:      func(h *rbac.Handler, c *gin.Context) { h.DeleteChannelOverride(c) },
			faultBody: `{"error":"Failed to delete override"}`,
		},
		{
			name: "SetChannelPermissionSync",
			request: func(f authorityFixture) (gin.Params, any) {
				return gin.Params{{Key: "id", Value: f.syncChannelID}}, map[string]bool{"sync_permissions": false}
			},
			call:      func(h *rbac.Handler, c *gin.Context) { h.SetChannelPermissionSync(c) },
			faultBody: `{"error":"Failed to update sync"}`,
		},
	}
}

// invokeAuthorityWriter runs w as f.actor and normalizes its answer by status
// AND exact body, so a 403 or 500 from anywhere else is not mistaken for this.
func invokeAuthorityWriter(t *testing.T, h *rbac.Handler, w authorityWriter, f authorityFixture) string {
	t.Helper()
	return invokeAuthorityWriterAs(t, h, w, f, f.actor.ID)
}

// invokeAuthorityWriterAs is invokeAuthorityWriter with the acting user chosen.
func invokeAuthorityWriterAs(t *testing.T, h *rbac.Handler, w authorityWriter, f authorityFixture, actorID string) string {
	t.Helper()
	params, body := w.request(f)
	rec := <-invokeStaleRBACMutation(t, actorID, params, body, w.call, h)
	code, got := rec.Code, rec.Body.String()
	switch {
	case code >= 200 && code < 300:
		return stmthook.Allowed
	case code == http.StatusForbidden && got == `{"error":"Insufficient permissions"}`:
		return stmthook.Denied
	case code == http.StatusInternalServerError && got == w.faultBody:
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected %d %s", code, got)
}

// TestAuthorityCapture_ServerVanishingBeforeParentLock_IsADenial pins the
// oracle: when the server is deleted after an authority write was admitted and
// before its transaction locks the server row, the writer denies the actor as a
// non-member (403) instead of reporting a fault (500), while a real fault at
// that same lock is still reported as one.
func TestAuthorityCapture_ServerVanishingBeforeParentLock_IsADenial(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	hook, hookedDB := stmthook.Open(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(hookedDB, cache, logger.New("test"))
	h := rbac.NewHandler(hookedDB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	sequence := []string{authorityParentLockFragment}

	for _, w := range authorityWriters() {
		for _, sc := range stmthook.Scenarios() {
			t.Run(w.name+"/"+sc.Name, func(t *testing.T) {
				f := newAuthorityFixture(t, ts)
				hook.Arm(sequence, sc.Between(ts.DB, f.owner.ID, f.serverID), sc.Fault)

				got := invokeAuthorityWriter(t, h, w, f)

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), f.serverID)
				assert.Equal(t, sc.Want, got,
					"%s, with the server deleted before its transaction locked it, must deny as a non-member rather than "+
						"report a fault; a real fault at that lock must still be reported", w.name)
			})
		}
	}
}

// Codex on #3508: the parent-lock denial above is reached only after
// LockAuthorityPrincipalsTx, which locks the acting user's row first. When the
// OWNER acts and their erasure commits after admission, their row and (through
// servers.owner_id's ON DELETE CASCADE) the server are both gone, so the
// principal lock came up short and answered a 500 without reaching the parent
// lock's 403. The cases above act as a non-owner, so none could see it. Here
// the owner acts and the hook fires at the principal lock.
func TestAuthorityCapture_OwnerErasedBeforePrincipalLock_IsADenial(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	hook, hookedDB := stmthook.Open(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(hookedDB, cache, logger.New("test"))
	h := rbac.NewHandler(hookedDB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	sequence := []string{authorityPrincipalLockFragment}

	for _, w := range authorityWriters() {
		for _, sc := range stmthook.Scenarios() {
			t.Run(w.name+"/"+sc.Name, func(t *testing.T) {
				f := newAuthorityFixture(t, ts)
				hook.Arm(sequence, sc.Between(ts.DB, f.owner.ID, f.serverID), sc.Fault)

				got := invokeAuthorityWriterAs(t, h, w, f, f.owner.ID)

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), f.serverID)
				assert.Equal(t, sc.Want, got,
					"%s, acted by the owner with the server gone before the principal lock, must deny as a "+
						"non-member rather than report a fault; a real fault at that lock must still be reported", w.name)
			})
		}
	}
}

// Control for the case above: a principal missing from a server that still
// exists is not a vanished server, and stays a fault. UnassignRole locks its
// target member as a principal; erasing that member, not the owner, leaves the
// server in place, so a fix that read every short principal lock as
// ErrNotMember would turn this 500 into a denial.
func TestAuthorityCapture_PrincipalErasedOnALiveServer_StaysAFault(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	hook, hookedDB := stmthook.Open(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(hookedDB, cache, logger.New("test"))
	h := rbac.NewHandler(hookedDB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)

	var unassign authorityWriter
	for _, w := range authorityWriters() {
		if w.name == "UnassignRole" {
			unassign = w
		}
	}
	require.NotNil(t, unassign.call)

	f := newAuthorityFixture(t, ts)
	hook.Arm([]string{authorityPrincipalLockFragment}, func() error {
		_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, f.member.ID)
		return err
	}, nil)

	got := invokeAuthorityWriterAs(t, h, unassign, f, f.owner.ID)

	seen, betweenErr := hook.Report()
	require.Equal(t, 1, seen, "the hook must fire at the principal lock")
	require.NoError(t, betweenErr)
	assert.Equal(t, stmthook.Fault, got)
}

// Codex on #3508: withAuthorityCapture's deferred rollback discarded its
// error, so when the parent lock found no server and returned ErrNotMember and
// the rollback then failed too, every authority writer still answered the
// clean 403. A failed discard of an interrupted roles or overrides change is a
// fault, as it is for CreateRole and for every gated graph mutation.
//
// The hook ends the hooked transaction's own backend, which sits idle in its
// transaction after the principal lock, and then answers the parent lock with
// no row. The control answers with no row on a live connection, so the
// rollback succeeds and the 403 stands.
func TestAuthorityCapture_FailedDiscardAfterServerGone_IsAFault(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	hook, hookedDB := stmthook.Open(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(hookedDB, cache, logger.New("test"))
	h := rbac.NewHandler(hookedDB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)

	for _, w := range authorityWriters() {
		for _, terminate := range []bool{false, true} {
			name, want := w.name+"/control: the discard succeeds", stmthook.Denied
			if terminate {
				name, want = w.name+"/the discard fails", stmthook.Fault
			}
			t.Run(name, func(t *testing.T) {
				f := newAuthorityFixture(t, ts)
				terminated := 0
				var between func() error
				if terminate {
					between = stmthook.TerminateIdleBackend(ts.DB, authorityPrincipalLockFragment, &terminated)
				}
				hook.Arm([]string{authorityParentLockFragment}, between, sql.ErrNoRows)

				got := invokeAuthorityWriter(t, h, w, f)

				seen, betweenErr := hook.Report()
				require.Equal(t, 1, seen, "the hook must fire at the parent lock")
				require.NoError(t, betweenErr)
				if terminate {
					require.Equal(t, 1, terminated, "exactly the hooked backend must be ended")
				}
				assert.Equal(t, want, got)
			})
		}
	}
}
