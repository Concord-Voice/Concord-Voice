package channels_test

// #3454 T4: the dangerous-action gates on DeleteChannel and on shortening a
// channel's message-expiration policy. These run through the real router, so
// the verifier is the real MFA handler: the actor enrolls a WebAuthn
// credential, and a "code" is an inline step-up token minted for one purpose.
// A token minted for the other route's purpose is a real wrong-purpose token,
// refused by the token store itself.

import (
	"bytes"
	"context"
	"database/sql"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	chanGateBodyInvalidCode = `{"error":"Invalid MFA code"}`
	chanGateBodyEnrollment  = `{"error":"Set up an authenticator app or security key to do this.","mfa_enrollment_required":true}`
	chanGateBodyMFARequired = `{"error":"MFA verification required","methods":["webauthn"],"mfa_required":true}`
	chanGateBodyForbidden   = `{"error":"insufficient permissions"}`
	chanGateWrongCode       = "123456"
)

// chanGateFixture is a server with an owner; a manager holding ManageChannels
// through a role; a plain member; and a member whose ManageChannels exists
// only as a channel ALLOW on the gated channel.
type chanGateFixture struct {
	owner, mod, plain, allowOnly testhelpers.TestUser
	serverID, channelID          string
}

func newChanGateFixture(t *testing.T, ts *testhelpers.TestServer) chanGateFixture {
	t.Helper()
	s := uuid.NewString()[:8]
	f := chanGateFixture{
		owner:     ts.CreateTestUser(t, "cgo"+s),
		mod:       ts.CreateTestUser(t, "cgm"+s),
		plain:     ts.CreateTestUser(t, "cgp"+s),
		allowOnly: ts.CreateTestUser(t, "cga"+s),
	}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Channel gate server")
	f.channelID = ts.CreateTestChannel(t, f.serverID, "gated")
	for _, u := range []testhelpers.TestUser{f.mod, f.plain, f.allowOnly} {
		ts.AddMemberToServer(t, f.serverID, u.ID, roleMember)
	}
	role := ts.CreateTestRole(t, f.serverID, "channel manager", 10, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, f.serverID, f.mod.ID, role)
	ts.CreateChannelOverride(t, f.channelID, "user", f.allowOnly.ID, int64(rbac.PermManageChannels), 0)
	return f
}

func enrollChanGateWebAuthn(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, NOW())`, uuid.NewString(), userID, []byte("cred-"+userID))
	require.NoError(t, err)
}

func mintChanGateToken(t *testing.T, db *sql.DB, userID string, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), db, userID, stepup.FactorWebAuthn, purpose, "")
	require.Nil(t, e)
	return token
}

func chanGateTokenLive(t *testing.T, db *sql.DB, userID string, purpose stepup.Purpose) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM step_up_tokens WHERE user_id = $1 AND purpose = $2`,
		userID, string(purpose)).Scan(&n))
	return n > 0
}

// chanGateBudget reads the actor's dangerous-action budget counter, "absent"
// when no attempt has been charged since the last clear.
func chanGateBudget(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	v, err := ts.Redis.Get(context.Background(), stepup.DangerousActionBudget(nil).Key(userID)).Result()
	if err != nil {
		return "absent"
	}
	return v
}

// chanGateWindow reads the channel's expiration window; 0 means none.
func chanGateWindow(t *testing.T, db *sql.DB, channelID string) int64 {
	t.Helper()
	var window sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT expiration_window_seconds FROM channels WHERE id = $1`, channelID).Scan(&window))
	return window.Int64
}

func chanGateChannelExists(t *testing.T, db *sql.DB, channelID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1)`, channelID).Scan(&exists))
	return exists
}

// chanGateRoute is one gated channel request.
type chanGateRoute struct {
	name    string
	purpose stepup.Purpose
	// other is the other channel route's purpose: a token minted for it is
	// the wrong-purpose token.
	other  stepup.Purpose
	method string
	path   func(f chanGateFixture) string
	// body is the request for code; "" sends no mfa_code at all (and no body
	// for DeleteChannel, which never had one).
	body func(code string) any
	// done reports whether the action landed.
	done func(t *testing.T, db *sql.DB, f chanGateFixture) bool
}

func withChanGateCode(base map[string]any, code string) map[string]any {
	if code != "" {
		base["mfa_code"] = code
	}
	return base
}

