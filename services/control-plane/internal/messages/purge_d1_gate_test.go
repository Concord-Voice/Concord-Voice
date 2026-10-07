package messages_test

// #3454 A-3: the D1 gate on the channel and server purge, composed with the
// self-purge soft-lock into one admission. A purge whose author filter is
// every author, or another member, is a D1 action; on a server that enforces
// MFA on dangerous actions its admission confirms under the server rule.
//
// The per-route table runs through the real router, so the verifier is the
// real MFA handler and a "code" is an inline WebAuthn step-up token minted for
// one purpose: a token minted for the other purge purpose is a real
// wrong-purpose token, refused by the token store itself. The composed
// admission tests run on the soft-lock harness (softlock_harness_test.go),
// whose verifier counts calls and spends a TOTP step inside the caller's
// transaction, and whose Redis is a miniredis the test can inspect.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

const (
	d1BodyMFARequired      = `{"error":"MFA verification required","methods":["webauthn"],"mfa_required":true}`
	d1BodyInvalidCode      = `{"error":"Invalid MFA code"}`
	d1BodyEnrollment       = `{"error":"Set up an authenticator app or security key to do this.","mfa_enrollment_required":true}`
	d1BodyChannelForbidden = `{"error":"Insufficient permissions to purge this channel"}`
	d1BodyServerForbidden  = `{"error":"Insufficient permissions to purge this server"}`
	d1WrongCode            = "123456"
	// d1Session is the access token's sid claim on the harness, which a
	// step-up grace is keyed on.
	d1Session = "6f1c0e5a-3b9d-4c47-9a51-2f6d8e0b7c13"
)

// d1Fixture is a server with one text channel, its owner, a moderator holding
// ManageAllMessages through a role, and a member whose messages the moderator
// purges.
type d1Fixture struct {
	owner, mod, author testhelpers.TestUser
	serverID, channel  string
}

func newD1Fixture(t *testing.T, ts *testhelpers.TestServer) d1Fixture {
	t.Helper()
	s := uuid.NewString()[:8]
	f := d1Fixture{
		owner:  ts.CreateTestUser(t, "d1o"+s),
		mod:    ts.CreateTestUser(t, "d1m"+s),
		author: ts.CreateTestUser(t, "d1a"+s),
	}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "D1 purge server")
	f.channel = ts.CreateTestChannel(t, f.serverID, "general")
	ts.AddMemberToServer(t, f.serverID, f.mod.ID, "member")
	ts.AddMemberToServer(t, f.serverID, f.author.ID, "member")
	ts.AssignRoleToUser(t, f.serverID, f.mod.ID, ts.CreateTestRole(t, f.serverID, "moderator", 10, int64(rbac.ModeratorPermissions)))
	ts.CreateTestMessage(t, f.channel, f.author, "one")
	ts.CreateTestMessage(t, f.channel, f.author, "two")
	return f
}

func enrollD1WebAuthn(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, NOW())`, uuid.NewString(), userID, []byte("cred-"+userID))
	require.NoError(t, err)
}

func mintD1Token(t *testing.T, db *sql.DB, userID string, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), db, userID, stepup.FactorWebAuthn, purpose, "")
	require.Nil(t, e)
	return token
}

func d1TokenLive(t *testing.T, db *sql.DB, userID string, purpose stepup.Purpose) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM step_up_tokens WHERE user_id = $1 AND purpose = $2`,
		userID, string(purpose)).Scan(&n))
	return n > 0
}

func d1Count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

// d1Route is one purge route as the moderator calls it to purge the author.
type d1Route struct {
	name             string
	purpose, other   stepup.Purpose
	path, auditScope func(f d1Fixture) string
	forbidden        string
}

func d1Routes() []d1Route {
	return []d1Route{
		{
			name: "channel purge", purpose: stepup.PurposeChannelPurge, other: stepup.PurposeServerPurge,
			path:       func(f d1Fixture) string { return purgeChannelPath(f.channel) },
			auditScope: func(f d1Fixture) string { return f.channel },
			forbidden:  d1BodyChannelForbidden,
		},
		{
			name: "server purge", purpose: stepup.PurposeServerPurge, other: stepup.PurposeChannelPurge,
			path:       func(f d1Fixture) string { return purgeServerPath(f.serverID) },
			auditScope: func(f d1Fixture) string { return f.serverID },
			forbidden:  d1BodyServerForbidden,
		},
	}
}

