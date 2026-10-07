package servers_test

// #3454 T2: the dangerous-action gates on UpdateServer (PATCH
// /api/v1/servers/:id) and DeleteServer (DELETE /api/v1/servers/:id), driven
// over HTTP through the real router and the real MFA verifier. Each test names
// the mutant that kills it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// The submissions the per-route table sends.
const (
	gateNoCode        = "no code"
	gateWrongCode     = "wrong code"
	gateValidCode     = "valid code"
	gateOtherWebAuthn = "WebAuthn token minted for another purpose"
	gateOwnWebAuthn   = "WebAuthn token minted for this purpose"
)

// The answers a gated route can give, classified.
const (
	gateOK          = "ok"
	gateMFARequired = "mfa required"
	gateInvalidCode = "invalid code"
	gateEnrollment  = "enrollment required"
)

// gateMFARequiredBody is the no-code refusal for a TOTP-enrolled actor,
// spelled out so a changed body fails here.
const gateMFARequiredBody = `{"error":"MFA verification required","mfa_required":true,"methods":["totp"]}`

// gateUpdateForbiddenBody is UpdateServer's generic 403, byte for byte.
const gateUpdateForbiddenBody = `{"error":"insufficient permissions"}`

// gateDeleteNotOwnerBody is DeleteServer's non-owner 403, byte for byte.
const gateDeleteNotOwnerBody = `{"error":"Only the server owner can delete the server"}`

const gateRenamed = "Gated rename"

// gateRoute is one gated route: how to call it, whether its write landed, and
// the purpose its WebAuthn token is minted for (otherPurpose is a sibling D1
// purpose, the most plausible token to replay).
type gateRoute struct {
	name                  string
	purpose, otherPurpose stepup.Purpose
	call                  func(env *mfaEnv, token, serverID, code string) *httptest.ResponseRecorder
	applied               func(t *testing.T, env *mfaEnv, serverID string) bool
	requireOKBody         func(t *testing.T, w *httptest.ResponseRecorder, msg string)
}

