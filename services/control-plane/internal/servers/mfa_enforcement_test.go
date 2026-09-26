package servers_test

// #3453 E-1, E-2, O-2 and the Nightwatch emission tests for
// GET/PUT /api/v1/servers/:id/mfa-enforcement, driven over HTTP through the
// real router. Each test names the mutant that kills it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// mfaBackupCode is each enrolled persona's single backup code. It stays at or
// under 20 characters so the real verifier checks it as a TOTP/backup code
// rather than as a WebAuthn inline token.
const mfaBackupCode = "MFAENF01"

// mfaWrongCode is never a valid code for anyone. Distinctive, so O-2 can prove
// no part of it reaches a log.
const mfaWrongCode = "O2CODE7Q"

// mfaBodyForbidden is rbac.RequirePermission's 403 body, byte for byte.
const mfaBodyForbidden = `{"error":"Insufficient permissions"}`

// mfaEventRecorder is a concurrency-safe securityevent.Emitter.
type mfaEventRecorder struct {
	mu     sync.Mutex
	events []securityevent.Event
}

func (r *mfaEventRecorder) Emit(_ context.Context, e securityevent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// take returns the events recorded since the last take and clears them.
func (r *mfaEventRecorder) take() []securityevent.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

type mfaEnv struct {
	ts       *testhelpers.TestServer
	events   *mfaEventRecorder
	logs     *testhelpers.SyncBuffer
	enforcer *voice.PermissionEnforcer
	nats     *natsclient.Client
}

// setupMFAEnforcementEnv builds the real router exactly as
// testhelpers.SetupTestServer does, plus a capturing Nightwatch emitter and a
// log buffer (the recovery_integration_test.go precedent for a router with
// custom dependencies).
func setupMFAEnforcementEnv(t *testing.T) *mfaEnv {
	t.Helper()
	return setupMFAEnforcementEnvWithDB(t, nil)
}

// setupMFAEnforcementEnvWithDB is setupMFAEnforcementEnv with the router's
// database pool replaced by routerDB when it is non-nil. Fixtures still write
// through env.ts.DB, the ordinary test pool, so only the router's own traffic
// crosses routerDB (the lock-order test's SQLSTATE spy).
func setupMFAEnforcementEnvWithDB(t *testing.T, routerDB *sql.DB) *mfaEnv {
	t.Helper()
	t.Setenv("CONCORD_ENV", "test")
	db, dbCleanup := testhelpers.SetupTestDB(t)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	cfg := &config.Config{
		Environment:                      "test",
		Port:                             "0",
		JWTSecret:                        testhelpers.TestJWTSecret,
		AllowedOrigins:                   []string{"*"},
		InstanceType:                     os.Getenv("INSTANCE_TYPE"),
		MFAEncryptionKey:                 strings.Repeat("00", 32),
		MFAEncryptionKeyVersion:          1,
		WebAuthnRPID:                     "localhost",
		WebAuthnRPOrigins:                []string{"http://localhost:3001"},
		ActivityHistoryClusterEnabled:    true,
		ControlPlaneReplicaCount:         1,
		ControlPlaneReplicaCountExplicit: true,
		NATSUrl:                          os.Getenv("NATS_URL"),
	}
	logs := &testhelpers.SyncBuffer{}
	recorder := &mfaEventRecorder{}
	history := presencehistory.NewService(db,
		presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}), true)
	t.Cleanup(func() {
		redisCleanup()
		dbCleanup()
	})
	if routerDB == nil {
		routerDB = db
	}
	router, hub, natsClient, opsRuntime, enforcer, _, closePresence, _, _, _, err := api.NewRouter(
		t.Context(), routerDB, rdb, cfg, nil, logger.NewWithWriter(logs),
		api.RouterDependencies{PresenceHistory: history, SecurityEvents: recorder},
	)
	require.NoError(t, err)
	if natsClient != nil {
		t.Cleanup(func() { _ = natsClient.Close() })
	}
	t.Cleanup(enforcer.Close)
	t.Cleanup(func() { require.NoError(t, opsRuntime.Stop(context.Background())) })
	t.Cleanup(func() { hub.Shutdown() })
	t.Cleanup(closePresence)
	return &mfaEnv{
		ts:       &testhelpers.TestServer{Router: router, Hub: hub, DB: db, Redis: rdb, PresenceHistory: history},
		events:   recorder,
		logs:     logs,
		enforcer: enforcer,
		nats:     natsClient,
	}
}

// mfaFixture: an owner, a raw-bit Administrator, a member holding every
// NON-administrator permission (so only bit 62 can admit anyone but the
// owner), and a user who is not a member.
type mfaFixture struct {
	owner, admin, member, outsider testhelpers.TestUser
	serverID                       string
}

