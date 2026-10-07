//go:build integration

package rbac_test

// #3454 T7: the dangerous-action gates on CreateRole, UpdateRole and
// DeleteRole, RS4 and the D6 AssignRole pin, RS5 at the role routes, the
// channel-key forced-retry pin (A-1), GuardTx's #3508 classification, and the
// flip test. Driven over HTTP through the real router and the real MFA
// verifier, except the forced-retry pin, which needs the handler's test seam.
// Each test names the mutant that kills it.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// rgDangerous is a dangerous grant the owner holds (OwnerPermissions carries
// it), so the owner passes the escalation check and only the gate decides.
const rgDangerous = int64(rbac.PermManageCryptoRotation)

const (
	rgWrongCode     = "123456"
	rgForbiddenBody = `{"error":"Insufficient permissions"}`
	rgMFARequired   = `{"error":"MFA verification required","mfa_required":true,"methods":["webauthn"]}`
	rgCreatedName   = "Gated create"
)

// The submissions the per-route table sends, and the answers it classifies.
const (
	rgNoCode        = "no code"
	rgWrong         = "wrong code"
	rgValid         = "valid code"
	rgOtherWebAuthn = "WebAuthn token minted for another purpose"

	rgOK          = "ok"
	rgRequired    = "mfa required"
	rgInvalid     = "invalid code"
	rgEnrollment  = "enrollment required"
	rgUnknownFmt  = "unexpected %d %s"
	rgEnrollField = `"mfa_enrollment_required":true`
)

// roleGateRoute is one gated role route: how to prepare and call it, and
// whether its write landed.
type roleGateRoute struct {
	name                  string
	purpose, otherPurpose stepup.Purpose
	okStatus              int
	method                string
	// path builds the route's path; roleID is empty for CreateRole.
	path func(serverID, roleID string) string
	// body builds the request body; nil sends none.
	body    func(code string) any
	applied func(t *testing.T, db *sql.DB, serverID, roleID string) bool
}

func roleGateRoutes() []roleGateRoute {
	return []roleGateRoute{
		{
			name: "CreateRole", purpose: stepup.PurposeRoleCreate, otherPurpose: stepup.PurposeRoleUpdate,
			okStatus: http.StatusCreated, method: http.MethodPost,
			path: func(serverID, _ string) string { return rolesPath(serverID) },
			body: func(code string) any {
				b := map[string]any{"name": rgCreatedName, "permissions": bitsJSON(rgDangerous)}
				if code != "" {
					b["mfa_code"] = code
				}
				return b
			},
			applied: func(t *testing.T, db *sql.DB, serverID, _ string) bool {
				return rgCount(t, db, `SELECT count(*) FROM roles WHERE server_id = $1 AND name = $2`, serverID, rgCreatedName) == 1
			},
		},
		{
			name: "UpdateRole", purpose: stepup.PurposeRoleUpdate, otherPurpose: stepup.PurposeRoleDelete,
			okStatus: http.StatusOK, method: http.MethodPatch,
			path: rolePath,
			body: func(code string) any {
				b := map[string]any{"permissions": bitsJSON(rgDangerous)}
				if code != "" {
					b["mfa_code"] = code
				}
				return b
			},
			applied: func(t *testing.T, db *sql.DB, _, roleID string) bool {
				return rgCount(t, db, `SELECT count(*) FROM roles WHERE id = $1 AND permissions = $2`, roleID, rgDangerous) == 1
			},
		},
		{
			name: "DeleteRole", purpose: stepup.PurposeRoleDelete, otherPurpose: stepup.PurposeRoleCreate,
			okStatus: http.StatusOK, method: http.MethodDelete,
			path: rolePath,
			body: func(code string) any {
				if code == "" {
					return nil
				}
				return map[string]any{"mfa_code": code}
			},
			applied: func(t *testing.T, db *sql.DB, _, roleID string) bool {
				return rgCount(t, db, `SELECT count(*) FROM roles WHERE id = $1`, roleID) == 0
			},
		},
	}
}

func rgCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