func shortenBody(code string) any {
	return withChanGateCode(map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"}, code)
}

func chanGateRoutes() []chanGateRoute {
	return []chanGateRoute{
		{
			name: "delete channel", purpose: stepup.PurposeChannelDelete, other: stepup.PurposeChannelExpirationShorten,
			method: http.MethodDelete,
			path:   func(f chanGateFixture) string { return pathChannelsPrefix + f.channelID },
			body: func(code string) any {
				if code == "" {
					return nil
				}
				return map[string]any{"mfa_code": code}
			},
			done: func(t *testing.T, db *sql.DB, f chanGateFixture) bool {
				return !chanGateChannelExists(t, db, f.channelID)
			},
		},
		{
			name: "shorten expiration", purpose: stepup.PurposeChannelExpirationShorten, other: stepup.PurposeChannelDelete,
			method: http.MethodPatch,
			path:   func(f chanGateFixture) string { return pathChannelsPrefix + f.channelID + "/expiration" },
			body:   shortenBody,
			done: func(t *testing.T, db *sql.DB, f chanGateFixture) bool {
				return chanGateWindow(t, db, f.channelID) == 3600
			},
		},
	}
}

// assertChanGateNothingWritten is "a refusal writes nothing": the channel
// still exists with no expiration policy, revision 0, and no system row.
func assertChanGateNothingWritten(t *testing.T, db *sql.DB, f chanGateFixture) {
	t.Helper()
	require.True(t, chanGateChannelExists(t, db, f.channelID), "the channel must survive a refusal")
	var revision int64
	var messages int
	require.NoError(t, db.QueryRow(`SELECT expiration_revision FROM channels WHERE id = $1`, f.channelID).Scan(&revision))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM messages WHERE channel_id = $1`, f.channelID).Scan(&messages))
	assert.Zero(t, chanGateWindow(t, db, f.channelID))
	assert.Zero(t, revision)
	assert.Zero(t, messages)
}

// chanGateCase is one cell of the per-route table.
type chanGateCase struct {
	route               chanGateRoute
	enforcing, enrolled bool
	code                string
}

func (tc chanGateCase) name() string {
	name := tc.route.name
	if tc.enforcing {
		name += "/enforcing"
	}
	if tc.enrolled {
		name += "/enrolled"
	}
	return name + "/" + tc.code
}

// chanGateCases is spec §11's table: route × toggle × enrolled × {no code,
// wrong code, valid code, a WebAuthn token for another purpose}.
func chanGateCases() []chanGateCase {
	routes := chanGateRoutes()
	cases := make([]chanGateCase, 0, len(routes)*16)
	for _, route := range routes {
		for _, enforcing := range []bool{false, true} {
			for _, enrolled := range []bool{false, true} {
				for _, code := range []string{"none", "wrong", "valid", "other purpose"} {
					cases = append(cases, chanGateCase{route: route, enforcing: enforcing, enrolled: enrolled, code: code})
				}
			}
		}
	}
	return cases
}

// TestChannelGate_Table runs chanGateCases through the real router.
func TestChannelGate_Table(t *testing.T) {
	ts := setupTS(t)
	for _, tc := range chanGateCases() {
		t.Run(tc.name(), func(t *testing.T) {
			f := newChanGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, tc.enforcing)
			if tc.enrolled {
				enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)
			}
			valid := mintChanGateToken(t, ts.DB, f.mod.ID, tc.route.purpose)
			other := mintChanGateToken(t, ts.DB, f.mod.ID, tc.route.other)
			sent := map[string]string{"none": "", "wrong": chanGateWrongCode, "valid": valid, "other purpose": other}[tc.code]

			w := ts.DoRequest(tc.route.method, tc.route.path(f), tc.route.body(sent), testhelpers.AuthHeaders(f.mod.AccessToken))

			assert.True(t, chanGateTokenLive(t, ts.DB, f.mod.ID, tc.route.other), "a token for another purpose is never spent")
			assertChanGateOutcome(t, ts.DB, f, tc, w)
		})
	}
}

// assertChanGateOutcome is the table's expected answer for one cell.
func assertChanGateOutcome(t *testing.T, db *sql.DB, f chanGateFixture, tc chanGateCase, w *httptest.ResponseRecorder) {
	t.Helper()
	switch {
	case !tc.enforcing:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, tc.route.done(t, db, f))
		assert.True(t, chanGateTokenLive(t, db, f.mod.ID, tc.route.purpose), "nothing is verified off the toggle")
	case !tc.enrolled:
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, chanGateBodyEnrollment, w.Body.String(), "RS5 at the masked denial")
		assertChanGateNothingWritten(t, db, f)
	case tc.code == "valid":
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, tc.route.done(t, db, f))
		assert.False(t, chanGateTokenLive(t, db, f.mod.ID, tc.route.purpose), "the verified token is spent on commit")
	case tc.code == "none":
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String())
		assertChanGateNothingWritten(t, db, f)
	default:
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, chanGateBodyInvalidCode, w.Body.String())
		assertChanGateNothingWritten(t, db, f)
		assert.True(t, chanGateTokenLive(t, db, f.mod.ID, tc.route.purpose))
	}
}

// Toggle off, no code: each route answers exactly as it did before #3454.
func TestChannelGate_ToggleOffNoCodeIsUnchanged(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	auth := testhelpers.AuthHeaders(f.mod.AccessToken)

	w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+f.channelID+"/expiration", shortenBody(""), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var policy map[string]any
	testhelpers.ParseJSON(t, w, &policy)
	assert.ElementsMatch(t, []string{"window_seconds", "updated_at", "revision", "backfill_pending"}, slices.Collect(maps.Keys(policy)))
	assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID))

	w = ts.DoRequest(http.MethodDelete, pathChannelsPrefix+f.channelID, nil, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, `{"message":"Channel deleted successfully"}`, w.Body.String())
	assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID))
}

// I-ID: the owner bypasses no gate.
func TestChannelGate_OwnerIsGated(t *testing.T) {
	ts := setupTS(t)
	for _, route := range chanGateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newChanGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			enrollChanGateWebAuthn(t, ts.DB, f.owner.ID)

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.owner.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String())
			assertChanGateNothingWritten(t, ts.DB, f)

			token := mintChanGateToken(t, ts.DB, f.owner.ID, route.purpose)
			w = ts.DoRequest(route.method, route.path(f), route.body(token), testhelpers.AuthHeaders(f.owner.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.True(t, route.done(t, ts.DB, f))
		})
	}
}

// I-UNGATED: on an enforcing server, a change that does not shorten retention
// asks for nothing and charges nothing; a shortening against a finite prior
// window is gated.
func TestChannelExpirationGate_OnlyShorteningIsGated(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	path := pathChannelsPrefix + f.channelID + "/expiration"
	auth := testhelpers.AuthHeaders(f.mod.AccessToken)
	set := func(window int, code string) any {
		return withChanGateCode(map[string]any{"mode": "set", "window_seconds": window, "retroactive": "new_only"}, code)
	}

	w := ts.DoRequest(http.MethodPatch, path, set(86400, ""), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)

	w = ts.DoRequest(http.MethodPatch, path, set(604800, ""), auth)
	require.Equal(t, http.StatusOK, w.Code, "lengthening is ungated: %s", w.Body.String())
	assert.Equal(t, int64(604800), chanGateWindow(t, ts.DB, f.channelID))
	assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID), "no code, no charge")

	w = ts.DoRequest(http.MethodPatch, path, set(604800, ""), auth)
	require.Equal(t, http.StatusOK, w.Code, "the same window is not shorter: %s", w.Body.String())

	token := mintChanGateToken(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten)
	w = ts.DoRequest(http.MethodPatch, path, set(2592000, token), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, chanGateTokenLive(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten), "an ungated change verifies nothing")

	w = ts.DoRequest(http.MethodPatch, path, set(3600, ""), auth)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String(), "shorter than a finite prior window")
	assert.Equal(t, int64(2592000), chanGateWindow(t, ts.DB, f.channelID))

	w = ts.DoRequest(http.MethodPatch, path, map[string]any{"mode": "clear", "retroactive": "leave_pending"}, auth)
	require.Equal(t, http.StatusOK, w.Code, "turning expiration off is ungated: %s", w.Body.String())
	assert.Zero(t, chanGateWindow(t, ts.DB, f.channelID))

	w = ts.DoRequest(http.MethodPatch, path, set(2592000, ""), auth)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String(), "any finite window shortens an off policy")
}

// RS5 pair on an enforcing server, at each denial's own scope. DeleteChannel
// resolves ManageChannels at server scope, so a grant that exists only as a
// channel ALLOW cannot be lifted by a factor there and gets the generic 403;
// the expiration route resolves it at channel scope, where that ALLOW is what
// the mask withheld.
func TestChannelGate_RS5Pair(t *testing.T) {
	ts := setupTS(t)
	for _, route := range chanGateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newChanGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.mod.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.JSONEq(t, chanGateBodyEnrollment, w.Body.String(), "an unenrolled raw holder")

			w = ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.plain.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, chanGateBodyForbidden, w.Body.String(), "a member without the bit")

			w = ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.allowOnly.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			if route.purpose == stepup.PurposeChannelDelete {
				assert.Equal(t, chanGateBodyForbidden, w.Body.String(), "a channel ALLOW never satisfied the server-scope check")
			} else {
				assert.JSONEq(t, chanGateBodyEnrollment, w.Body.String(), "a channel ALLOW is a raw grant at channel scope")
			}
			assertChanGateNothingWritten(t, ts.DB, f)
		})
	}
}

// The channel ALLOW alone, with the toggle off, is enough at channel scope
// and never at server scope: the scope each RS5 call passes is the scope the
// denial was decided at.
func TestChannelGate_ChannelAllowScopeWithToggleOff(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	auth := testhelpers.AuthHeaders(f.allowOnly.AccessToken)

	w := ts.DoRequest(http.MethodDelete, pathChannelsPrefix+f.channelID, nil, auth)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, chanGateBodyForbidden, w.Body.String())

	w = ts.DoRequest(http.MethodPatch, pathChannelsPrefix+f.channelID+"/expiration", shortenBody(""), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// RS5 at DeleteChannel's in-transaction denial. A permission-cache entry that
// predates the change lets the pooled check pass, so the denial is decided
// under the capture's locks and answered on its transaction: the enrollment
// refusal for an unenrolled raw holder on a server that now enforces, the
// generic 403 for a member who lost the role.
func TestDeleteChannelGate_InTransactionRS5(t *testing.T) {
	ts := setupTS(t)

	t.Run("unenrolled raw holder", func(t *testing.T) {
		f := newChanGateFixture(t, ts)
		testhelpers.PublishPermissionCache(t, ts.Redis, f.serverID, f.mod.ID, "", rbac.PermManageChannels)
		testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true) // no generation bump: the entry stays served

		w := ts.DoRequest(http.MethodDelete, pathChannelsPrefix+f.channelID, nil, testhelpers.AuthHeaders(f.mod.AccessToken))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, chanGateBodyEnrollment, w.Body.String())
		assertChanGateNothingWritten(t, ts.DB, f)
	})

	t.Run("member who lost the bit", func(t *testing.T) {
		f := newChanGateFixture(t, ts)
		testhelpers.PublishPermissionCache(t, ts.Redis, f.serverID, f.plain.ID, "", rbac.PermManageChannels)
		testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)

		w := ts.DoRequest(http.MethodDelete, pathChannelsPrefix+f.channelID, nil, testhelpers.AuthHeaders(f.plain.AccessToken))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, chanGateBodyForbidden, w.Body.String())
		assertChanGateNothingWritten(t, ts.DB, f)
	})
}

// The budget is charged only when a code is present, and cleared only after a
// verified commit.
func TestChannelGate_BudgetChargedOnlyWithACode(t *testing.T) {
	ts := setupTS(t)
	for _, route := range chanGateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newChanGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)
			auth := testhelpers.AuthHeaders(f.mod.AccessToken)

			w := ts.DoRequest(route.method, route.path(f), route.body(""), auth)
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID), "no code, no charge")

			w = ts.DoRequest(route.method, route.path(f), route.body(chanGateWrongCode), auth)
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "1", chanGateBudget(t, ts, f.mod.ID), "a refused code is charged and not cleared")

			token := mintChanGateToken(t, ts.DB, f.mod.ID, route.purpose)
			w = ts.DoRequest(route.method, route.path(f), route.body(token), auth)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID), "a verified commit clears the budget")
		})
	}
}

// A malformed DeleteChannel body is a 400 before anything is read, never "no
// code" (A-6).
func TestDeleteChannelGate_MalformedBodyIsRefused(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	for _, body := range []string{`null`, `[]`, `{"mfa_code":7}`, `{"mfa_code":"1"}junk`} {
		req := httptest.NewRequest(http.MethodDelete, pathChannelsPrefix+f.channelID, bytes.NewBufferString(body))
		req.Header = testhelpers.AuthHeaders(f.mod.AccessToken)
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, body)
	}
	assert.True(t, chanGateChannelExists(t, ts.DB, f.channelID))
}

// The expiration body takes mfa_code beside the closed schema, and refuses
// everything it refused before.
func TestChannelExpirationGate_BodyContract(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	path := pathChannelsPrefix + f.channelID + "/expiration"
	auth := testhelpers.AuthHeaders(f.mod.AccessToken)
	for _, body := range []map[string]any{
		{"mode": "set", "window_seconds": 3600, "retroactive": "new_only", "mfa_code": 7},
		{"mode": "set", "window_seconds": 3600, "retroactive": "new_only", "mfa_code": string(bytes.Repeat([]byte("x"), 257))},
		{"mode": "set", "window_seconds": 3600, "retroactive": "new_only", "mfa_code": "1", "step_up_token": "t"},
		{"mode": "clear", "retroactive": "leave_pending", "window_seconds": 3600, "mfa_code": "1"},
	} {
		w := ts.DoRequest(http.MethodPatch, path, body, auth)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%v: %s", body, w.Body.String())
	}
	assertChanGateNothingWritten(t, ts.DB, f)

	w := ts.DoRequest(http.MethodPatch, path, map[string]any{"mode": "clear", "retroactive": "leave_pending", "mfa_code": nil}, auth)
	require.Equal(t, http.StatusOK, w.Code, "a null mfa_code is no code: %s", w.Body.String())
	assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID))
}

// grace: a verified deletion grants the ManageChannels grace on this server,
// which covers a second deletion and a shortening there with no verifier
// call, and never slides.
func TestChannelGate_GraceSpansBothRoutes(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	second := ts.CreateTestChannel(t, f.serverID, "second")
	third := ts.CreateTestChannel(t, f.serverID, "third")
	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	testhelpers.EnrollInlineTOTP(t, ts.DB, f.mod.ID)
	verifier := &testhelpers.FakeMFAVerifier{AcceptCode: "246810"}
	log := logger.NewWithWriter(&bytes.Buffer{})
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	handler := channels.NewHandler(ts.DB, log, ts.Hub, resolver, ts.Redis)
	handler.SetAuthorityHandler(rbac.NewHandler(ts.DB, log, ts.Redis, ts.Hub, resolver, rbac.NewPermissionCache(ts.Redis), nil))
	handler.SetMFAVerifier(verifier)
	sid := uuid.NewString()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", f.mod.ID)
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"sid": sid})
		c.Next()
	})
	router.DELETE("/api/v1/channels/:id", handler.DeleteChannel)
	router.PATCH("/api/v1/channels/:id/expiration", handler.UpdateExpiration)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	w := do(http.MethodDelete, pathChannelsPrefix+f.channelID, ``)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Zero(t, verifier.Calls(), "no code, no grace: nothing to verify")

	w = do(http.MethodDelete, pathChannelsPrefix+f.channelID, `{"mfa_code":"246810"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, verifier.Calls())
	ttlKeys, err := ts.Redis.Keys(context.Background(), "stepup:grace:"+f.mod.ID+":*").Result()
	require.NoError(t, err)
	require.Len(t, ttlKeys, 1, "a verified commit grants one grace")
	ttlBefore, err := ts.Redis.TTL(context.Background(), ttlKeys[0]).Result()
	require.NoError(t, err)

	w = do(http.MethodDelete, pathChannelsPrefix+second, ``)
	require.Equal(t, http.StatusOK, w.Code, "grace-covered deletion: %s", w.Body.String())
	w = do(http.MethodPatch, pathChannelsPrefix+third+"/expiration", `{"mode":"set","window_seconds":3600,"retroactive":"new_only"}`)
	require.Equal(t, http.StatusOK, w.Code, "grace-covered shortening: %s", w.Body.String())
	assert.Equal(t, 1, verifier.Calls(), "grace confirms with no verifier call")
	assert.Equal(t, []stepup.Purpose{stepup.PurposeChannelDelete}, verifier.Purposes())
	ttlAfter, err := ts.Redis.TTL(context.Background(), ttlKeys[0]).Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, ttlAfter, ttlBefore, "a covered action never slides the grace")
}