func newMFAFixture(t *testing.T, env *mfaEnv, prefix string, enforcing bool) mfaFixture {
	t.Helper()
	ts := env.ts
	f := mfaFixture{
		owner:    ts.CreateTestUser(t, prefix+"owner"),
		admin:    ts.CreateTestUser(t, prefix+"admin"),
		member:   ts.CreateTestUser(t, prefix+"member"),
		outsider: ts.CreateTestUser(t, prefix+"outsider"),
	}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "MFA Enforcement "+prefix)
	ts.AddMemberToServer(t, f.serverID, f.admin.ID, "member")
	ts.AddMemberToServer(t, f.serverID, f.member.ID, "member")
	adminRole := ts.CreateTestRole(t, f.serverID, "admin-"+uuid.NewString()[:8], 5, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, f.serverID, f.admin.ID, adminRole)
	allButAdmin := ts.CreateTestRole(t, f.serverID, "all-"+uuid.NewString()[:8], 4, int64(rbac.ConcretePermissions))
	ts.AssignRoleToUser(t, f.serverID, f.member.ID, allButAdmin)
	if enforcing {
		setMFAFlag(t, env, f.serverID, true)
	}
	return f
}

func setMFAFlag(t *testing.T, env *mfaEnv, serverID string, on bool) {
	t.Helper()
	_, err := env.ts.DB.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = $2 WHERE id = $1`, serverID, on)
	require.NoError(t, err)
}

func readMFAFlag(t *testing.T, env *mfaEnv, serverID string) bool {
	t.Helper()
	var on bool
	require.NoError(t, env.ts.DB.QueryRow(
		`SELECT enforce_mfa_dangerous_actions FROM servers WHERE id = $1`, serverID).Scan(&on))
	return on
}

// enrollMFATOTP gives userID a confirmed TOTP factor sealed under the router's
// keyring, plus one unused backup code (mfaBackupCode), and returns the TOTP
// secret so a caller can mint a live code.
func enrollMFATOTP(t *testing.T, env *mfaEnv, userID string) string {
	t.Helper()
	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	key, err := mfa.GenerateSecret(userID + "@mfa-enforcement.test")
	require.NoError(t, err)
	enc, nonce, version, err := ring.Seal([]byte(key.Secret()))
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(mfaBackupCode))
	_, err = env.ts.DB.Exec(`INSERT INTO user_mfa_totp
		(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
		VALUES ($1, $2, $3, $4, TRUE, TRUE, $5, $6)`,
		userID, enc, nonce, version, pq.Array([]string{hex.EncodeToString(digest[:])}), pq.Array([]bool{false}))
	require.NoError(t, err)
	return key.Secret()
}

func backupCodeSpent(t *testing.T, env *mfaEnv, userID string) bool {
	t.Helper()
	var used []bool
	require.NoError(t, env.ts.DB.QueryRow(
		`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(pq.Array(&used)))
	require.Len(t, used, 1)
	return used[0]
}

// budgetCount reads the attempt counter the PUT charges; 0 when absent.
func budgetCount(t *testing.T, env *mfaEnv, userID string) int {
	t.Helper()
	n, err := env.ts.Redis.Get(context.Background(), servers.MFAEnforcementStepUpPrefix+userID).Int()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	return n
}

// serverGeneration seeds (if needed) and reads the server's permission
// generation, so a bump is observable as a changed value.
func serverGeneration(t *testing.T, env *mfaEnv, serverID, userID string) string {
	t.Helper()
	return testhelpers.SeedPermissionGenerations(t, env.ts.Redis, serverID, userID).Server
}

func mfaURL(serverID string) string { return "/api/v1/servers/" + serverID + "/mfa-enforcement" }

func putMFA(env *mfaEnv, u testhelpers.TestUser, serverID string, body map[string]any) *httptest.ResponseRecorder {
	return env.ts.DoRequest(http.MethodPut, mfaURL(serverID), body, testhelpers.AuthHeaders(u.AccessToken))
}

func putMFARaw(env *mfaEnv, u testhelpers.TestUser, serverID, raw string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, mfaURL(serverID), strings.NewReader(raw))
	req.Header = testhelpers.AuthHeaders(u.AccessToken)
	w := httptest.NewRecorder()
	env.ts.Router.ServeHTTP(w, req)
	return w
}

func getMFA(env *mfaEnv, u testhelpers.TestUser, serverID string) *httptest.ResponseRecorder {
	return env.ts.DoRequest(http.MethodGet, mfaURL(serverID), nil, testhelpers.AuthHeaders(u.AccessToken))
}

func bodyOn() map[string]any             { return map[string]any{"enabled": true} }
func bodyOff(code string) map[string]any { return map[string]any{"enabled": false, "mfa_code": code} }
func bodyOffNoCode() map[string]any      { return map[string]any{"enabled": false} }
func mfaValueBody(value bool) string {
	return fmt.Sprintf(`{"enforce_mfa_dangerous_actions":%t}`, value)
}
func mfaErrorBody(message string) string { return fmt.Sprintf(`{"error":%q}`, message) }