// assertD1NothingWritten is "a refusal writes nothing": the author's messages
// are all there and no purge audit row exists.
func assertD1NothingWritten(t *testing.T, ts *testhelpers.TestServer, f d1Fixture) {
	t.Helper()
	assert.Equal(t, 2, d1Count(t, ts.DB, `SELECT COUNT(*) FROM messages WHERE channel_id = $1`, f.channel))
	assert.Zero(t, d1Count(t, ts.DB, `SELECT COUNT(*) FROM message_purges WHERE server_id = $1`, f.serverID))
}

// d1GateCase is one cell of the per-route table.
type d1GateCase struct {
	route               d1Route
	enforcing, enrolled bool
	code                string
}

func (tc d1GateCase) name() string {
	name := tc.route.name
	if tc.enforcing {
		name += "/enforcing"
	}
	if tc.enrolled {
		name += "/enrolled"
	}
	return name + "/" + tc.code
}

// d1GateCases is spec §11's table: route × toggle × enrolled × {no code, wrong
// code, valid code, a WebAuthn token for the other purge's purpose}.
func d1GateCases() []d1GateCase {
	routes := d1Routes()
	cases := make([]d1GateCase, 0, len(routes)*16)
	for _, route := range routes {
		for _, enforcing := range []bool{false, true} {
			for _, enrolled := range []bool{false, true} {
				for _, code := range []string{"none", "wrong", "valid", "other purpose"} {
					cases = append(cases, d1GateCase{route: route, enforcing: enforcing, enrolled: enrolled, code: code})
				}
			}
		}
	}
	return cases
}

// d1GateBody is the purge request body for one cell: the "none" row sends no
// mfa_code at all.
func d1GateBody(f d1Fixture, code, valid, other string) map[string]any {
	body := map[string]any{"range": "all", "target_user_id": f.author.ID}
	if sent := map[string]string{"wrong": d1WrongCode, "valid": valid, "other purpose": other}[code]; sent != "" {
		body["mfa_code"] = sent
	}
	return body
}

// TestPurgeD1Gate_Table is spec §11's per-route table for both purge routes:
// toggle × enrolled × {no code, wrong code, valid code, a WebAuthn token for
// the other purge's purpose}. The unenrolled rows are also RS5's: the mask
// withholds ManageAllMessages from the moderator, and the preflight denial
// (resolvePurgeAuthor on the channel route, serverPurgeDeletes on the server
// route) answers mfa_enrollment_required.
//
// Mutants killed: deleting the composed admission's D1 arm (the enforcing
// "none" rows purge); spending a token under any purpose but the route's (the
// "other purpose" rows succeed); dropping RS5 from either preflight site (the
// unenrolled rows read the generic 403).
func TestPurgeD1Gate_Table(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, tc := range d1GateCases() {
		t.Run(tc.name(), func(t *testing.T) {
			f := newD1Fixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, tc.enforcing)
			if tc.enrolled {
				enrollD1WebAuthn(t, ts.DB, f.mod.ID)
			}
			valid := mintD1Token(t, ts.DB, f.mod.ID, tc.route.purpose)
			other := mintD1Token(t, ts.DB, f.mod.ID, tc.route.other)

			w := ts.DoRequest(http.MethodDelete, tc.route.path(f), d1GateBody(f, tc.code, valid, other), testhelpers.AuthHeaders(f.mod.AccessToken))

			assert.True(t, d1TokenLive(t, ts.DB, f.mod.ID, tc.route.other), "a token for another purpose is never spent")
			assertD1GateOutcome(t, ts, f, tc, w)
		})
	}
}

// assertD1GateOutcome is the table's expected answer for one cell.
func assertD1GateOutcome(t *testing.T, ts *testhelpers.TestServer, f d1Fixture, tc d1GateCase, w *httptest.ResponseRecorder) {
	t.Helper()
	switch {
	case !tc.enforcing:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.JSONEq(t, `{"deleted_count":2,"hidden_count":0}`, w.Body.String())
		assert.True(t, d1TokenLive(t, ts.DB, f.mod.ID, tc.route.purpose), "nothing is verified off the toggle")
	case !tc.enrolled:
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, d1BodyEnrollment, w.Body.String(), "RS5 at the masked preflight denial")
		assertD1NothingWritten(t, ts, f)
	case tc.code == "valid":
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Zero(t, d1Count(t, ts.DB, `SELECT COUNT(*) FROM messages WHERE channel_id = $1`, f.channel))
		assert.False(t, d1TokenLive(t, ts.DB, f.mod.ID, tc.route.purpose), "the verified token is spent on commit")
	case tc.code == "none":
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, d1BodyMFARequired, w.Body.String())
		assert.Empty(t, w.Header().Get("Retry-After"), "waiting lifts nothing")
		assertD1NothingWritten(t, ts, f)
	default:
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.JSONEq(t, d1BodyInvalidCode, w.Body.String())
		assertD1NothingWritten(t, ts, f)
		assert.True(t, d1TokenLive(t, ts.DB, f.mod.ID, tc.route.purpose))
	}
}