// rgEnrollWebAuthn gives userID an inline WebAuthn factor (policy P1), as the
// members gate tests do. The real verifier accepts that user's WebAuthn
// inline tokens, minted by rgMintToken; no other code is ever valid.
func rgEnrollWebAuthn(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, NOW())`, uuid.NewString(), userID, []byte("cred-"+userID))
	require.NoError(t, err)
}

func rgMintToken(t *testing.T, db *sql.DB, userID string, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), db, userID, stepup.FactorWebAuthn, purpose, "")
	require.Nil(t, e)
	return token
}

func rgLiveTokens(t *testing.T, db *sql.DB, userID string) int {
	t.Helper()
	return rgCount(t, db, `SELECT count(*) FROM step_up_tokens WHERE user_id = $1`, userID)
}

func rgBudget(t *testing.T, rdb *redis.Client, userID string) int {
	t.Helper()
	n, err := rdb.Get(context.Background(), stepup.DangerousActionBudget(rdb).Key(userID)).Int()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	return n
}

// rgFixture is a fresh owner, server and target role per case, so no case
// inherits a rate-limit bucket, a spent token or a deleted row.
type rgFixture struct {
	owner    testhelpers.TestUser
	serverID string
	roleID   string
}

func newRGFixture(t *testing.T, ts *testhelpers.TestServer, enforcing, enrolled bool) rgFixture {
	t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	f := rgFixture{owner: ts.CreateTestUser(t, "rg"+tag)}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Role gate "+tag)
	f.roleID = ts.CreateTestRole(t, f.serverID, "target-"+tag, 1, 0)
	if enrolled {
		rgEnrollWebAuthn(t, ts.DB, f.owner.ID)
	}
	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, enforcing)
	return f
}

func (r roleGateRoute) do(ts *testhelpers.TestServer, token string, f rgFixture, code string) *httptest.ResponseRecorder {
	return ts.DoRequest(r.method, r.path(f.serverID, f.roleID), r.body(code), testhelpers.AuthHeaders(token))
}

func rgSameJSON(got, want string) bool {
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(want), &w) != nil {
		return false
	}
	return reflect.DeepEqual(g, w)
}

func rgEnrollmentBody() string {
	return fmt.Sprintf(`{"error":%q,"mfa_enrollment_required":true}`, stepup.ErrMsgMFAEnrollmentRequired)
}

func classifyRoleGate(r roleGateRoute, w *httptest.ResponseRecorder) string {
	body := w.Body.String()
	switch {
	case w.Code == r.okStatus:
		return rgOK
	case w.Code == http.StatusForbidden && rgSameJSON(body, rgMFARequired):
		return rgRequired
	case w.Code == http.StatusForbidden && rgSameJSON(body, fmt.Sprintf(`{"error":%q}`, stepup.ErrMsgInvalidMFACode)):
		return rgInvalid
	case w.Code == http.StatusForbidden && rgSameJSON(body, rgEnrollmentBody()):
		return rgEnrollment
	}
	return fmt.Sprintf(rgUnknownFmt, w.Code, body)
}

func wantRoleGate(enforcing, enrolled bool, submission string) string {
	switch {
	case !enforcing:
		return rgOK
	case !enrolled:
		return rgEnrollment
	case submission == rgNoCode:
		return rgRequired
	case submission == rgValid:
		return rgOK
	default:
		return rgInvalid
	}
}

// The per-route table: setting x enrollment x submission, acting as the owner
// (I-ID: the owner is gated too). A refusal writes nothing; a server that does
// not enforce verifies nothing (the own-purpose token stays unspent); a token
// minted for a sibling purpose is refused and stays unspent. An unenrolled
// owner of an enforcing server is masked, so RS5 at the route middleware
// answers before the handler runs.
// Kills: Require removed from createRoleTx, deleteRoleTx or
// requireRoleUpdateGate (enforcing + enrolled + no code answers ok); Require
// run under another purpose (the own-purpose token is refused); fires=false.
func TestRoleGate_PerRouteTable(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range roleGateRoutes() {
		for _, enforcing := range []bool{false, true} {
			for _, enrolled := range []bool{false, true} {
				for _, submission := range []string{rgNoCode, rgWrong, rgValid, rgOtherWebAuthn} {
					t.Run(fmt.Sprintf("%s/enforcing=%t/enrolled=%t/%s", r.name, enforcing, enrolled, submission), func(t *testing.T) {
						runRoleGateCase(t, ts, r, enforcing, enrolled, submission)
					})
				}
			}
		}
	}
}

func runRoleGateCase(t *testing.T, ts *testhelpers.TestServer, r roleGateRoute, enforcing, enrolled bool, submission string) {
	f := newRGFixture(t, ts, enforcing, enrolled)
	code := map[string]string{rgNoCode: "", rgWrong: rgWrongCode}[submission]
	switch submission {
	case rgValid:
		code = rgMintToken(t, ts.DB, f.owner.ID, r.purpose)
	case rgOtherWebAuthn:
		code = rgMintToken(t, ts.DB, f.owner.ID, r.otherPurpose)
	}
	tokensBefore := rgLiveTokens(t, ts.DB, f.owner.ID)

	w := r.do(ts, f.owner.AccessToken, f, code)
	want := wantRoleGate(enforcing, enrolled, submission)
	require.Equal(t, want, classifyRoleGate(r, w), w.Body.String())
	assert.Equal(t, want == rgOK, r.applied(t, ts.DB, f.serverID, f.roleID), "the write lands exactly when the gate admits")

	wantTokens := tokensBefore
	if enforcing && enrolled && submission == rgValid {
		wantTokens--
	}
	assert.Equal(t, wantTokens, rgLiveTokens(t, ts.DB, f.owner.ID),
		"a token is spent only under its own purpose, on an enforcing server, by a committed write")
}

// With the setting off, a request that carries no code is answered as before
// #3454: the same status and body shape, no budget charge, no grace written.
// Kills: Charge run without a code; a grace granted on an unverified success.
func TestRoleGate_SettingOffAndNoCodeIsUnchanged(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range roleGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newRGFixture(t, ts, false, true)
			w := r.do(ts, f.owner.AccessToken, f, "")
			require.Equal(t, r.okStatus, w.Code, w.Body.String())
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			if r.name == "DeleteRole" {
				assert.JSONEq(t, `{"message":"Role deleted"}`, w.Body.String())
			} else {
				require.Len(t, body, 1, w.Body.String())
				var role map[string]any
				require.NoError(t, json.Unmarshal(body["role"], &role))
				assert.Equal(t, bitsJSON(rgDangerous), role["permissions"], "the decimal-string wire form is unchanged")
				for k := range role {
					assert.NotContains(t, strings.ToLower(k), "mfa")
				}
			}
			assert.True(t, r.applied(t, ts.DB, f.serverID, f.roleID))
			assert.Equal(t, 0, rgBudget(t, ts.Redis, f.owner.ID), "a request with no code is never charged")
			graces, err := ts.Redis.Keys(context.Background(), "stepup:grace:"+f.owner.ID+":*").Result()
			require.NoError(t, err)
			assert.Empty(t, graces, "an unverified success grants nothing")
		})
	}
}

// The budget is charged before the transaction only when a code is sent,
// stands on a refusal and on an unverified success, and is cleared only after
// a verified commit.
// Kills: Charge dropped or run on every request; settleRoleGate skipped, or
// run before the commit or on a refusal.
func TestRoleGate_BudgetIsChargedOnlyWithACode(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, r := range roleGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			off := newRGFixture(t, ts, false, true)
			require.Equal(t, rgOK, classifyRoleGate(r, r.do(ts, off.owner.AccessToken, off, rgWrongCode)))
			assert.Equal(t, 1, rgBudget(t, ts.Redis, off.owner.ID),
				"a code is charged whatever the setting, and an unverified success clears nothing")

			on := newRGFixture(t, ts, true, true)
			require.Equal(t, rgRequired, classifyRoleGate(r, r.do(ts, on.owner.AccessToken, on, "")))
			assert.Equal(t, 0, rgBudget(t, ts.Redis, on.owner.ID), "a refusal with no code is not charged")
			require.Equal(t, rgInvalid, classifyRoleGate(r, r.do(ts, on.owner.AccessToken, on, rgWrongCode)))
			assert.Equal(t, 1, rgBudget(t, ts.Redis, on.owner.ID), "a refused code stays charged")
			token := rgMintToken(t, ts.DB, on.owner.ID, r.purpose)
			require.Equal(t, rgOK, classifyRoleGate(r, r.do(ts, on.owner.AccessToken, on, token)))
			assert.Equal(t, 0, rgBudget(t, ts.Redis, on.owner.ID), "a verified commit clears the budget")
		})
	}
}

// UpdateRole fires only when the write NEWLY confers a dangerous grant: an
// edit that keeps an existing grant, or carries no permissions at all, is
// ungated on an enforcing server.
// Kills: the prior read dropped (prior 0 would gate the unchanged grant);
// fires computed on the requested bits alone.
func TestRoleGate_UpdateRoleFiresOnlyOnANewDangerousGrant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newRGFixture(t, ts, true, true)
	_, err := ts.DB.Exec(`UPDATE roles SET permissions = $2 WHERE id = $1`, f.roleID, rgDangerous)
	require.NoError(t, err)
	path := rolePath(f.serverID, f.roleID)
	auth := testhelpers.AuthHeaders(f.owner.AccessToken)

	w := ts.DoRequest(http.MethodPatch, path, map[string]any{"permissions": bitsJSON(rgDangerous | int64(rbac.PermSendMessages))}, auth)
	assert.Equal(t, http.StatusOK, w.Code, "keeping a granted bit is not a new grant: %s", w.Body.String())
	w = ts.DoRequest(http.MethodPatch, path, map[string]any{"name": "renamed"}, auth)
	assert.Equal(t, http.StatusOK, w.Code, "an edit without permissions is ungated: %s", w.Body.String())
	w = ts.DoRequest(http.MethodPatch, path, map[string]any{"permissions": bitsJSON(rgDangerous | int64(rbac.PermManageDevResources))}, auth)
	assert.Equal(t, rgRequired, classifyRoleGate(roleGateRoutes()[1], w), "a newly added dangerous bit fires")
}

// rgPersona adds a member holding roleID to the server.
func rgPersona(t *testing.T, ts *testhelpers.TestServer, serverID, roleID, name string, enrolled bool) testhelpers.TestUser {
	t.Helper()
	u := ts.CreateTestUser(t, name+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	ts.AddMemberToServer(t, serverID, u.ID, "member")
	ts.AssignRoleToUser(t, serverID, u.ID, roleID)
	if enrolled {
		rgEnrollWebAuthn(t, ts.DB, u.ID)
	}
	return u
}

// RS5 at the role routes' middleware (§14.1): on an enforcing server, a member
// who would hold ManageRoles without the mask, through the bit or raw
// Administrator, and has no inline factor, is told to enroll; a member without
// the bit, enrolled or not, gets the unchanged generic 403. Nothing is written.
// Kills: EnrollmentDenial removed from RequirePermission's denial branch.
func TestRoleGate_RS5AtTheRouteMiddleware(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newRGFixture(t, ts, true, true)
	manage := ts.CreateTestRole(t, f.serverID, "manage-roles", 5, int64(rbac.PermManageRoles))
	admin := ts.CreateTestRole(t, f.serverID, "admin", 6, int64(rbac.PermAdministrator))
	other := ts.CreateTestRole(t, f.serverID, "other", 4, int64(rbac.PermSendMessages))
	restricted := map[string]testhelpers.TestUser{
		"unenrolled ManageRoles holder": rgPersona(t, ts, f.serverID, manage, "rsm", false),
		"unenrolled raw Administrator":  rgPersona(t, ts, f.serverID, admin, "rsa", false),
	}
	generic := map[string]testhelpers.TestUser{
		"unenrolled member without the bit": rgPersona(t, ts, f.serverID, other, "rsu", false),
		"enrolled member without the bit":   rgPersona(t, ts, f.serverID, other, "rse", true),
	}
	for _, r := range roleGateRoutes() {
		for name, u := range restricted {
			w := r.do(ts, u.AccessToken, f, "")
			assert.Equal(t, rgEnrollment, classifyRoleGate(r, w), "%s %s", r.name, name)
		}
		for name, u := range generic {
			w := r.do(ts, u.AccessToken, f, rgWrongCode)
			assert.Equal(t, http.StatusForbidden, w.Code, "%s %s", r.name, name)
			assert.Equal(t, rgForbiddenBody, w.Body.String(), "%s %s", r.name, name)
		}
		assert.False(t, r.applied(t, ts.DB, f.serverID, f.roleID), "%s: no refusal writes", r.name)
	}
}

// RS5 inside the transactions: a planted permission-cache entry lets an
// unenrolled ManageRoles holder past the cached middleware, as a flag flipped
// on after that entry was published would. The in-transaction denial is then
// the masked one, and answers with the enrollment body on the transaction.
// Kills: EnrollmentDenial removed from authorizeCreateRoleTx or from
// requireRoleMutationPermissionTx.
func TestRoleGate_RS5InTheTransaction(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cases := []struct {
		name   string
		method string
		path   func(f rgFixture) string
		body   any
	}{
		{"CreateRole", http.MethodPost, func(f rgFixture) string { return rolesPath(f.serverID) },
			map[string]any{"name": "planted", "permissions": "0"}},
		{"UpdateRole", http.MethodPatch, func(f rgFixture) string { return rolePath(f.serverID, f.roleID) },
			map[string]any{"name": "planted"}},
		{"DeleteRole", http.MethodDelete, func(f rgFixture) string { return rolePath(f.serverID, f.roleID) }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRGFixture(t, ts, true, true)
			holder := rgPersona(t, ts, f.serverID,
				ts.CreateTestRole(t, f.serverID, "manage-roles", 5, int64(rbac.PermManageRoles)), "rsp", false)
			testhelpers.PublishPermissionCache(t, ts.Redis, f.serverID, holder.ID, "", rbac.PermManageRoles)
			w := ts.DoRequest(tc.method, tc.path(f), tc.body, testhelpers.AuthHeaders(holder.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.JSONEq(t, rgEnrollmentBody(), w.Body.String())
			assert.Equal(t, 1, rgCount(t, ts.DB, `SELECT count(*) FROM roles WHERE id = $1 AND name LIKE 'target-%'`, f.roleID))
		})
	}
}

// AssignRole fixtures: an owner, a target member, and a role to assign.
func rgAssign(ts *testhelpers.TestServer, f rgFixture, token, targetID, roleID string) *httptest.ResponseRecorder {
	return ts.DoRequest(http.MethodPost, assignRolePath(f.serverID, targetID), map[string]any{"role_id": roleID},
		testhelpers.AuthHeaders(token))
}

func rgAssigned(t *testing.T, db *sql.DB, serverID, userID, roleID string) bool {
	return rgCount(t, db, `SELECT count(*) FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`,
		serverID, userID, roleID) == 1
}

// The D6 pin and RS4 in the AssignRole table (§14.3, §6). AssignRole is not a
// D1 route: assigning an existing role that carries a dangerous grant asks no
// one for a code, on any server, and an enrolled owner keeps the owner bypass.
// RS4 narrows that bypass for an unenrolled owner of an enforcing server only:
// a role carrying a bit the masked owner set lacks is refused with the
// enrollment body, while one carrying no masked bit is still assignable.
// Kills: the gate wired into AssignRole (the enrolled owner is prompted); RS4
// applied to an enrolled owner or a non-enforcing server; RS4 removed from
// both the pre-check and the guard.
func TestRoleGate_AssignRoleD6PinAndRS4(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cases := []struct {
		name                string
		enforcing, enrolled bool
		perms               int64
		want                string
	}{
		{"enrolled owner, enforcing, dangerous role: ungated (D6)", true, true, rgDangerous, rgOK},
		{"enrolled owner, enforcing, Administrator role: ungated (D6)", true, true, int64(rbac.PermAdministrator), rgOK},
		{"unenrolled owner, not enforcing, dangerous role", false, false, rgDangerous, rgOK},
		{"unenrolled owner, enforcing, unmasked role", true, false, int64(rbac.PermSendMessages), rgOK},
		{"unenrolled owner, enforcing, dangerous role: RS4", true, false, rgDangerous, rgEnrollment},
		{"unenrolled owner, enforcing, Administrator role: RS4", true, false, int64(rbac.PermAdministrator), rgEnrollment},
		{"unenrolled owner, enforcing, ManageChannels role: RS4", true, false, int64(rbac.PermManageChannels), rgEnrollment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRGFixture(t, ts, tc.enforcing, tc.enrolled)
			target := ts.CreateTestUser(t, "rgt"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
			ts.AddMemberToServer(t, f.serverID, target.ID, "member")
			role := ts.CreateTestRole(t, f.serverID, "assignable", 2, tc.perms)
			w := rgAssign(ts, f, f.owner.AccessToken, target.ID, role)
			assign := roleGateRoute{okStatus: http.StatusOK}
			require.Equal(t, tc.want, classifyRoleGate(assign, w), w.Body.String())
			assert.Equal(t, tc.want == rgOK, rgAssigned(t, ts.DB, f.serverID, target.ID, role))
		})
	}
}

// guardStatementFragment is unique to the authoritative role guard's statement.
const guardStatementFragment = `FOR SHARE OF r`

// RS4 is decided twice and each copy is pinned on its own. The pooled
// pre-check refuses before any transaction opens (the guard's statement never
// runs); the authoritative guard refuses a role that gained a masked bit after
// the pre-check read it.
// Kills: RS4 removed from preCheckOwnerConferral (the transaction runs);
// RS4 removed from evaluateRoleGuard (the late-granted role is assigned).
func TestRoleGate_RS4PreCheckAndGuardEachRefuse(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	t.Run("pre-check", func(t *testing.T) {
		f := newRGFixture(t, ts, true, false)
		target := ts.CreateTestUser(t, "rgp"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
		ts.AddMemberToServer(t, f.serverID, target.ID, "member")
		role := ts.CreateTestRole(t, f.serverID, "dangerous", 2, rgDangerous)
		hook.Arm([]string{guardStatementFragment}, nil, nil)
		w := rgAssign(ts, f, f.owner.AccessToken, target.ID, role)
		seen, _ := hook.Report()
		assert.JSONEq(t, rgEnrollmentBody(), w.Body.String())
		assert.Zero(t, seen, "the pre-check must refuse before the transaction")
		assert.False(t, rgAssigned(t, ts.DB, f.serverID, target.ID, role))
	})
	t.Run("guard", func(t *testing.T) {
		f := newRGFixture(t, ts, true, false)
		target := ts.CreateTestUser(t, "rgg"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
		ts.AddMemberToServer(t, f.serverID, target.ID, "member")
		role := ts.CreateTestRole(t, f.serverID, "late", 2, 0)
		hook.Arm([]string{guardStatementFragment}, func() error {
			_, err := ts.DB.Exec(`UPDATE roles SET permissions = $2 WHERE id = $1`, role, rgDangerous)
			return err
		}, nil)
		w := rgAssign(ts, f, f.owner.AccessToken, target.ID, role)
		seen, betweenErr := hook.Report()
		require.Equal(t, 1, seen)
		require.NoError(t, betweenErr)
		assert.JSONEq(t, rgEnrollmentBody(), w.Body.String())
		assert.False(t, rgAssigned(t, ts.DB, f.serverID, target.ID, role))
	})
}

// rgCategory creates a category on serverID. No helper writes categories or
// their overrides.
func rgCategory(t *testing.T, db *sql.DB, serverID string) string {
	t.Helper()
	var categoryID string
	require.NoError(t, db.QueryRow(
		`INSERT INTO channel_groups (server_id, name) VALUES ($1, 'rs4') RETURNING id`, serverID).Scan(&categoryID))
	return categoryID
}

// rgCategoryOverrideSQL gives a role a category override ALLOW.
const rgCategoryOverrideSQL = `INSERT INTO category_permission_overrides (category_id, target_type, target_id, allow, deny)
	VALUES ($1, 'role', $2, $3, 0)`

// RS4 counts what an assignment confers, not only roles.permissions (Codex
// security review of #3454). A role with permissions = 0 carrying a dangerous
// channel or category override ALLOW is refused to an unenrolled owner of an
// enforcing server, because the resolver ORs a channel override into its
// holder's channel permissions and the category sync copies a category
// override onto synced children with no gate (C11). An unmasked override, an
// enrolled owner and an override on another server's channel stay assignable.
// Kills: the override read removed from preCheckOwnerConferral AND from
// unenrolledOwnerConferral (the Administrator row is assigned); the category
// arm dropped from roleOverrideAllowsQuery (the category row is assigned); the
// same-server predicate dropped (the foreign row is refused).
func TestRoleGate_RS4CountsOverrideAllows(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	channelOverride := func(t *testing.T, f rgFixture, roleID string, allow int64) {
		ts.CreateChannelOverride(t, ts.CreateTestChannel(t, f.serverID, "rs4"), "role", roleID, allow, 0)
	}
	foreignChannelOverride := func(t *testing.T, _ rgFixture, roleID string, allow int64) {
		other := ts.CreateTestUser(t, "rgx"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
		otherServer := ts.CreateTestServer(t, other.ID, "Other "+other.ID[:8])
		ts.CreateChannelOverride(t, ts.CreateTestChannel(t, otherServer, "rs4"), "role", roleID, allow, 0)
	}
	categoryOverride := func(t *testing.T, f rgFixture, roleID string, allow int64) {
		_, err := ts.DB.Exec(rgCategoryOverrideSQL, rgCategory(t, ts.DB, f.serverID), roleID, allow)
		require.NoError(t, err)
	}
	cases := []struct {
		name     string
		enrolled bool
		override func(*testing.T, rgFixture, string, int64)
		allow    int64
		want     string
	}{
		{"channel override, Administrator", false, channelOverride, int64(rbac.PermAdministrator), rgEnrollment},
		{"channel override, ManageAllMessages (masked, not D)", false, channelOverride,
			int64(rbac.PermManageAllMessages), rgEnrollment},
		{"category override, ManageDevResources", false, categoryOverride,
			int64(rbac.PermManageDevResources), rgEnrollment},
		{"channel override, unmasked bit", false, channelOverride, int64(rbac.PermSendMessages), rgOK},
		{"enrolled owner keeps the bypass", true, channelOverride, int64(rbac.PermAdministrator), rgOK},
		{"override on another server's channel", false, foreignChannelOverride, int64(rbac.PermAdministrator), rgOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRGFixture(t, ts, true, tc.enrolled)
			target := ts.CreateTestUser(t, "rgo"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
			ts.AddMemberToServer(t, f.serverID, target.ID, "member")
			role := ts.CreateTestRole(t, f.serverID, "chanmod", 2, 0)
			tc.override(t, f, role, tc.allow)
			w := rgAssign(ts, f, f.owner.AccessToken, target.ID, role)
			require.Equal(t, tc.want, classifyRoleGate(roleGateRoute{okStatus: http.StatusOK}, w), w.Body.String())
			assert.Equal(t, tc.want == rgOK, rgAssigned(t, ts.DB, f.serverID, target.ID, role))
		})
	}
}

// The override arm of RS4 is decided twice, like the bitfield arm, and each
// copy is pinned on its own. The guard case writes a CATEGORY override between
// the pre-check and the guard statement: the assignment transaction already
// holds the server's channels FOR UPDATE there, so a channel override insert
// would wait on its foreign-key check for the rest of the test.
// Kills: the override read removed from preCheckOwnerConferral (the
// transaction runs); the override read removed from unenrolledOwnerConferral
// (an override written after the pre-check read is assigned).
func TestRoleGate_RS4OverridePreCheckAndGuardEachRefuse(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	t.Run("pre-check", func(t *testing.T) {
		f := newRGFixture(t, ts, true, false)
		target := ts.CreateTestUser(t, "rop"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
		ts.AddMemberToServer(t, f.serverID, target.ID, "member")
		role := ts.CreateTestRole(t, f.serverID, "chanmod", 2, 0)
		ts.CreateChannelOverride(t, ts.CreateTestChannel(t, f.serverID, "rs4"), "role", role,
			int64(rbac.PermAdministrator), 0)
		hook.Arm([]string{guardStatementFragment}, nil, nil)
		w := rgAssign(ts, f, f.owner.AccessToken, target.ID, role)
		seen, _ := hook.Report()
		assert.JSONEq(t, rgEnrollmentBody(), w.Body.String())
		assert.Zero(t, seen, "the pre-check must refuse before the transaction")
		assert.False(t, rgAssigned(t, ts.DB, f.serverID, target.ID, role))
	})
	t.Run("guard", func(t *testing.T) {
		f := newRGFixture(t, ts, true, false)
		target := ts.CreateTestUser(t, "rog"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
		ts.AddMemberToServer(t, f.serverID, target.ID, "member")
		role := ts.CreateTestRole(t, f.serverID, "late-chanmod", 2, 0)
		categoryID := rgCategory(t, ts.DB, f.serverID)
		hook.Arm([]string{guardStatementFragment}, func() error {
			_, err := ts.DB.Exec(rgCategoryOverrideSQL, categoryID, role, int64(rbac.PermAdministrator))
			return err
		}, nil)
		w := rgAssign(ts, f, f.owner.AccessToken, target.ID, role)
		seen, betweenErr := hook.Report()
		require.Equal(t, 1, seen)
		require.NoError(t, betweenErr)
		assert.JSONEq(t, rgEnrollmentBody(), w.Body.String())
		assert.False(t, rgAssigned(t, ts.DB, f.serverID, target.ID, role))
	})
}

// A lock conflict PostgreSQL reports at CreateRole's COMMIT is a refused
// COMMIT: the transaction rolled back, so it answers the retryable
// lock_conflict 503 with Retry-After: 1, as every other gated route does
// (Codex review of #3454). Any other commit failure keeps the route's 500.
// Kills: roleCommitError not applied (a commit lock conflict answers 500);
// every commit error marked (the control answers lock_conflict).
func TestRoleGate_CreateRoleCommitLockConflictIsRetryable(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	for _, tc := range []struct {
		name  string
		fault error
		lock  bool
	}{
		{"deadlock at commit", &pq.Error{Code: "40P01", Message: "deadlock detected"}, true},
		{"lock timeout at commit", &pq.Error{Code: "55P03", Message: "canceling statement due to lock timeout"}, true},
		{"any other commit failure", stmthook.ErrInjected, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRGFixture(t, ts, false, false)
			hook.ArmCommit(false, tc.fault)

			w := ts.DoRequest(http.MethodPost, rolesPath(f.serverID),
				map[string]any{"name": rgCreatedName, "permissions": bitsJSON(0)}, testhelpers.AuthHeaders(f.owner.AccessToken))

			assert.Zero(t, rgCount(t, ts.DB, `SELECT count(*) FROM roles WHERE server_id = $1 AND name = $2`,
				f.serverID, rgCreatedName), "a refused COMMIT creates nothing")
			if tc.lock {
				require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
				assert.JSONEq(t, `{"error":"The server is busy. Try again.","lock_conflict":true}`, w.Body.String())
				assert.Equal(t, "1", w.Header().Get("Retry-After"))
				return
			}
			require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			assert.JSONEq(t, `{"error":"Failed to create role"}`, w.Body.String(), "the route's own 500, not the gate's")
		})
	}
}

// GuardTx's #3508 classification survives the gate (A-8): an enrolled owner
// of an enforcing server whose erasure lands at the transaction's first users
// lock gets each route's vanished-server 403, never the gate's 401.
// Kills: LockGateTx moved ahead of GuardTx or the principal lock.
func TestRoleGate_OwnerErasureKeepsTheVanishedServerAnswer(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	firstUsersLock := map[string]string{
		"CreateRole": `SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`,
		"UpdateRole": `ORDER BY id FOR NO KEY UPDATE`,
		"DeleteRole": `ORDER BY id FOR NO KEY UPDATE`,
	}
	for _, r := range roleGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newRGFixture(t, ts, true, true)
			hook.Arm([]string{firstUsersLock[r.name]}, func() error {
				_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, f.owner.ID)
				return err
			}, nil)
			w := r.do(ts, f.owner.AccessToken, f, rgMintToken(t, ts.DB, f.owner.ID, r.purpose))
			seen, betweenErr := hook.Report()
			require.Equal(t, 1, seen)
			require.NoError(t, betweenErr)
			assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.Equal(t, rgForbiddenBody, w.Body.String())
		})
	}
}

const rgFlipSQL = `UPDATE servers SET enforce_mfa_dangerous_actions = true WHERE id = $1`

// rgStartFlip turns enforcement on from its own transaction, under its own
// lock_timeout, and reports how it ended.
func rgStartFlip(db *sql.DB, serverID string) <-chan error {
	done := make(chan error, 1)
	go func() {
		tx, err := db.Begin()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SET LOCAL lock_timeout = '10s'`); err != nil {
			done <- err
			return
		}
		if _, err := tx.Exec(rgFlipSQL, serverID); err != nil {
			done <- err
			return
		}
		done <- tx.Commit()
	}()
	return done
}