func requireEnrollmentRequired(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, w.Code, msg)
	require.JSONEq(t, fmt.Sprintf(`{"error":%q,"mfa_enrollment_required":true}`, stepup.ErrMsgMFAEnrollmentRequired),
		w.Body.String(), msg)
}

// deniedFallback is the one event every 401/403 on this route produces.
var deniedFallback = securityevent.Event{
	EventType: securityevent.EventPrivilegedAction, Outcome: securityevent.OutcomeDenied,
	Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonPrivilegedRouteDenied,
	RouteTemplate: securityevent.RouteServerMFAEnforcement,
}

// ---------------------------------------------------------------------------
// E-1: authorization
// ---------------------------------------------------------------------------

// A non-member is stopped by RequireMembership before the handler runs.
// Kills: the route registered without RequireMembership.
func TestMFAEnforcement_NonMemberIsRefusedAsNotAMember(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfanm", true)

	for name, w := range map[string]*httptest.ResponseRecorder{
		"GET": getMFA(env, f.outsider, f.serverID),
		"PUT": putMFA(env, f.outsider, f.serverID, bodyOffNoCode()),
	} {
		assert.Equal(t, http.StatusForbidden, w.Code, name)
		assert.JSONEq(t, mfaErrorBody("Not a member of this server"), w.Body.String(), name)
	}
	assert.True(t, readMFAFlag(t, env, f.serverID))
}

// A member holding every non-administrator permission is neither the owner
// nor a raw-bit Administrator, so GET and every PUT shape get
// RequirePermission's own 403, byte for byte, whatever their enrollment.
// Kills: authorization evaluated AFTER the MFA gate (an unenrolled member
// would get mfa_enrollment_required on ON, an enrolled one mfa_required on
// OFF, each learning their own requirement); a bit other than 62 admitting.
func TestMFAEnforcement_NonAdministratorGetsTheGeneric403(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfana", true)
	plain := env.ts.CreateTestUser(t, "mfanaplain")
	env.ts.AddMemberToServer(t, f.serverID, plain.ID, "member")

	// The reference body comes from the real middleware, not from this file.
	ref := env.ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+f.serverID+"/roles/"+uuid.NewString(), nil,
		testhelpers.AuthHeaders(plain.AccessToken))
	require.Equal(t, http.StatusForbidden, ref.Code)
	require.Equal(t, mfaBodyForbidden, ref.Body.String(), "control: RequirePermission's own 403 body")

	check := func(persona string) {
		cases := map[string]*httptest.ResponseRecorder{
			"GET":           getMFA(env, f.member, f.serverID),
			"PUT on":        putMFA(env, f.member, f.serverID, bodyOn()),
			"PUT off":       putMFA(env, f.member, f.serverID, bodyOffNoCode()),
			"PUT off+code":  putMFA(env, f.member, f.serverID, bodyOff(mfaWrongCode)),
			"PUT off+valid": putMFA(env, f.member, f.serverID, bodyOff(mfaBackupCode)),
		}
		for name, w := range cases {
			assert.Equal(t, http.StatusForbidden, w.Code, "%s %s", persona, name)
			assert.Equal(t, ref.Body.String(), w.Body.String(), "%s %s: byte-identical to RequirePermission", persona, name)
		}
	}
	check("unenrolled")
	enrollMFATOTP(t, env, f.member.ID)
	check("enrolled")
	assert.True(t, readMFAFlag(t, env, f.serverID), "a refused PUT must not write")
	assert.False(t, backupCodeSpent(t, env, f.member.ID), "a refused PUT must not spend a backup code")
}

// An unenrolled Administrator on an enforcing server is MASKED, yet must read
// the setting that masks them; so must an unenrolled owner. Kills: the
// Administrator check read through the masked resolver (Apply clears bit 62).
func TestMFAEnforcement_UnenrolledAdministratorReadsTheSetting(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfaur", true)

	for name, u := range map[string]testhelpers.TestUser{"admin": f.admin, "owner": f.owner} {
		w := getMFA(env, u, f.serverID)
		assert.Equal(t, http.StatusOK, w.Code, name)
		assert.JSONEq(t, mfaValueBody(true), w.Body.String(), name)
	}
}

// ---------------------------------------------------------------------------
// E-1: the gate on the requested value
// ---------------------------------------------------------------------------

// ON by an unenrolled owner or Administrator: refused, nothing written, no
// bump, no success event, and exactly the one denied fallback.
// Kills: the ON enrollment check removed; a bump or event on a refusal.
func TestMFAEnforcement_OnRequiresEnrollment(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfaon", false)
	gen := serverGeneration(t, env, f.serverID, f.owner.ID)
	env.events.take()

	for name, u := range map[string]testhelpers.TestUser{"owner": f.owner, "admin": f.admin} {
		requireEnrollmentRequired(t, putMFA(env, u, f.serverID, bodyOn()), name)
		assert.Equal(t, []securityevent.Event{deniedFallback}, env.events.take(), name)
	}
	assert.False(t, readMFAFlag(t, env, f.serverID), "a refused ON must not write")
	assert.Equal(t, gen, serverGeneration(t, env, f.serverID, f.owner.ID), "a refused ON must not bump")
}