// TestPurgeD1Gate_RS5MemberWithoutTheBit: a member who holds no
// ManageAllMessages at all, raw or masked, asking to purge another author on
// an enforcing server gets each route's generic 403, byte for byte: the mask
// did not cause that denial, so naming enrollment would be false.
//
// Mutant killed: answering RS5 from the masked bits alone (the member is told
// to enroll).
func TestPurgeD1Gate_RS5MemberWithoutTheBit(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	for _, route := range d1Routes() {
		t.Run(route.name, func(t *testing.T) {
			f := newD1Fixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			body := map[string]any{"range": "all", "target_user_id": f.mod.ID}
			w := ts.DoRequest(http.MethodDelete, route.path(f), body, testhelpers.AuthHeaders(f.author.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, route.forbidden, w.Body.String())
		})
	}
}

// d1Purge is one purge call on the soft-lock harness, carrying the grace
// session.
func (s *softLockHarness) d1Purge(t *testing.T, userID, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return s.do(t, request{method: http.MethodDelete, path: path, userID: userID, session: d1Session, body: body})
}

// totpSteps reads how many codes softLockVerifier spent for userID.
func (s *softLockHarness) totpSteps(t *testing.T, userID string) int {
	t.Helper()
	var n sql.NullInt64
	require.NoError(t, s.ts.DB.QueryRow(`SELECT last_used_step FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&n))
	return int(n.Int64)
}

// requireD1Refusal asserts A-3.4's plain seam body: a 403 carrying flag and
// neither delete_rate_limited nor Retry-After.
func requireD1Refusal(t *testing.T, w *httptest.ResponseRecorder, flag string) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	body := decode(t, w)
	assert.NotContains(t, body, "delete_rate_limited", "a D1 refusal is never the soft-lock's")
	assert.Empty(t, w.Header().Get("Retry-After"), "waiting lifts nothing")
	if flag != "" {
		assert.Equal(t, true, body[flag], "refusal must carry %s", flag)
	}
	return body
}

// mixedWorld adds a second channel in which the moderator's ManageAllMessages
// is denied by a member override, so a server purge with no target builds a
// D1 spec for the first channel (every author) and a self spec for the second
// (ManageOwn forces the moderator's own messages): one plan both rules apply
// to. It seeds ownN of the moderator's messages there and three of the
// author's in the first channel.
func (s *softLockHarness) mixedWorld(t *testing.T, w softLockWorld, ownN int) {
	t.Helper()
	denied := s.ts.CreateTestChannel(t, w.serverID, "own-only")
	_, err := s.ts.DB.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'user', $3, 0, $4)`, uuid.NewString(), denied, w.moderator.ID, int64(rbac.PermManageAllMessages))
	require.NoError(t, err)
	s.seed(t, denied, w.moderator, ownN)
	s.seed(t, w.channelID, w.author, 3)
}

// TestPurgeComposedAdmission_MixedPlanVerifiesOnce: on an enforcing server, a
// server purge with a D1 spec and an over-threshold self spec is confirmed by
// ONE verification under the server purge's purpose, which spends one TOTP
// step, and the commit grants both graces it earned (D1's and the delete
// scope's) and clears the budget.
//
// Mutants killed: confirming D1 and the soft-lock separately (two verifier
// calls; the second TOTP verify is a replay in production); granting only
// one of the graces.
func TestPurgeComposedAdmission_MixedPlanVerifiesOnce(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.moderator.ID)
	s.mixedWorld(t, w, 16)

	res := s.d1Purge(t, w.moderator.ID, "/servers/"+w.serverID+"/messages",
		map[string]any{"range": "all", "mfa_code": softLockValidCode})

	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.EqualValues(t, 19, decode(t, res)["deleted_count"])
	assert.Equal(t, []stepup.Purpose{stepup.PurposeServerPurge}, s.verifier.calls(), "one verification for both rules")
	assert.Equal(t, 1, s.totpSteps(t, w.moderator.ID), "one TOTP step spent")
	assert.Zero(t, s.countBy(t, w.author.ID))
	assert.Zero(t, s.countBy(t, w.moderator.ID))
	assert.Len(t, s.graceKeys(w.moderator.ID), 2, "the D1 grace and the delete-scope grace")
	assert.Empty(t, s.counter(budgetKey(w.moderator.ID)), "the budget is cleared once the admission committed")
	assert.Empty(t, s.counter(burstKey(w.moderator.ID, w.serverID)), "and the counters reset after the purge")
}

// TestPurgeComposedAdmission_D1RefusalBody: a refusal of a plan with a D1 spec
// is D1's plain body (A-3.4) — no delete_rate_limited, no Retry-After — even
// when the soft-lock tripped too, and it purges nothing and writes no audit
// row. A plan with no D1 spec keeps the soft-lock's decoration (the control).
//
// Mutants killed: deleting the composed admission's D1 arm (the D1-only purge
// runs); answering a D1 plan through respondSoftLockError (the flag and the
// header appear).
func TestPurgeComposedAdmission_D1RefusalBody(t *testing.T) {
	t.Run("D1 only, no code", func(t *testing.T) {
		s := newSoftLockHarness(t)
		w := s.world(t, true)
		s.enroll(t, w.moderator.ID)
		s.seed(t, w.channelID, w.author, 3)
		requireD1Refusal(t, s.d1Purge(t, w.moderator.ID, "/channels/"+w.channelID+"/messages",
			map[string]any{"range": "all", "target_user_id": w.author.ID}), "mfa_required")
		assert.Equal(t, 3, s.countBy(t, w.author.ID))
		assert.Zero(t, s.auditRows(t, w.channelID))
	})
	t.Run("D1 and the soft-lock, wrong code", func(t *testing.T) {
		s := newSoftLockHarness(t)
		w := s.world(t, true)
		s.enroll(t, w.moderator.ID)
		s.mixedWorld(t, w, 16)
		body := requireD1Refusal(t, s.d1Purge(t, w.moderator.ID, "/servers/"+w.serverID+"/messages",
			map[string]any{"range": "all", "mfa_code": "000000"}), "")
		assert.Equal(t, "Invalid MFA code", body["error"])
		assert.Equal(t, 3, s.countBy(t, w.author.ID))
		assert.Equal(t, 16, s.countBy(t, w.moderator.ID))
		assert.Zero(t, s.auditRows(t, w.serverID))
	})
	t.Run("control: soft-lock only keeps its decoration", func(t *testing.T) {
		s := newSoftLockHarness(t)
		w := s.world(t, true)
		s.enroll(t, w.author.ID)
		s.seed(t, w.channelID, w.author, 16)
		requireSoftLockRefusal(t, s.d1Purge(t, w.author.ID, "/channels/"+w.channelID+"/messages",
			map[string]any{"range": "all"}), "mfa_required")
	})
}

// TestPurgeComposedAdmission_BudgetChargedOnce: the budget is charged at most
// once per request (A-3.5). With both rules applying, the soft-lock charges
// the code and D1 does not charge it again; with D1 alone, D1 charges it; a
// request with no code is never charged; and a refused confirmation is not
// cleared.
//
// Mutants killed: charging D1 whatever the soft-lock did (the mixed counter
// reads 2); never charging D1 (the D1-only counter is empty); charging a
// codeless request.
func TestPurgeComposedAdmission_BudgetChargedOnce(t *testing.T) {
	s := newSoftLockHarness(t)

	mixed := s.world(t, true)
	s.enroll(t, mixed.moderator.ID)
	s.mixedWorld(t, mixed, 16)
	requireD1Refusal(t, s.d1Purge(t, mixed.moderator.ID, "/servers/"+mixed.serverID+"/messages",
		map[string]any{"range": "all", "mfa_code": "000000"}), "")
	assert.Equal(t, "1", s.counter(budgetKey(mixed.moderator.ID)), "both rules apply: charged once")

	only := s.world(t, true)
	s.enroll(t, only.moderator.ID)
	s.seed(t, only.channelID, only.author, 3)
	path := "/channels/" + only.channelID + "/messages"
	requireD1Refusal(t, s.d1Purge(t, only.moderator.ID, path,
		map[string]any{"range": "all", "target_user_id": only.author.ID}), "mfa_required")
	assert.Empty(t, s.counter(budgetKey(only.moderator.ID)), "no code, no charge")
	requireD1Refusal(t, s.d1Purge(t, only.moderator.ID, path,
		map[string]any{"range": "all", "target_user_id": only.author.ID, "mfa_code": "000000"}), "")
	assert.Equal(t, "1", s.counter(budgetKey(only.moderator.ID)), "D1 alone charges the code")
}

// TestPurgeD1Gate_ToggleOffByteIdentical: on a server that does not enforce, a
// D1 purge with no code answers exactly as before #3454 — the same status,
// body and headers — asks for no factor, and leaves Redis untouched (the grace
// pre-read is a read); a purge the actor may not make keeps its generic 403.
//
// Mutant killed: charging, or confirming, regardless of the flag.
func TestPurgeD1Gate_ToggleOffByteIdentical(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.seed(t, w.channelID, w.author, 3)
	s.seed(t, w.channelID, w.owner, 2)

	channel := s.d1Purge(t, w.moderator.ID, "/channels/"+w.channelID+"/messages",
		map[string]any{"range": "all", "target_user_id": w.author.ID})
	require.Equal(t, http.StatusOK, channel.Code)
	assert.Equal(t, `{"deleted_count":3,"hidden_count":0}`, channel.Body.String())
	assert.Empty(t, channel.Header().Get("Retry-After"))

	server := s.d1Purge(t, w.moderator.ID, "/servers/"+w.serverID+"/messages", map[string]any{"range": "all"})
	require.Equal(t, http.StatusOK, server.Code)
	assert.Equal(t, `{"deleted_count":2,"hidden_count":0}`, server.Body.String())

	denied := s.d1Purge(t, w.author.ID, "/channels/"+w.channelID+"/messages",
		map[string]any{"range": "all", "target_user_id": w.moderator.ID})
	require.Equal(t, http.StatusForbidden, denied.Code)
	assert.Equal(t, d1BodyChannelForbidden, denied.Body.String())

	assert.Empty(t, s.verifier.calls(), "nothing is verified off the toggle")
	assert.Empty(t, s.mr.Keys(), "and nothing is written to Redis")
}

// TestPurgeComposedAdmission_UnknownAdmissionSettlesNothing: a D1 purge whose
// admission COMMIT reports an error after the server committed. When the
// reconciling read proves the commit, the purge runs and settles: the budget
// is cleared and the D1 grace granted. When that read fails too, the outcome
// is ErrAdmissionUnknown: a 500, nothing purged, and nothing settled — the
// budget stays charged and no grace is granted, although the factor and the
// audit row did commit.
//
// Mutant killed: settling on ErrAdmissionUnknown (the budget is cleared and a
// grace key appears).
func TestPurgeComposedAdmission_UnknownAdmissionSettlesNothing(t *testing.T) {
	errAckLost := errors.New("connection lost after COMMIT was sent")
	for _, failRead := range []bool{false, true} {
		name := "acknowledgement lost"
		if failRead {
			name = "reconciling read fails too"
		}
		t.Run(name, func(t *testing.T) {
			s, hook := newHookedSoftLockHarness(t)
			w := s.world(t, true)
			s.enroll(t, w.moderator.ID)
			s.seed(t, w.channelID, w.author, 3)
			if failRead {
				hook.Arm([]string{"SELECT EXISTS (SELECT 1 FROM message_purges WHERE id"}, nil, stmthook.ErrInjected)
			}
			s.handler.SetBeforeSoftLockConfirmHookForTest(func() { hook.ArmCommit(true, errAckLost) })

			res := s.d1Purge(t, w.moderator.ID, "/channels/"+w.channelID+"/messages",
				map[string]any{"range": "all", "target_user_id": w.author.ID, "mfa_code": softLockValidCode})

			assert.Equal(t, 1, s.totpSteps(t, w.moderator.ID), "the admission committed, spending the factor")
			assert.Equal(t, 1, s.auditRows(t, w.channelID), "with its audit row")
			if !failRead {
				require.Equal(t, http.StatusOK, res.Code, res.Body.String())
				assert.Zero(t, s.countBy(t, w.author.ID))
				assert.Empty(t, s.counter(budgetKey(w.moderator.ID)), "the budget is cleared")
				assert.Len(t, s.graceKeys(w.moderator.ID), 1, "and the D1 grace granted")
				return
			}
			require.Equal(t, http.StatusInternalServerError, res.Code, res.Body.String())
			assert.Equal(t, 3, s.countBy(t, w.author.ID), "nothing is purged on an unproven admission")
			assert.Equal(t, "1", s.counter(budgetKey(w.moderator.ID)), "nothing is settled: the budget stays charged")
			assert.Empty(t, s.graceKeys(w.moderator.ID), "and no grace is granted")
		})
	}
}

// TestPurgeComposedAdmission_AuthorityRecheckedBeforeConfirmation is I7 in
// the admission (A-3.2): the moderator loses ManageAllMessages between the
// preflight and the admission on an enforcing server. The admission re-runs
// the plan's authority check over the D1 spec before it confirms anything,
// so the refusal is the route's own 403, never mfa_required, and nothing is
// verified, purged or audited.
//
// Mutant killed: confirming before the authority check (the answer is
// mfa_required, telling an actor who lost the authority what it would take).
func TestPurgeComposedAdmission_AuthorityRecheckedBeforeConfirmation(t *testing.T) {
	for _, route := range []struct{ name, forbidden string }{
		{name: "channel purge", forbidden: d1BodyChannelForbidden},
		{name: "server purge", forbidden: d1BodyServerForbidden},
	} {
		t.Run(route.name, func(t *testing.T) {
			s := newSoftLockHarness(t)
			w := s.world(t, true)
			s.enroll(t, w.moderator.ID)
			s.seed(t, w.channelID, w.author, 3)
			s.handler.SetBeforeSoftLockConfirmHookForTest(func() {
				_, err := s.ts.DB.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2
					AND role_id IN (SELECT id FROM roles WHERE server_id = $1 AND NOT is_default)`, w.serverID, w.moderator.ID)
				require.NoError(t, err)
			})
			path := "/channels/" + w.channelID + "/messages"
			if route.name == "server purge" {
				path = "/servers/" + w.serverID + "/messages"
			}

			res := s.d1Purge(t, w.moderator.ID, path, map[string]any{"range": "all", "target_user_id": w.author.ID})

			require.Equal(t, http.StatusForbidden, res.Code, res.Body.String())
			assert.Equal(t, route.forbidden, res.Body.String())
			assert.Empty(t, s.verifier.calls())
			assert.Equal(t, 3, s.countBy(t, w.author.ID))
			assert.Zero(t, s.auditRows(t, w.channelID)+s.auditRows(t, w.serverID))
		})
	}
}

// TestPurgeProvenance_BanKickPin: the ban/kick path keeps its pooled audit row
// and no admission (A-3.7); the provenance its transaction recorded decides
// the per-batch recheck. A purge whose ban committed while the server did not
// enforce (unconfirmed), or one that never set a provenance (the zero value),
// is refused once the server enforces; a confirmed one, and a self spec, never
// are; and an unconfirmed purge on a server that still does not enforce runs.
//
// Mutants killed: dropping the recheck from guardServerPurgeBatch (the
// unconfirmed purge runs); reading the zero value as anything but unconfirmed;
// refusing a confirmed provenance.
func TestPurgeProvenance_BanKickPin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enforcing  bool
		provenance messages.PurgeProvenance
		want       messages.PurgeStatus
	}{
		{name: "unconfirmed, then the server enforces", enforcing: true, provenance: messages.PurgeUnconfirmed, want: messages.PurgeFailed},
		{name: "zero value, then the server enforces", enforcing: true, want: messages.PurgeFailed},
		{name: "confirmed, the server enforces", enforcing: true, provenance: messages.PurgeConfirmed, want: messages.PurgeCompleted},
		{name: "unconfirmed, still not enforcing", provenance: messages.PurgeUnconfirmed, want: messages.PurgeCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSoftLockHarness(t)
			w := s.world(t, false)
			s.enroll(t, w.moderator.ID)
			s.seed(t, w.channelID, w.author, 3)
			s.setEnforcing(t, w.serverID, tc.enforcing)

			deleted, status, err := s.handler.PurgeUserServerMessages(context.Background(),
				w.serverID, w.moderator.ID, w.author.ID, "ban", tc.provenance)

			assert.Equal(t, tc.want, status)
			if tc.want == messages.PurgeFailed {
				require.Error(t, err)
				assert.Zero(t, deleted)
				assert.Equal(t, 3, s.countBy(t, w.author.ID), "a refused batch deletes nothing")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 3, deleted)
			assert.Zero(t, s.countBy(t, w.author.ID))
		})
	}
}