func gateRoutes() []gateRoute {
	return []gateRoute{
		{
			name: "UpdateServer", purpose: stepup.PurposeServerUpdate, otherPurpose: stepup.PurposeServerDelete,
			call: func(env *mfaEnv, token, serverID, code string) *httptest.ResponseRecorder {
				body := map[string]any{"name": gateRenamed}
				if code != "" {
					body["mfa_code"] = code
				}
				return env.ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+serverID, body, testhelpers.AuthHeaders(token))
			},
			applied: func(t *testing.T, env *mfaEnv, serverID string) bool {
				t.Helper()
				var name string
				require.NoError(t, env.ts.DB.QueryRow(`SELECT name FROM servers WHERE id = $1`, serverID).Scan(&name))
				return name == gateRenamed
			},
			requireOKBody: requireUpdateServerBody,
		},
		{
			name: "DeleteServer", purpose: stepup.PurposeServerDelete, otherPurpose: stepup.PurposeServerUpdate,
			call: func(env *mfaEnv, token, serverID, code string) *httptest.ResponseRecorder {
				var body any
				if code != "" {
					body = map[string]any{"mfa_code": code}
				}
				return env.ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+serverID, body, testhelpers.AuthHeaders(token))
			},
			applied: func(t *testing.T, env *mfaEnv, serverID string) bool {
				t.Helper()
				var exists bool
				require.NoError(t, env.ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM servers WHERE id = $1)`, serverID).Scan(&exists))
				return !exists
			},
			requireOKBody: func(t *testing.T, w *httptest.ResponseRecorder, msg string) {
				t.Helper()
				require.Equal(t, http.StatusOK, w.Code, msg)
				require.JSONEq(t, `{"message":"Server deleted successfully"}`, w.Body.String(), msg)
			},
		},
	}
}

// requireUpdateServerBody pins UpdateServer's success shape: exactly the two
// keys it has always had, the owner's legacy role, and the new name.
func requireUpdateServerBody(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "%s: %s", msg, w.Body.String())
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	require.Equal(t, []string{"role", "server"}, keys, msg)
	require.JSONEq(t, `"owner"`, string(body["role"]), msg)
	var server map[string]any
	require.NoError(t, json.Unmarshal(body["server"], &server))
	require.Equal(t, gateRenamed, server["name"], msg)
	for k := range server {
		require.NotContains(t, strings.ToLower(k), "mfa", msg)
	}
}

// gateFixture is a fresh owner and server per case, so no case inherits a
// rate-limit bucket, a spent code or a deleted row from another.
type gateFixture struct {
	owner    testhelpers.TestUser
	serverID string
}

func newGateFixture(t *testing.T, env *mfaEnv, enforcing, enrolled bool) gateFixture {
	t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	f := gateFixture{owner: env.ts.CreateTestUser(t, "g"+tag)}
	f.serverID = env.ts.CreateTestServer(t, f.owner.ID, "Gate "+tag)
	if enrolled {
		enrollMFATOTP(t, env, f.owner.ID)
	}
	setMFAFlag(t, env, f.serverID, enforcing)
	return f
}

// mintWebAuthnToken mints an unspent WebAuthn inline token for purpose, as
// WebAuthnVerifyInlineFinish does.
func mintWebAuthnToken(t *testing.T, env *mfaEnv, userID string, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), env.ts.DB, userID, stepup.FactorWebAuthn, purpose, "")
	require.Nil(t, e)
	return token
}

func unspentTokens(t *testing.T, env *mfaEnv, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, env.ts.DB.QueryRow(`SELECT count(*) FROM step_up_tokens WHERE user_id = $1`, userID).Scan(&n))
	return n
}

func classifyGate(t *testing.T, r gateRoute, w *httptest.ResponseRecorder, msg string) string {
	t.Helper()
	body := w.Body.String()
	switch {
	case w.Code == http.StatusOK:
		r.requireOKBody(t, w, msg)
		return gateOK
	case w.Code == http.StatusForbidden && sameJSON(t, body, gateMFARequiredBody):
		return gateMFARequired
	case w.Code == http.StatusForbidden && sameJSON(t, body, mfaErrorBody(stepup.ErrMsgInvalidMFACode)):
		return gateInvalidCode
	case w.Code == http.StatusForbidden && strings.Contains(body, `"mfa_enrollment_required":true`):
		requireEnrollmentRequired(t, w, msg)
		return gateEnrollment
	}
	return fmt.Sprintf("unexpected %d %s", w.Code, body)
}

// sameJSON reports whether got is the JSON document want, whatever the key
// order.
func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil {
		return false
	}
	require.NoError(t, json.Unmarshal([]byte(want), &w))
	return reflect.DeepEqual(g, w)
}

func wantGate(enforcing, enrolled bool, submission string) string {
	switch {
	case !enforcing:
		return gateOK
	case !enrolled:
		return gateEnrollment
	case submission == gateNoCode:
		return gateMFARequired
	case submission == gateValidCode, submission == gateOwnWebAuthn:
		return gateOK
	default:
		return gateInvalidCode
	}
}

// The per-route table: setting x enrollment x submission. A refusal writes
// nothing; a server that does not enforce verifies nothing (the backup code
// and a token minted for this very purpose stay unspent); a WebAuthn token
// minted for the sibling purpose is refused and stays unspent.
// Kills: either route's Require call removed (enforcing + enrolled + no code
// answers 200); Require run with the wrong purpose (the own-purpose token is
// refused, or the other-purpose token accepted); fires=false.
func TestDangerousGate_PerRouteTable(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	submissions := []string{gateNoCode, gateWrongCode, gateValidCode, gateOtherWebAuthn, gateOwnWebAuthn}
	for _, r := range gateRoutes() {
		for _, enforcing := range []bool{false, true} {
			for _, enrolled := range []bool{false, true} {
				for _, submission := range submissions {
					name := fmt.Sprintf("%s/enforcing=%t/enrolled=%t/%s", r.name, enforcing, enrolled, submission)
					t.Run(name, func(t *testing.T) {
						runGateCase(t, env, r, enforcing, enrolled, submission)
					})
				}
			}
		}
	}
}

func runGateCase(t *testing.T, env *mfaEnv, r gateRoute, enforcing, enrolled bool, submission string) {
	f := newGateFixture(t, env, enforcing, enrolled)
	code := ""
	switch submission {
	case gateWrongCode:
		code = mfaWrongCode
	case gateValidCode:
		code = mfaBackupCode
	case gateOtherWebAuthn:
		code = mintWebAuthnToken(t, env, f.owner.ID, r.otherPurpose)
	case gateOwnWebAuthn:
		code = mintWebAuthnToken(t, env, f.owner.ID, r.purpose)
	}
	tokensBefore := unspentTokens(t, env, f.owner.ID)

	w := r.call(env, f.owner.AccessToken, f.serverID, code)
	want := wantGate(enforcing, enrolled, submission)
	require.Equal(t, want, classifyGate(t, r, w, submission), w.Body.String())
	assert.Equal(t, want == gateOK, r.applied(t, env, f.serverID), "the write lands exactly when the gate admits")

	verified := enforcing && enrolled
	if enrolled {
		assert.Equal(t, verified && submission == gateValidCode, backupCodeSpent(t, env, f.owner.ID),
			"the backup code is spent only by a verified, committed confirmation")
	}
	spentToken := verified && submission == gateOwnWebAuthn
	wantTokens := tokensBefore
	if spentToken {
		wantTokens--
	}
	assert.Equal(t, wantTokens, unspentTokens(t, env, f.owner.ID),
		"a token is spent only under its own purpose on an enforcing server")
}

// With the setting off, a request that carries no code is answered exactly as
// before #3454: the same status and body shape, no budget charge, and no grace
// written.
// Kills: Charge run without a code; a grace granted on an unverified success.
func TestDangerousGate_SettingOffAndNoCodeIsUnchanged(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	for _, r := range gateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newGateFixture(t, env, false, true)
			w := r.call(env, f.owner.AccessToken, f.serverID, "")
			r.requireOKBody(t, w, r.name)
			assert.True(t, r.applied(t, env, f.serverID))
			assert.Equal(t, 0, budgetCount(t, env, f.owner.ID), "a request with no code is never charged")
			graces, err := env.ts.Redis.Keys(context.Background(), "stepup:grace:"+f.owner.ID+":*").Result()
			require.NoError(t, err)
			assert.Empty(t, graces, "an unverified success grants nothing")
		})
	}
}

// The attempt budget is charged before the transaction only when a code is
// sent, stands on a refusal and on an unverified success, and is cleared only
// after a verified commit.
// Kills: Charge dropped or run on every request; the budget cleared on a
// refusal or on an unconfirmed success; Settle skipped after a verified commit.
func TestDangerousGate_BudgetIsChargedOnlyWithACode(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	for _, r := range gateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			off := newGateFixture(t, env, false, true)
			r.requireOKBody(t, r.call(env, off.owner.AccessToken, off.serverID, mfaWrongCode), "not enforcing, wrong code")
			assert.Equal(t, 1, budgetCount(t, env, off.owner.ID),
				"a code is charged whatever the setting, and an unverified success clears nothing")

			on := newGateFixture(t, env, true, true)
			w := r.call(env, on.owner.AccessToken, on.serverID, "")
			require.Equal(t, gateMFARequired, classifyGate(t, r, w, "no code"))
			assert.Equal(t, 0, budgetCount(t, env, on.owner.ID), "a refusal with no code is not charged")

			w = r.call(env, on.owner.AccessToken, on.serverID, mfaWrongCode)
			require.Equal(t, gateInvalidCode, classifyGate(t, r, w, "wrong code"))
			assert.Equal(t, 1, budgetCount(t, env, on.owner.ID), "a refused code stays charged")

			w = r.call(env, on.owner.AccessToken, on.serverID, mfaBackupCode)
			require.Equal(t, gateOK, classifyGate(t, r, w, "valid code"))
			assert.Equal(t, 0, budgetCount(t, env, on.owner.ID), "a verified commit clears the budget")
		})
	}
}

// RS5 at UpdateServer's pooled denial: on an enforcing server, a member who
// would hold ManageServer without the mask (through the bit itself or raw
// Administrator) and has no inline factor is told to enroll; a member without
// the bit, enrolled or not, and a non-member get the unchanged generic 403.
// Nothing is written.
// Kills: rbac.EnrollmentDenial removed from the pooled denial (the raw holders
// get the generic 403); EnrollmentDenial called with a bit the member lacks.
func TestDangerousGate_UpdateServerRS5(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	ts := env.ts
	f := newGateFixture(t, env, true, true)
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	manage := ts.CreateTestRole(t, f.serverID, "manage-"+tag, 4, int64(rbac.PermManageServer))
	admin := ts.CreateTestRole(t, f.serverID, "admin-"+tag, 5, int64(rbac.PermAdministrator))
	other := ts.CreateTestRole(t, f.serverID, "other-"+tag, 3, int64(rbac.PermManageChannels))
	persona := func(name, roleID string, enrolled bool) testhelpers.TestUser {
		u := ts.CreateTestUser(t, name+tag)
		ts.AddMemberToServer(t, f.serverID, u.ID, "member")
		ts.AssignRoleToUser(t, f.serverID, u.ID, roleID)
		if enrolled {
			enrollMFATOTP(t, env, u.ID)
		}
		return u
	}
	update := gateRoutes()[0]

	for name, u := range map[string]testhelpers.TestUser{
		"unenrolled ManageServer holder": persona("rsm", manage, false),
		"unenrolled raw Administrator":   persona("rsa", admin, false),
	} {
		requireEnrollmentRequired(t, update.call(env, u.AccessToken, f.serverID, ""), name)
	}
	outsider := ts.CreateTestUser(t, "rso"+tag)
	for name, u := range map[string]testhelpers.TestUser{
		"unenrolled member without the bit": persona("rsu", other, false),
		"enrolled member without the bit":   persona("rse", other, true),
		"non-member":                        outsider,
	} {
		w := update.call(env, u.AccessToken, f.serverID, mfaBackupCode)
		assert.Equal(t, http.StatusForbidden, w.Code, name)
		assert.Equal(t, gateUpdateForbiddenBody, w.Body.String(), name)
	}
	assert.False(t, update.applied(t, env, f.serverID), "no refusal writes")
}

// The gate transaction re-checks ManageServer under its locks, and answers
// that denial with RS5 on the transaction. A planted permission-cache entry
// stands in for a grant revoked (or a setting turned on) after the pooled
// check read it.
// Kills: the in-transaction ManageServer check removed (the revoked member's
// edit lands); the in-transaction EnrollmentDenial removed (the unenrolled
// holder gets the generic 403).
func TestDangerousGate_UpdateServerRechecksUnderTheGate(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	ts := env.ts
	update := gateRoutes()[0]
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	revoked := newGateFixture(t, env, false, false)
	member := ts.CreateTestUser(t, "rv"+tag)
	ts.AddMemberToServer(t, revoked.serverID, member.ID, "member")
	testhelpers.PublishPermissionCache(t, ts.Redis, revoked.serverID, member.ID, "", rbac.PermManageServer)
	w := update.call(env, member.AccessToken, revoked.serverID, "")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, gateUpdateForbiddenBody, w.Body.String())
	assert.False(t, update.applied(t, env, revoked.serverID))

	masked := newGateFixture(t, env, true, false)
	holder := ts.CreateTestUser(t, "mh"+tag)
	ts.AddMemberToServer(t, masked.serverID, holder.ID, "member")
	ts.AssignRoleToUser(t, masked.serverID, holder.ID,
		ts.CreateTestRole(t, masked.serverID, "manage-"+tag, 4, int64(rbac.PermManageServer)))
	testhelpers.PublishPermissionCache(t, ts.Redis, masked.serverID, holder.ID, "", rbac.PermManageServer)
	requireEnrollmentRequired(t, update.call(env, holder.AccessToken, masked.serverID, ""), "in-transaction RS5")
	assert.False(t, update.applied(t, env, masked.serverID))
}

// RS5 at DeleteServer has only its owner arm (§14.1): an unenrolled owner of
// an enforcing server is told to enroll, by Require itself. A non-owner,
// even a raw Administrator, enrolled or not, keeps the unchanged owner-only
// 403: no factor lifts an identity check.
// Kills: the gate skipped for the owner; the owner check moved after Require
// (a non-owner would learn the server enforces).
func TestDangerousGate_DeleteServerRS5OwnerArm(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	ts := env.ts
	del := gateRoutes()[1]

	f := newGateFixture(t, env, true, false)
	requireEnrollmentRequired(t, del.call(env, f.owner.AccessToken, f.serverID, ""), "unenrolled owner")
	assert.False(t, del.applied(t, env, f.serverID))

	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	admin := ts.CreateTestRole(t, f.serverID, "admin-"+tag, 5, int64(rbac.PermAdministrator))
	for _, enrolled := range []bool{false, true} {
		u := ts.CreateTestUser(t, fmt.Sprintf("da%t%s", enrolled, tag))
		ts.AddMemberToServer(t, f.serverID, u.ID, "member")
		ts.AssignRoleToUser(t, f.serverID, u.ID, admin)
		if enrolled {
			enrollMFATOTP(t, env, u.ID)
		}
		w := del.call(env, u.AccessToken, f.serverID, mfaBackupCode)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, gateDeleteNotOwnerBody, w.Body.String(), "raw Administrator, enrolled=%t", enrolled)
	}
	assert.False(t, del.applied(t, env, f.serverID))
}

// UpdateServer is grace-eligible: a verified edit lets the same session's next
// edit through without a code, and no other session's. DeleteServer is always
// fresh: the grace never covers it.
// Kills: Settle not called after UpdateServer's commit, or the grace not read
// before its transaction (the second edit is prompted); DeleteServer given a
// grace read.
func TestDangerousGate_UpdateServerGraceAndDeleteAlwaysFresh(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	update, del := gateRoutes()[0], gateRoutes()[1]
	f := newGateFixture(t, env, true, true)
	session := func() string {
		token, err := auth.GenerateAccessToken(f.owner.ID, testhelpers.TestJWTSecret, true, "", uuid.NewString())
		require.NoError(t, err)
		return token
	}
	first := session()

	require.Equal(t, gateOK, classifyGate(t, update, update.call(env, first, f.serverID, mfaBackupCode), "verified edit"))
	require.Equal(t, gateOK, classifyGate(t, update, update.call(env, first, f.serverID, ""), "grace-covered edit"))
	assert.Equal(t, gateMFARequired, classifyGate(t, update, update.call(env, session(), f.serverID, ""), "another session"))
	assert.Equal(t, gateMFARequired, classifyGate(t, del, del.call(env, first, f.serverID, ""), "delete under grace"))
	assert.False(t, del.applied(t, env, f.serverID))
}

// The toggle's UPDATE waits on each route's gate: with the route paused at its
// write, holding the gate's servers lock, the toggle's statement queues on a
// row lock and proceeds only after the route commits. So the setting cannot
// change between the gate's read and the write it authorized.
// Kills: the gate's servers lock weakened to FOR KEY SHARE, or the write moved
// out of the gate transaction.
func TestDangerousGate_ToggleWaitsOnTheGate(t *testing.T) {
	spy, routerDB := openSQLStateSpyDB(t)
	env := setupMFAEnforcementEnvWithDB(t, routerDB)
	writes := map[string]string{"UpdateServer": `UPDATE servers SET name = $1`, "DeleteServer": `DELETE FROM servers WHERE id = $1`}
	for _, r := range gateRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newGateFixture(t, env, false, true)
			pause := spy.armPause(t, writes[r.name])
			req := startRequest(func() *httptest.ResponseRecorder {
				return r.call(env, f.owner.AccessToken, f.serverID, "")
			})
			waitForSignal(t, pause.reached, req, "the route to reach its write")

			ctx := context.Background()
			probe, err := env.ts.DB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = probe.Rollback() }()
			_, err = probe.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`)
			require.NoError(t, err)
			var pid int
			require.NoError(t, probe.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			flipped := make(chan error, 1)
			go func() {
				_, err := probe.ExecContext(ctx, `UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, f.serverID)
				flipped <- err
			}()
			requireBackendWaitsOnALock(t, env.ts.DB, pid, flipped)
			pause.Release()

			r.requireOKBody(t, req.await(t, r.name), "the route read the setting before the flip")
			select {
			case err := <-flipped:
				require.NoError(t, err, "the toggle's UPDATE proceeds once the gate commits")
			case <-time.After(lockProofBound):
				t.Fatal("the toggle's UPDATE did not finish after the gate committed")
			}
		})
	}
}

// requireBackendWaitsOnALock polls pg_stat_activity until backend pid waits on
// a lock. The poll only paces reads of an observed state; nothing is inferred
// from elapsed time.
func requireBackendWaitsOnALock(t *testing.T, observer *sql.DB, pid int, finished <-chan error) {
	t.Helper()
	deadline := time.Now().Add(lockProofBound)
	for {
		var waitType sql.NullString
		require.NoError(t, observer.QueryRow(
			`SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waitType))
		if waitType.String == "Lock" {
			return
		}
		select {
		case err := <-finished:
			t.Fatalf("the toggle's UPDATE finished without waiting on the gate (err=%v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the toggle's UPDATE never waited on a lock within %s", lockProofBound)
		}
		time.Sleep(time.Millisecond)
	}
}