// rgWaitForFlipLockWait polls pg_stat_activity until the flip is parked on a
// row lock. The poll only paces reads of an observed state; nothing is
// inferred from elapsed time.
func rgWaitForFlipLockWait(db *sql.DB, done <-chan error) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return fmt.Errorf("the toggle did not wait for the gate transaction: %v", err)
		default:
		}
		var waiting bool
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%SET enforce_mfa_dangerous_actions = true%')`).Scan(&waiting); err != nil {
			return err
		}
		if waiting {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("the toggle's UPDATE never waited on a lock")
}

// The flip test: each route is held at its write, after its gate read the
// flag OFF, and the toggle's UPDATE must wait on the gate's servers lock, so
// the flag cannot change between the gate's read and the commit it governs.
// CreateRole's servers FOR SHARE is the gate's own; UpdateRole and DeleteRole
// already held servers FOR UPDATE through their authority wrapper.
// Kills: CreateRole's LockGateTx removed or weakened to FOR KEY SHARE.
func TestRoleGate_FlipWaitsForTheGate(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	firstWrite := map[string]string{
		"CreateRole": `INSERT INTO roles (id, server_id, name`,
		"UpdateRole": `UPDATE roles SET permissions = `,
		"DeleteRole": `DELETE FROM roles WHERE id = $1 AND server_id = $2`,
	}
	for _, r := range roleGateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newRGFixture(t, ts, false, true)
			var flip <-chan error
			hook.Arm([]string{firstWrite[r.name]}, func() error {
				flip = rgStartFlip(ts.DB, f.serverID)
				return rgWaitForFlipLockWait(ts.DB, flip)
			}, nil)
			w := r.do(ts, f.owner.AccessToken, f, "")
			seen, betweenErr := hook.Report()
			require.Equal(t, 1, seen)
			require.NoError(t, betweenErr)
			require.Equal(t, r.okStatus, w.Code, "the gate read the flag OFF and the write committed: %s", w.Body.String())
			select {
			case err := <-flip:
				require.NoError(t, err, "the toggle commits once the gate transaction ends")
			case <-time.After(10 * time.Second):
				t.Fatal("the toggle never resumed")
			}
		})
	}
}