// OFF with no code asks for the actor's own inline factors and charges
// nothing. Kills: the budget charged for a credential-less request.
func TestMFAEnforcement_OffWithoutCodeAsksForTheFactor(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfanc", true)
	enrollMFATOTP(t, env, f.owner.ID)

	w := putMFA(env, f.owner, f.serverID, bodyOffNoCode())
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, `{"error":"MFA verification required","mfa_required":true,"methods":["totp"]}`, w.Body.String())
	assert.True(t, readMFAFlag(t, env, f.serverID))
	assert.Equal(t, 0, budgetCount(t, env, f.owner.ID), "a request with no code must not charge the budget")
}

// OFF with a wrong code: refused, budget charged, nothing written, no bump.
func TestMFAEnforcement_OffWithWrongCodeIsRefusedAndCharged(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfawc", true)
	enrollMFATOTP(t, env, f.owner.ID)
	gen := serverGeneration(t, env, f.serverID, f.owner.ID)
	env.events.take()

	w := putMFA(env, f.owner, f.serverID, bodyOff(mfaWrongCode))
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, mfaErrorBody(stepup.ErrMsgInvalidMFACode), w.Body.String())
	assert.Equal(t, 1, budgetCount(t, env, f.owner.ID), "a code-bearing OFF charges the budget")
	assert.True(t, readMFAFlag(t, env, f.serverID))
	assert.Equal(t, gen, serverGeneration(t, env, f.serverID, f.owner.ID))
	assert.Equal(t, []securityevent.Event{deniedFallback}, env.events.take())
}

// OFF by an unenrolled owner never succeeds, with or without a code (I8).
// Kills: the gatePurgeFenceDisable fail-open (`if !MFAEnabled { return nil }`)
// copied into the OFF path.
func TestMFAEnforcement_OffByUnenrolledOwnerRequiresEnrollment(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfauo", true)

	requireEnrollmentRequired(t, putMFA(env, f.owner, f.serverID, bodyOffNoCode()), "no code")
	requireEnrollmentRequired(t, putMFA(env, f.owner, f.serverID, bodyOff(mfaWrongCode)), "with a code")
	assert.True(t, readMFAFlag(t, env, f.serverID), "an unenrolled owner must never turn enforcement off")
}

// OFF while already off still requires the confirmation; only a verified one
// is a no-op 200, and it neither bumps nor emits (L6).
// Kills: the same-value no-op evaluated before the gate.
func TestMFAEnforcement_OffWhileOffStillRequiresConfirmation(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfaoo", false)
	enrollMFATOTP(t, env, f.owner.ID)
	gen := serverGeneration(t, env, f.serverID, f.owner.ID)
	env.events.take()

	w := putMFA(env, f.owner, f.serverID, bodyOffNoCode())
	require.Equal(t, http.StatusForbidden, w.Code, "OFF-while-off with no code must still ask for MFA")
	assert.Contains(t, w.Body.String(), `"mfa_required":true`)
	w = putMFA(env, f.owner, f.serverID, bodyOff(mfaWrongCode))
	require.Equal(t, http.StatusForbidden, w.Code, "OFF-while-off with a wrong code must be refused")
	env.events.take()

	w = putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, mfaValueBody(false), w.Body.String())
	assert.Empty(t, env.events.take(), "a no-op emits nothing")
	assert.Equal(t, gen, serverGeneration(t, env, f.serverID, f.owner.ID), "a no-op does not bump")
	assert.Equal(t, 0, budgetCount(t, env, f.owner.ID), "a verified committed OFF clears the budget")
}

// ON while already on (enrolled): a no-op 200 with no bump and no event.
// Kills: a bump or event on a no-op.
func TestMFAEnforcement_OnWhileOnIsANoOp(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfaoa", true)
	enrollMFATOTP(t, env, f.admin.ID)
	gen := serverGeneration(t, env, f.serverID, f.admin.ID)
	env.events.take()

	w := putMFA(env, f.admin, f.serverID, bodyOn())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, mfaValueBody(true), w.Body.String())
	assert.Empty(t, env.events.take())
	assert.Equal(t, gen, serverGeneration(t, env, f.serverID, f.admin.ID))
}

