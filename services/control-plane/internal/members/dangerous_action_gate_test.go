package members_test

// #3454: the dangerous-action gate on a ban and on a kick that purges. These
// run through the real router, so the verifier is the real MFA handler: the
// actor enrolls a WebAuthn credential, and a "code" is an inline step-up
// token minted for one purpose. A token minted for another route's purpose is
// a real wrong-purpose token, refused by the token store itself.

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/members"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	gateBodyInvalidCode = `{"error":"Invalid MFA code"}`
	gateBodyEnrollment  = `{"error":"Set up an authenticator app or security key to do this.","mfa_enrollment_required":true}`
	gateBodyMFARequired = `{"error":"MFA verification required","methods":["webauthn"],"mfa_required":true}`
	gateWrongCode       = "123456"
)

// gateFixture is a server with an owner, a moderator holding Kick and Ban
// through a role, and a plain member as the target.
type gateFixture struct {
	owner, mod, target testhelpers.TestUser
	serverID           string
}

func newGateFixture(t *testing.T, ts *testhelpers.TestServer) gateFixture {
	t.Helper()
	s := uuid.NewString()[:8]
	f := gateFixture{
		owner:  ts.CreateTestUser(t, "gto"+s),
		mod:    ts.CreateTestUser(t, "gtm"+s),
		target: ts.CreateTestUser(t, "gtt"+s),
	}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Gate server")
	ts.AddMemberToServer(t, f.serverID, f.mod.ID, "member")
	ts.AddMemberToServer(t, f.serverID, f.target.ID, "member")
	role := ts.CreateTestRole(t, f.serverID, "moderator", 10, int64(rbac.ModeratorPermissions|rbac.PermBan))
	ts.AssignRoleToUser(t, f.serverID, f.mod.ID, role)
	return f
}

func enrollGateWebAuthn(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, NOW())`, uuid.NewString(), userID, []byte("cred-"+userID))
	require.NoError(t, err)
}

func mintGateToken(t *testing.T, db *sql.DB, userID string, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), db, userID, stepup.FactorWebAuthn, purpose, "")
	require.Nil(t, e)
	return token
}

func gateTokenLive(t *testing.T, db *sql.DB, userID string, purpose stepup.Purpose) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM step_up_tokens WHERE user_id = $1 AND purpose = $2`,
		userID, string(purpose)).Scan(&n))
	return n > 0
}

func gateCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

// gateRoute is one gated moderation request.
type gateRoute struct {
	name    string
	purpose stepup.Purpose
	// other is another D1 route's purpose: a token minted for it is the
	// wrong-purpose token.
	other  stepup.Purpose
	method string
	path   func(f gateFixture) string
	body   func(code string) map[string]any
	// done reports whether the action landed.
	done func(t *testing.T, db *sql.DB, f gateFixture) bool
}

func gateRoutes() []gateRoute {
	member := func(f gateFixture) string { return "/api/v1/servers/" + f.serverID + "/members/" + f.target.ID }
	ban := func(f gateFixture) string { return "/api/v1/servers/" + f.serverID + "/bans/" + f.target.ID }
	withCode := func(base map[string]any, code string) map[string]any {
		if code != "" {
			base["mfa_code"] = code
		}
		return base
	}
	return []gateRoute{
		{
			name: "ban", purpose: stepup.PurposeMemberBan, other: stepup.PurposeMemberKickPurge,
			method: http.MethodPost, path: ban,
			body: func(code string) map[string]any { return withCode(map[string]any{}, code) },
			done: func(t *testing.T, db *sql.DB, f gateFixture) bool {
				return gateCount(t, db, `SELECT COUNT(*) FROM server_bans WHERE server_id = $1 AND user_id = $2`, f.serverID, f.target.ID) == 1
			},
		},
		{
			name: "kick with purge", purpose: stepup.PurposeMemberKickPurge, other: stepup.PurposeMemberBan,
			method: http.MethodDelete, path: member,
			body: func(code string) map[string]any { return withCode(map[string]any{"purge_messages": true}, code) },
			done: func(t *testing.T, db *sql.DB, f gateFixture) bool {
				return gateCount(t, db, `SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`, f.serverID, f.target.ID) == 0
			},
		},
	}
}

// assertNothingWritten is "a refusal writes nothing": the target is still a
// member, unbanned, and no moderation audit row or purge row exists.
func assertNothingWritten(t *testing.T, db *sql.DB, f gateFixture) {
	t.Helper()
	assert.Equal(t, 1, gateCount(t, db, `SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`, f.serverID, f.target.ID))
	assert.Zero(t, gateCount(t, db, `SELECT COUNT(*) FROM server_bans WHERE server_id = $1`, f.serverID))
	assert.Zero(t, gateCount(t, db, `SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action IN ('member_banned', 'member_removed')`, f.serverID))
	assert.Zero(t, gateCount(t, db, `SELECT COUNT(*) FROM message_purges WHERE server_id = $1`, f.serverID))
}

// gateCase is one cell of the per-route table.
type gateCase struct {
	route               gateRoute
	enforcing, enrolled bool
	code                string
}