// I-UNGATED, statement by statement: an UpdateRole that carries no
// permissions never issues the gate's statements.
// Kills: LockGateTx run unconditionally in updateRoleTx.
func TestRoleGate_UpdateRoleWithoutPermissionsTakesNoGateLock(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	f := newRGFixture(t, ts, true, true)
	hook.Arm([]string{`enforce_mfa_dangerous_actions, current_setting('transaction_isolation')`}, nil, nil)
	w := ts.DoRequest(http.MethodPatch, rolePath(f.serverID, f.roleID), map[string]any{"name": "ungated"},
		testhelpers.AuthHeaders(f.owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	seen, _ := hook.Report()
	assert.Zero(t, seen, "an edit without permissions must not run LockGateTx")
}

// rgCountingVerifier is the real MFA verifier, counted.
type rgCountingVerifier struct {
	inner    stepup.MFATxCodeVerifier
	mu       sync.Mutex
	purposes []stepup.Purpose
}

func (v *rgCountingVerifier) GetEnabledMethods(ctx context.Context, userID string) ([]string, error) {
	return v.inner.GetEnabledMethods(ctx, userID)
}

func (v *rgCountingVerifier) VerifyCodeTx(ctx context.Context, tx *sql.Tx, userID string, purpose stepup.Purpose, code string) (bool, error) {
	v.mu.Lock()
	v.purposes = append(v.purposes, purpose)
	v.mu.Unlock()
	return v.inner.VerifyCodeTx(ctx, tx, userID, purpose, code)
}

// The channel-key forced-retry pin (§14.9, A-1). A channel created between
// the wrapper's preflight and its locked re-read forces
// errChannelAuthoritySetChanged on attempt 1, which returns before write, so
// only attempt 2 reaches the gate. The gated delete still succeeds; the real
// verifier runs once, for the one attempt that reached the gate; and the
// WebAuthn token is gone after the commit.
// Kills: the set comparison moved after write in
// runServerChannelKeyAuthorityWrite (the verifier runs twice).
func TestRoleGate_ChannelKeyForcedRetryVerifiesOncePerGateAttempt(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newRGFixture(t, ts, true, true)

	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	log := logger.New("test")
	verifier := &rgCountingVerifier{inner: mfa.NewHandler(ts.DB, ts.Redis, log, ring, testhelpers.TestJWTSecret, nil, "test")}
	cache := rbac.NewPermissionCache(ts.Redis)
	h := rbac.NewHandler(ts.DB, log, ts.Redis, websocket.NewHub(ts.DB, ts.Redis), rbac.NewResolver(ts.DB, cache, log), cache, nil)
	h.SetMFAVerifier(verifier)
	attempts := 0
	rbac.SetChannelKeyAuthorityPreflightForTest(h, func() {
		attempts++
		if attempts == 1 {
			ts.CreateTestChannel(t, f.serverID, "forces-the-retry")
		}
	})

	token := rgMintToken(t, ts.DB, f.owner.ID, stepup.PurposeRoleDelete)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: f.serverID}, {Key: "role_id", Value: f.roleID}}
	c.Set("user_id", f.owner.ID)
	body, err := json.Marshal(map[string]string{"mfa_code": token})
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodDelete, "/", bytes.NewReader(body))
	h.DeleteRole(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, attempts, "the forced change must make the wrapper retry")
	assert.Equal(t, []stepup.Purpose{stepup.PurposeRoleDelete}, verifier.purposes,
		"the verifier runs once per attempt that reached the gate: attempt 1 returned before write")
	assert.Zero(t, rgLiveTokens(t, ts.DB, f.owner.ID), "the token is spent by the committed attempt")
	assert.True(t, roleGateRoutes()[2].applied(t, ts.DB, f.serverID, f.roleID))
}