// The full round trip: ON by an enrolled owner, OFF with a backup code, each
// read back through GET, each bumping the server generation and emitting its
// own reason and severity; updated_at never moves.
// Kills: a missing bump; the success event's reason or severity swapped;
// updated_at touched by the UPDATE.
func TestMFAEnforcement_OnThenOffRoundTrip(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfart", false)
	enrollMFATOTP(t, env, f.owner.ID)
	var updatedAt time.Time
	require.NoError(t, env.ts.DB.QueryRow(`SELECT updated_at FROM servers WHERE id = $1`, f.serverID).Scan(&updatedAt))
	gen0 := serverGeneration(t, env, f.serverID, f.owner.ID)
	env.events.take()

	w := putMFA(env, f.owner, f.serverID, bodyOn())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, mfaValueBody(true), w.Body.String())
	assert.JSONEq(t, mfaValueBody(true), getMFA(env, f.owner, f.serverID).Body.String())
	gen1 := serverGeneration(t, env, f.serverID, f.owner.ID)
	assert.NotEqual(t, gen0, gen1, "a committed flip must bump the server generation")
	assert.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventPrivilegedAction, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonServerMFAEnforcementEnabled,
		AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteServerMFAEnforcement,
	}}, env.events.take())

	w = putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, mfaValueBody(false), w.Body.String())
	assert.JSONEq(t, mfaValueBody(false), getMFA(env, f.owner, f.serverID).Body.String())
	assert.NotEqual(t, gen1, serverGeneration(t, env, f.serverID, f.owner.ID))
	assert.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventPrivilegedAction, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonServerMFAEnforcementDisabled,
		AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteServerMFAEnforcement,
	}}, env.events.take())
	assert.True(t, backupCodeSpent(t, env, f.owner.ID), "the committed OFF spends the backup code")
	assert.Equal(t, 0, budgetCount(t, env, f.owner.ID), "the budget is cleared after a verified commit")

	var after time.Time
	require.NoError(t, env.ts.DB.QueryRow(`SELECT updated_at FROM servers WHERE id = $1`, f.serverID).Scan(&after))
	assert.True(t, updatedAt.Equal(after), "updated_at must not reveal a flip to members: %s != %s", updatedAt, after)
}

// ---------------------------------------------------------------------------
// E-1: body, budget, lock conflict, commit failure
// ---------------------------------------------------------------------------

// Strict body: 413 before 400, then a missing or ill-typed `enabled`, a
// second document, trailing garbage or an over-long code are 400s.
func TestMFAEnforcement_BodyValidation(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfabv", false)
	enrollMFATOTP(t, env, f.owner.ID)

	bad := map[string]string{
		"missing enabled":   `{"mfa_code":"123456"}`,
		"null enabled":      `{"enabled":null}`,
		"enabled not bool":  `{"enabled":"true"}`,
		"trailing garbage":  `{"enabled":true} x`,
		"second document":   `{"enabled":true}{"enabled":false}`,
		"code over 256":     fmt.Sprintf(`{"enabled":false,"mfa_code":%q}`, strings.Repeat("7", 257)),
		"not json":          `enabled=true`,
		"empty body":        ``,
		"array not object":  `[true]`,
		"code wrong type":   `{"enabled":false,"mfa_code":7}`,
		"truncated":         `{"enabled":true`,
		"enabled as number": `{"enabled":1}`,
	}
	// Body validation runs before authorization, so any member exercises it;
	// rotating keeps each under the route's 10/min limiter.
	senders := []testhelpers.TestUser{f.owner, f.admin, f.member}
	i := 0
	for name, raw := range bad {
		w := putMFARaw(env, senders[i%len(senders)], f.serverID, raw)
		i++
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
		assert.JSONEq(t, mfaErrorBody("Invalid request body"), w.Body.String(), name)
	}
	for name, raw := range map[string]string{
		"oversized valid document":   `{"enabled":true}` + strings.Repeat(" ", 1100),
		"oversized invalid document": fmt.Sprintf(`{"enabled":false,"mfa_code":%q}`, strings.Repeat("7", 2000)),
	} {
		w := putMFARaw(env, senders[i%len(senders)], f.serverID, raw)
		i++
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, name)
		assert.JSONEq(t, mfaErrorBody("Request body too large"), w.Body.String(), name)
	}
	assert.False(t, readMFAFlag(t, env, f.serverID))

	// Control: the same shape within the cap is accepted.
	w := putMFARaw(env, f.owner, f.serverID, `{"enabled":true}`+strings.Repeat(" ", 900))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// Five code-bearing attempts spend the budget; the sixth is a 429 even with
// the RIGHT code, and nothing is written or spent.
func TestMFAEnforcement_SixthOffAttemptIs429(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfa6", true)
	enrollMFATOTP(t, env, f.owner.ID)

	for i := 1; i <= stepup.BudgetLimit; i++ {
		w := putMFA(env, f.owner, f.serverID, bodyOff(mfaWrongCode))
		require.Equal(t, http.StatusForbidden, w.Code, "attempt %d", i)
	}
	w := putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.JSONEq(t, mfaErrorBody(stepup.ErrMsgTooManyAttempts), w.Body.String())
	assert.True(t, readMFAFlag(t, env, f.serverID))
	assert.False(t, backupCodeSpent(t, env, f.owner.ID), "a 429 must not reach the verifier")
}