func (tc gateCase) name() string {
	name := tc.route.name
	if tc.enforcing {
		name += "/enforcing"
	}
	if tc.enrolled {
		name += "/enrolled"
	}
	return name + "/" + tc.code
}

// gateCases is spec §11's table: route × toggle × enrolled × {no code, wrong
// code, valid code, a WebAuthn token for another purpose}.
func gateCases() []gateCase {
	routes := gateRoutes()
	cases := make([]gateCase, 0, len(routes)*16)
	for _, route := range routes {
		for _, enforcing := range []bool{false, true} {
			for _, enrolled := range []bool{false, true} {
				for _, code := range []string{"none", "wrong", "valid", "other purpose"} {
					cases = append(cases, gateCase{route: route, enforcing: enforcing, enrolled: enrolled, code: code})
				}
			}
		}
	}
	return cases
}

// TestModerationGate_Table runs gateCases through the real router.
func TestModerationGate_Table(t *testing.T) {
	ts := setupTS(t)
	for _, tc := range gateCases() {
		t.Run(tc.name(), func(t *testing.T) {
			f := newGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, tc.enforcing)
			if tc.enrolled {
				enrollGateWebAuthn(t, ts.DB, f.mod.ID)
			}
			valid := mintGateToken(t, ts.DB, f.mod.ID, tc.route.purpose)
			other := mintGateToken(t, ts.DB, f.mod.ID, tc.route.other)
			sent := map[string]string{"none": "", "wrong": gateWrongCode, "valid": valid, "other purpose": other}[tc.code]

			w := ts.DoRequest(tc.route.method, tc.route.path(f), tc.route.body(sent), testhelpers.AuthHeaders(f.mod.AccessToken))

			assert.True(t, gateTokenLive(t, ts.DB, f.mod.ID, tc.route.other), "a token for another purpose is never spent")
			assertGateOutcome(t, ts.DB, f, tc, w)
		})
	}
}

// assertGateOutcome is the table's expected answer for one cell.
func assertGateOutcome(t *testing.T, db *sql.DB, f gateFixture, tc gateCase, w *httptest.ResponseRecorder) {
	t.Helper()
	switch {
	case !tc.enforcing:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, tc.route.done(t, db, f))
		assert.True(t, gateTokenLive(t, db, f.mod.ID, tc.route.purpose), "nothing is verified off the toggle")
	case !tc.enrolled:
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, gateBodyEnrollment, w.Body.String(), "RS5 at the masked preflight denial")
		assertNothingWritten(t, db, f)
	case tc.code == "valid":
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, tc.route.done(t, db, f))
		assert.False(t, gateTokenLive(t, db, f.mod.ID, tc.route.purpose), "the verified token is spent on commit")
	case tc.code == "none":
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, gateBodyMFARequired, w.Body.String())
		assertNothingWritten(t, db, f)
	default:
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, gateBodyInvalidCode, w.Body.String())
		assertNothingWritten(t, db, f)
		assert.True(t, gateTokenLive(t, db, f.mod.ID, tc.route.purpose))
	}
}

// I-ID: the owner bypasses authorizeModerationTx, never the gate.
func TestModerationGate_OwnerIsGated(t *testing.T) {
	ts := setupTS(t)
	for _, route := range gateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			enrollGateWebAuthn(t, ts.DB, f.owner.ID)

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.owner.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.JSONEq(t, gateBodyMFARequired, w.Body.String())
			assertNothingWritten(t, ts.DB, f)

			token := mintGateToken(t, ts.DB, f.owner.ID, route.purpose)
			w = ts.DoRequest(route.method, route.path(f), route.body(token), testhelpers.AuthHeaders(f.owner.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.True(t, route.done(t, ts.DB, f))
		})
	}
}