// A lock conflict on CreateRole's own gate lock is the gate's 503, not the
// role-row guard's 500: another transaction holds the servers row FOR UPDATE
// when the gate's FOR SHARE runs, the gate's 3 s lock_timeout fires (55P03),
// and the route answers busy with Retry-After: 1 and writes nothing.
// Kills: the gate lock taken before applyGuardLockTimeout (the request waits
// for the holder instead of timing out); errRoleGateLock dropped from
// lockRoleGateTx, or isRoleGateError dropped from mapGuardError (500).
func TestRoleGate_CreateRoleGateLockConflictIsBusy(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	f := newRGFixture(t, ts, false, true)
	var holder *sql.Tx
	defer func() {
		if holder != nil {
			_ = holder.Rollback()
		}
	}()
	hook.ArmArg([]string{`enforce_mfa_dangerous_actions, current_setting('transaction_isolation')`}, f.serverID, func() error {
		tx, err := ts.DB.Begin()
		if err != nil {
			return err
		}
		holder = tx
		_, err = tx.Exec(`SELECT id FROM servers WHERE id = $1 FOR UPDATE`, f.serverID)
		return err
	}, nil)

	w := ts.DoRequest(http.MethodPost, rolesPath(f.serverID),
		map[string]any{"name": rgCreatedName, "permissions": "0"}, testhelpers.AuthHeaders(f.owner.AccessToken))

	seen, betweenErr := hook.Report()
	require.Equal(t, 1, seen, "the hook must fire at the gate's servers read")
	require.NoError(t, betweenErr)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"The server is busy. Try again.","lock_conflict":true}`, w.Body.String())
	assert.Equal(t, "1", w.Header().Get("Retry-After"))
	assert.False(t, roleGateRoutes()[0].applied(t, ts.DB, f.serverID, ""), "a refusal writes nothing")
}

// roles.delete is grace-eligible: a verified delete lets the same session's
// next delete on the same server through without a code, while another
// session is prompted, and an always-fresh route (roles.create) ignores the
// grace.
// Kills: settleRoleGate not called after DeleteRole's commit, or chargeRoleGate
// not reading the grace for roles.delete (the second delete is prompted);
// roles.create made grace-eligible.
func TestRoleGate_DeleteRoleGraceCoversTheSameSessionOnly(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	f := newRGFixture(t, ts, true, true)
	session := func() string {
		token, err := auth.GenerateAccessToken(f.owner.ID, testhelpers.TestJWTSecret, true, "", uuid.NewString())
		require.NoError(t, err)
		return token
	}
	first := session()
	del := roleGateRoutes()[2]
	deleteRole := func(token, code string) *httptest.ResponseRecorder {
		target := f
		target.roleID = ts.CreateTestRole(t, f.serverID, "grace-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8], 1, 0)
		return del.do(ts, token, target, code)
	}

	require.Equal(t, rgOK, classifyRoleGate(del, deleteRole(first, rgMintToken(t, ts.DB, f.owner.ID, stepup.PurposeRoleDelete))),
		"verified delete")
	assert.Equal(t, rgOK, classifyRoleGate(del, deleteRole(first, "")), "grace-covered delete, same session")
	assert.Equal(t, rgRequired, classifyRoleGate(del, deleteRole(session(), "")), "another session")
	create := roleGateRoutes()[0]
	assert.Equal(t, rgRequired, classifyRoleGate(create, create.do(ts, first, f, "")), "roles.create is always fresh")
	assert.False(t, create.applied(t, ts.DB, f.serverID, ""))
}