// L12: the budget is charged BEFORE the transaction and therefore before
// authorization. A non-administrator who exhausts their own budget gets 429,
// which tells them only about their own attempts, and the counter is already
// charged when the transaction begins.
// Kills: the Budget charged after BeginTx.
func TestMFAEnforcement_BudgetPrecedesTheTransactionAndAuthorization(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfabo", true)

	var chargedAtBegin []int
	servers.WrapMFAEnforcementBeginForTest(t, func(context.Context) {
		chargedAtBegin = append(chargedAtBegin, budgetCount(t, env, f.member.ID))
	})
	for i := 1; i <= stepup.BudgetLimit; i++ {
		w := putMFA(env, f.member, f.serverID, bodyOff(mfaWrongCode))
		require.Equal(t, http.StatusForbidden, w.Code, "attempt %d", i)
		require.Equal(t, mfaBodyForbidden, w.Body.String(), "attempt %d", i)
	}
	assert.Equal(t, []int{1, 2, 3, 4, 5}, chargedAtBegin,
		"each attempt's charge must already be recorded when its transaction begins")
	w := putMFA(env, f.member, f.serverID, bodyOff(mfaWrongCode))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "the budget answers before authorization (L12)")
	assert.Len(t, chargedAtBegin, stepup.BudgetLimit, "a 429 opens no transaction")
}

// A budget that cannot be evaluated is a 503, not a 429 and not a pass; an
// OFF with no code never consults it.
func TestMFAEnforcement_UnevaluableBudgetIs503(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfa503", true)
	enrollMFATOTP(t, env, f.owner.ID)
	servers.SetMFAEnforcementBudgetForTest(t, func(*redis.Client) stepup.Budget {
		return stepup.NewBudget(nil, servers.MFAEnforcementStepUpPrefix)
	})

	w := putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, mfaErrorBody(stepup.ErrMsgBudgetUnavailable), w.Body.String())
	assert.True(t, readMFAFlag(t, env, f.serverID))
	assert.False(t, backupCodeSpent(t, env, f.owner.ID))

	w = putMFA(env, f.owner, f.serverID, bodyOffNoCode())
	assert.Equal(t, http.StatusForbidden, w.Code, "a credential-less OFF does not consult the budget")
}

// A lock conflict anywhere in the gate is a 503 with Retry-After: 1 and the
// fixed failure class, including a users-row timeout arriving inside a
// *stepup.Error. Kills: IsLockConflict checked after the *stepup.Error branch
// (the wrapped case would surface as a 500).
func TestMFAEnforcement_LockConflictIs503WithRetryAfter(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfalc", false)
	enrollMFATOTP(t, env, f.owner.ID)

	for name, injected := range map[string]error{
		"deadlock at the servers row": fmt.Errorf("mfaenforce: lock server: %w", &pq.Error{Code: "40P01"}),
		"lock timeout at the users row": &stepup.Error{Status: http.StatusInternalServerError,
			Body:  map[string]any{"error": stepup.ErrMsgVerificationFailed},
			Cause: fmt.Errorf("lock step-up subject: %w", &pq.Error{Code: "55P03"})},
	} {
		servers.SetMFAEnforcementGateForTest(t, func(context.Context, *sql.Tx, string, string,
			stepup.Lock, mfaenforce.ServerLock, string) (mfaenforce.Gate, error) {
			return mfaenforce.Gate{}, injected
		})
		logs := env.logs
		logs.Reset()
		w := putMFA(env, f.owner, f.serverID, bodyOn())
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, name)
		assert.Equal(t, "1", w.Header().Get("Retry-After"), name)
		assert.JSONEq(t, mfaErrorBody("The server is busy. Try again."), w.Body.String(), name)
		assert.Contains(t, logs.String(), "mfa_gate_lock", name)
	}
	assert.False(t, readMFAFlag(t, env, f.serverID))
}

// A backup code accepted inside a transaction whose COMMIT fails is not
// spent, the setting is unchanged, and the budget is not cleared; the same
// code then succeeds. The server generation IS bumped, because a Commit error
// does not prove a rollback (C-3). Kills: the MFA factor verified on the pool
// rather than on the transaction (VerifyMFAFactor for VerifyMFAFactorTx); the
// budget cleared before the commit.
func TestMFAEnforcement_BackupCodeSurvivesAFailedCommit(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfacf", true)
	enrollMFATOTP(t, env, f.owner.ID)
	gen := serverGeneration(t, env, f.serverID, f.owner.ID)

	servers.SetMFAEnforcementCommitForTest(t, func(tx *sql.Tx) error {
		// Poison the transaction so the real COMMIT fails after every
		// in-transaction write, including the backup-code redemption, ran.
		_, poisonErr := tx.Exec(`SELECT 1/0`)
		require.Error(t, poisonErr)
		return tx.Commit()
	})
	env.events.take()
	w := putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.JSONEq(t, mfaErrorBody("Failed to update MFA enforcement"), w.Body.String())
	assert.True(t, readMFAFlag(t, env, f.serverID))
	assert.False(t, backupCodeSpent(t, env, f.owner.ID), "a rolled-back OFF must not burn the backup code")
	assert.Equal(t, 1, budgetCount(t, env, f.owner.ID), "a failed commit is not a success: the budget stays charged")
	// C-3: a Commit error does not prove a rollback, so the handler bumps
	// whenever the UPDATE ran. Here the COMMIT did roll back, and the bump
	// costs one cache miss per member.
	assert.NotEqual(t, gen, serverGeneration(t, env, f.serverID, f.owner.ID),
		"a failed commit whose UPDATE ran still bumps the server generation (C-3)")
	assert.Empty(t, env.events.take(), "no event without a commit (a 500 is not a 401/403)")

	servers.SetMFAEnforcementCommitForTest(t, func(tx *sql.Tx) error { return tx.Commit() })
	w = putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusOK, w.Code, "the unspent code still works: %s", w.Body.String())
	assert.False(t, readMFAFlag(t, env, f.serverID))
}