// I-UNGATED: a kick that purges nothing asks for nothing, even from an
// enrolled moderator on an enforcing server, and spends nothing it is sent.
func TestModerationGate_KickWithoutPurgeIsUngated(t *testing.T) {
	ts := setupTS(t)
	f := newGateFixture(t, ts)
	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	enrollGateWebAuthn(t, ts.DB, f.mod.ID)
	token := mintGateToken(t, ts.DB, f.mod.ID, stepup.PurposeMemberKickPurge)

	w := ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+f.serverID+"/members/"+f.target.ID,
		map[string]any{"mfa_code": token}, testhelpers.AuthHeaders(f.mod.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"message":"Member removed successfully"}`, w.Body.String())
	assert.True(t, gateTokenLive(t, ts.DB, f.mod.ID, stepup.PurposeMemberKickPurge), "an ungated kick verifies nothing")
	exists, err := ts.Redis.Exists(context.Background(), stepup.DangerousActionBudget(nil).Key(f.mod.ID)).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "an ungated kick charges nothing")
}

// RS5 pair on an enforcing server: an unenrolled raw holder learns that
// enrollment would lift the denial; a member without the bit gets the
// unchanged generic 403.
func TestModerationGate_RS5Pair(t *testing.T) {
	ts := setupTS(t)
	for _, route := range gateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			plain := ts.CreateTestUser(t, "gtp"+uuid.NewString()[:8])
			ts.AddMemberToServer(t, f.serverID, plain.ID, "member")

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.mod.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.JSONEq(t, gateBodyEnrollment, w.Body.String())

			w = ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(plain.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, `{"error":"insufficient permissions"}`, w.Body.String())
			assertNothingWritten(t, ts.DB, f)
		})
	}
}

// The budget is charged only when a code is present, and cleared only after a
// verified commit.
func TestModerationGate_BudgetChargedOnlyWithACode(t *testing.T) {
	ts := setupTS(t)
	for _, route := range gateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			enrollGateWebAuthn(t, ts.DB, f.mod.ID)
			key := stepup.DangerousActionBudget(nil).Key(f.mod.ID)
			charged := func() string {
				v, err := ts.Redis.Get(context.Background(), key).Result()
				if err != nil {
					return "absent"
				}
				return v
			}

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.mod.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "absent", charged(), "no code, no charge")

			w = ts.DoRequest(route.method, route.path(f), route.body(gateWrongCode), testhelpers.AuthHeaders(f.mod.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "1", charged(), "a refused code is charged and not cleared")

			token := mintGateToken(t, ts.DB, f.mod.ID, route.purpose)
			w = ts.DoRequest(route.method, route.path(f), route.body(token), testhelpers.AuthHeaders(f.mod.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "absent", charged(), "a verified commit clears the budget")
		})
	}
}

// provenanceRecorder is a serverMessagePurger that records the provenance
// each moderation purge arrived with.
type provenanceRecorder struct {
	mu   sync.Mutex
	seen []messages.PurgeProvenance
}

func (r *provenanceRecorder) PurgeUserServerMessages(
	_ context.Context, _, _, _, _ string, provenance messages.PurgeProvenance, _ bool,
) (int, messages.PurgeStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, provenance)
	return 0, messages.PurgeCompleted, nil
}

func (r *provenanceRecorder) last() messages.PurgeProvenance {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return 0
	}
	return r.seen[len(r.seen)-1]
}

// TestModerationGate_PurgeProvenance pins what PurgeUserServerMessages
// receives (A-3.7): unconfirmed from a server that did not enforce, confirmed
// from a verified gate and from a grace-covered one.
func TestModerationGate_PurgeProvenance(t *testing.T) {
	ts := setupTS(t)
	f := newGateFixture(t, ts)
	testhelpers.EnrollInlineTOTP(t, ts.DB, f.mod.ID)
	verifier := &testhelpers.FakeMFAVerifier{AcceptCode: "246810"}
	recorder := &provenanceRecorder{}
	log := logger.NewWithWriter(&bytes.Buffer{})
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	handler := members.NewHandler(ts.DB, log, ts.Redis, ts.Hub, resolver, nil)
	handler.SetAuthorityHandler(rbac.NewHandler(ts.DB, log, nil, nil, resolver, nil, nil))
	handler.SetMFAVerifier(verifier)
	handler.SetServerMessagePurger(recorder, 0, 0)
	sid := uuid.NewString()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", f.mod.ID)
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"sid": sid})
		c.Next()
	})
	router.POST("/api/v1/servers/:id/bans/:user_id", handler.BanMember)
	router.DELETE("/api/v1/servers/:id/members/:user_id", handler.RemoveMember)
	do := func(method, targetID, body string) *httptest.ResponseRecorder {
		path := "/api/v1/servers/" + f.serverID + "/bans/" + targetID
		if method == http.MethodDelete {
			path = "/api/v1/servers/" + f.serverID + "/members/" + targetID
		}
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	newTarget := func() string {
		u := ts.CreateTestUser(t, "gtv"+uuid.NewString()[:8])
		ts.AddMemberToServer(t, f.serverID, u.ID, "member")
		return u.ID
	}

	w := do(http.MethodPost, f.target.ID, `{"purge_messages":true}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, messages.PurgeUnconfirmed, recorder.last(), "a ban committed while the server did not enforce")

	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	w = do(http.MethodPost, newTarget(), `{"purge_messages":true,"mfa_code":"246810"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, messages.PurgeConfirmed, recorder.last(), "a verified ban")
	require.Equal(t, 1, verifier.Calls())

	w = do(http.MethodPost, newTarget(), `{"purge_messages":true}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, messages.PurgeConfirmed, recorder.last(), "a grace-covered ban")
	assert.Equal(t, 1, verifier.Calls(), "grace confirms with no verifier call")

	// The ban's grace is per bit: a kick needs its own confirmation.
	w = do(http.MethodDelete, newTarget(), `{"purge_messages":true}`)
	require.Equal(t, http.StatusForbidden, w.Code)
	w = do(http.MethodDelete, newTarget(), `{"purge_messages":true,"mfa_code":"246810"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, messages.PurgeConfirmed, recorder.last(), "a verified purging kick")
	assert.Equal(t, []stepup.Purpose{stepup.PurposeMemberBan, stepup.PurposeMemberKickPurge}, verifier.Purposes())
	assert.Len(t, recorder.seen, 4, "the refused kick never reached the purge")
}