// Two owners each delete their own server while in voice on the other's, so
// each deletion's voice-candidate set holds the other owner. Each deletion
// locks its candidates and itself in one id-ordered statement, so the second
// queues behind the first rather than holding one row and waiting for the
// other. The first is paused just before its gate re-takes the actor's row:
// the point where a gate that locked the actor separately would hold only the
// candidate.
// Kills: the actor dropped from lockServerDeleteUsers' sorted set, leaving
// LockGateTx to lock it after the candidates (40P01, and one deletion fails).
func TestDangerousGate_DeleteServerCrossOwnerPairDoesNotDeadlock(t *testing.T) {
	spy, routerDB := openSQLStateSpyDB(t)
	env := setupMFAEnforcementEnvWithDB(t, routerDB)
	ts := env.ts
	a := newGateFixture(t, env, false, true)
	b := newGateFixture(t, env, false, true)
	requireSpyCountsADeadlock(t, spy, routerDB, ts.DB, a.owner.ID, b.owner.ID)
	baseline := spy.count(sqlStateDeadlock)

	joinVoice := func(serverID, userID string) {
		ts.AddMemberToServer(t, serverID, userID, "member")
		channelID := uuid.NewString()
		_, err := ts.DB.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'voice', 'voice')`, channelID, serverID)
		require.NoError(t, err)
		_, err = ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
			VALUES ($1, $2, now(), now())`, channelID, userID)
		require.NoError(t, err)
	}
	joinVoice(a.serverID, b.owner.ID)
	joinVoice(b.serverID, a.owner.ID)

	del := gateRoutes()[1]
	pause := spy.armPause(t, `COALESCE(password_hash, '') FROM users WHERE id = $1 FOR NO KEY UPDATE`)
	first := startRequest(func() *httptest.ResponseRecorder {
		return del.call(env, a.owner.AccessToken, a.serverID, "")
	})
	waitForSignal(t, pause.reached, first, "the first deletion to reach its gate")
	second := startRequest(func() *httptest.ResponseRecorder {
		return del.call(env, b.owner.AccessToken, b.serverID, "")
	})
	assert.True(t, waitForLockWaitOrDone(t, ts.DB, second.done),
		"the second deletion must queue behind the first's users locks, or this interleaving proved nothing")
	pause.Release()

	del.requireOKBody(t, first.await(t, "the first deletion"), "first")
	del.requireOKBody(t, second.await(t, "the second deletion"), "second")
	assert.Equal(t, baseline, spy.count(sqlStateDeadlock), "the cross-owner pair deadlocked (40P01)")
}