// ---------------------------------------------------------------------------
// Nightwatch: every 403 refusal is the one fallback
// ---------------------------------------------------------------------------

// Every 403 refusal on the route, whatever produced it (membership, the
// authorization check, enrollment, a missing or a wrong code), reaches
// Nightwatch as exactly one privileged_route_denied event (I7).
// Kills: the handler emitting a refusal-specific event, or marking a refusal
// handled so that the fallback is suppressed.
func TestMFAEnforcement_EveryRefusalIsTheOneDeniedFallback(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfanw", true)
	enrolled := env.ts.CreateTestUser(t, "mfanwenrolled")
	enrollMFATOTP(t, env, enrolled.ID)
	env.ts.AddMemberToServer(t, f.serverID, enrolled.ID, "member")
	adminRole := env.ts.CreateTestRole(t, f.serverID, "nwadmin-"+uuid.NewString()[:8], 6, int64(rbac.PermAdministrator))
	env.ts.AssignRoleToUser(t, f.serverID, enrolled.ID, adminRole)
	env.events.take()

	refusals := []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"not a member", func() *httptest.ResponseRecorder { return putMFA(env, f.outsider, f.serverID, bodyOffNoCode()) }},
		{"not an administrator", func() *httptest.ResponseRecorder { return putMFA(env, f.member, f.serverID, bodyOn()) }},
		{"enrollment required", func() *httptest.ResponseRecorder { return putMFA(env, f.owner, f.serverID, bodyOffNoCode()) }},
		{"code required", func() *httptest.ResponseRecorder { return putMFA(env, enrolled, f.serverID, bodyOffNoCode()) }},
		{"wrong code", func() *httptest.ResponseRecorder { return putMFA(env, enrolled, f.serverID, bodyOff(mfaWrongCode)) }},
	}
	for _, r := range refusals {
		w := r.do()
		require.Equal(t, http.StatusForbidden, w.Code, r.name)
		assert.Equal(t, []securityevent.Event{deniedFallback}, env.events.take(), r.name)
	}
}

// ---------------------------------------------------------------------------
// E-2: the value appears in no other server payload
// ---------------------------------------------------------------------------

const mfaFlagKey = "enforce_mfa_dangerous_actions"

// containsKey walks a decoded JSON value for key at any depth.
func containsKey(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x[key]; ok {
			return true
		}
		for _, child := range x {
			if containsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if containsKey(child, key) {
				return true
			}
		}
	}
	return false
}

func requireNoFlagInBody(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	require.Less(t, w.Code, 300, "%s: %s", what, w.Body.String())
	var decoded any
	testhelpers.ParseJSON(t, w, &decoded)
	assert.False(t, containsKey(decoded, mfaFlagKey), "%s must not carry %s: %s", what, mfaFlagKey, w.Body.String())
}

// E-2 (design L1): on an ENFORCING server, no GetServer, ListServers,
// CreateServer or UpdateServer response, and no server_updated frame, carries
// the setting, for the owner, an Administrator or a member.
// Kills: the flag added to models.Server or to the server_updated payload.
func TestMFAEnforcement_ValueAppearsInNoServerPayload(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfae2", true)
	ts := env.ts
	// Enrolled BEFORE any request, so neither is masked out of the
	// ManageServer that UpdateServer checks, and no masked value is cached.
	enrollMFATOTP(t, env, f.owner.ID)
	enrollMFATOTP(t, env, f.admin.ID)

	for name, u := range map[string]testhelpers.TestUser{"owner": f.owner, "admin": f.admin, "member": f.member} {
		h := testhelpers.AuthHeaders(u.AccessToken)
		requireNoFlagInBody(t, ts.DoRequest(http.MethodGet, "/api/v1/servers/"+f.serverID, nil, h), name+" GetServer")
		requireNoFlagInBody(t, ts.DoRequest(http.MethodGet, "/api/v1/servers", nil, h), name+" ListServers")
		requireNoFlagInBody(t, ts.DoRequest(http.MethodPost, "/api/v1/servers",
			map[string]any{"name": "E2 " + name}, h), name+" CreateServer")
	}

	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	wsHeaders := http.Header{}
	wsHeaders.Set("Authorization", "Bearer "+f.member.AccessToken)
	client, _, err := websocket.DefaultDialer.Dial("ws"+wsServer.URL[len("http"):]+"/api/v1/ws", wsHeaders)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	// Subscribe to the server, then send a malformed subscribe_server. The hub
	// handles a client's frames in order on its Run loop, so the malformed
	// frame's error reply proves the real subscription is already in place:
	// a deterministic join, not a timed wait.
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, client.WriteJSON(map[string]any{"type": "subscribe_server", "data": map[string]any{"server_id": f.serverID}}))
	require.NoError(t, client.WriteJSON(map[string]any{"type": "subscribe_server", "data": map[string]any{"server_id": "not-a-uuid"}}))
	for {
		var frame map[string]any
		require.NoError(t, client.ReadJSON(&frame), "the subscription barrier must answer")
		if frame["type"] == "error" {
			break
		}
	}

	for name, u := range map[string]testhelpers.TestUser{"owner": f.owner, "admin": f.admin} {
		requireNoFlagInBody(t, ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+f.serverID,
			map[string]any{"name": "E2 renamed " + name}, testhelpers.AuthHeaders(u.AccessToken)), name+" UpdateServer")
	}

	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	seen := 0
	for seen < 2 {
		var frame map[string]any
		require.NoError(t, client.ReadJSON(&frame), "the member must receive both server_updated frames")
		if frame["type"] != "server_updated" {
			continue
		}
		seen++
		assert.False(t, containsKey(frame, mfaFlagKey), "server_updated must not carry %s: %v", mfaFlagKey, frame)
	}
}

// ---------------------------------------------------------------------------
// O-2: principle 7 in the logs
// ---------------------------------------------------------------------------

// mfaForbiddenLogTokens are the fields and values no log line on this route
// may carry (I7, observability principle 7).
var mfaForbiddenLogTokens = []string{
	"enrolled", "enforcing", "masked", "methods", "mfa_code", "totp", "webauthn",
	mfaWrongCode, mfaBackupCode,
}

// O-2: the handler logs nothing for a 4xx refusal, and a 500 logs the same
// fixed class for an enrolled and an unenrolled actor; neither carries any
// forbidden field or any part of the submitted code.
func TestMFAEnforcement_LogsCarryNoEnrollmentOrCode(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	enrolled := newMFAFixture(t, env, "mfao2e", true)
	unenrolled := newMFAFixture(t, env, "mfao2u", true)
	enrollMFATOTP(t, env, enrolled.owner.ID)

	refusals := func(f mfaFixture) string {
		env.logs.Reset()
		putMFA(env, f.owner, f.serverID, bodyOffNoCode())
		putMFA(env, f.owner, f.serverID, bodyOff(mfaWrongCode))
		putMFA(env, f.member, f.serverID, bodyOff(mfaWrongCode))
		putMFA(env, f.owner, f.serverID, bodyOn())
		return handlerLogLines(env.logs.String())
	}
	assert.Empty(t, refusals(enrolled), "an enrolled actor's refusals are not logged")
	assert.Empty(t, refusals(unenrolled), "an unenrolled actor's refusals are not logged")

	servers.SetMFAEnforcementGateForTest(t, func(context.Context, *sql.Tx, string, string,
		stepup.Lock, mfaenforce.ServerLock, string) (mfaenforce.Gate, error) {
		return mfaenforce.Gate{}, errors.New("injected gate failure")
	})
	failure := func(f mfaFixture) string {
		env.logs.Reset()
		w := putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
		require.Equal(t, http.StatusInternalServerError, w.Code)
		return env.logs.String()
	}
	enrolledLog, unenrolledLog := failure(enrolled), failure(unenrolled)
	for name, got := range map[string]string{"enrolled": enrolledLog, "unenrolled": unenrolledLog} {
		assert.Contains(t, got, "failure_class=mfa_enforcement_internal", name)
		for _, token := range mfaForbiddenLogTokens {
			assert.NotContains(t, strings.ToLower(got), strings.ToLower(token), "%s log leaked %q", name, token)
		}
	}
	assert.Equal(t, handlerLogLines(enrolledLog), handlerLogLines(unenrolledLog),
		"the 500 line must not differ between an enrolled and an unenrolled actor")
}

// handlerLogLines keeps the handler's own lines (not the request logger's),
// with the volatile time field removed, so two captures can be compared.
func handlerLogLines(captured string) string {
	var out bytes.Buffer
	for _, line := range strings.Split(captured, "\n") {
		if !strings.Contains(line, "MFA enforcement") {
			continue
		}
		fields := strings.Fields(line)
		kept := fields[:0]
		for _, field := range fields {
			if !strings.HasPrefix(field, "time=") {
				kept = append(kept, field)
			}
		}
		fmt.Fprintln(&out, strings.Join(kept, " "))
	}
	return out.String()
}
