package api_test

// POST /api/v1/auth/step-up/password through the REAL api.NewRouter (#3509):
// what a handler test cannot see — the route's own middleware, its Nightwatch
// telemetry, and the refusals the full verifier chain produces. Postgres is
// the real test database; Redis is a per-test miniredis (the
// router_klipy_ratelimit_test.go exception: a private keyspace and a clock the
// test controls).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	stepUpMintPath = "/api/v1/auth/step-up/password"
	// stepUpMintRoute is the route's security-event template, spelled as the
	// wire string so the test pins the value a consumer of the stream sees.
	stepUpMintRoute = securityevent.RouteTemplate("POST /api/v1/auth/step-up/password")
)

// mintEvents records every security event the router emits. The observer and
// the handler both emit, from the request goroutine; the mutex keeps the
// race detector quiet if that ever changes.
type mintEvents struct {
	mu     sync.Mutex
	events []securityevent.Event
}

func (r *mintEvents) Emit(_ context.Context, event securityevent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *mintEvents) onMintRoute() []securityevent.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []securityevent.Event
	for _, event := range r.events {
		if event.RouteTemplate == stepUpMintRoute {
			out = append(out, event)
		}
	}
	return out
}

type mintRouterFixture struct {
	ts     *testhelpers.TestServer
	mr     *miniredis.Miniredis
	events *mintEvents
	router http.Handler
}

func newMintRouterFixture(t *testing.T) *mintRouterFixture {
	t.Helper()
	t.Setenv("CONCORD_ENV", "test")

	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	events := &mintEvents{}

	router, hub, natsClient, opsRuntime, permissionEnforcer, _, closePresence, _, _, _, err := api.NewRouter(
		t.Context(), db, rdb,
		&config.Config{
			Environment:                      "test",
			Port:                             "0",
			JWTSecret:                        testhelpers.TestJWTSecret,
			AllowedOrigins:                   []string{"*"},
			MFAEncryptionKey:                 "0000000000000000000000000000000000000000000000000000000000000000",
			MFAEncryptionKeyVersion:          1,
			WebAuthnRPID:                     "localhost",
			WebAuthnRPOrigins:                []string{"http://localhost:3001"},
			ActivityHistoryClusterEnabled:    true,
			ControlPlaneReplicaCount:         1,
			ControlPlaneReplicaCountExplicit: true,
		},
		nil, logger.NewWithWriter(io.Discard),
		api.RouterDependencies{
			PresenceHistory: presencehistory.NewService(
				db, presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}), true,
			),
			SecurityEvents: events,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		closePresence()
		hub.Shutdown()
		permissionEnforcer.Close()
		if natsClient != nil {
			_ = natsClient.Close()
		}
		require.NoError(t, opsRuntime.Stop(context.Background()))
	})
	return &mintRouterFixture{ts: &testhelpers.TestServer{DB: db}, mr: mr, events: events, router: router}
}

func (f *mintRouterFixture) mint(t *testing.T, user testhelpers.TestUser, password, purpose string) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"current_password": password, "purpose": purpose})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, stepUpMintPath, bytes.NewReader(payload))
	req.Header = testhelpers.AuthHeaders(user.AccessToken)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body) // a non-JSON body leaves body nil, and the assertions on it fail
	return w.Code, body
}

// regression for #3509 review (security M1, CWE-778): the mint verified
// passwords and recorded nothing, where /login records every wrong password.
func TestMintRoute_WrongPasswordEmitsAnAuthenticationDenial(t *testing.T) {
	f := newMintRouterFixture(t)
	user := f.ts.CreateTestUser(t, "mint_event_wrong")

	status, _ := f.mint(t, user, "wrong-password", "dm.clear")

	require.Equal(t, http.StatusForbidden, status)
	assert.Contains(t, f.events.onMintRoute(), securityevent.Event{
		EventType:     securityevent.EventAuthentication,
		Outcome:       securityevent.OutcomeDenied,
		Severity:      securityevent.SeverityMedium,
		ReasonCode:    securityevent.ReasonInvalidCredentials,
		AuthMethod:    securityevent.AuthPassword,
		RouteTemplate: stepUpMintRoute,
	}, "a wrong password at the mint must be recorded exactly as /login records one")
}

// regression for #3509 review (security L2): only the per-IP limiter guarded
// the mint, so one account could be guessed at from many addresses, bounded by
// nothing but the email lockout. A per-user cap no looser than the per-IP one
// (10 per 15 minutes) must refuse the eleventh mint.
func TestMintRoute_PerUserCapRefusesTheEleventhMint(t *testing.T) {
	f := newMintRouterFixture(t)
	user := f.ts.CreateTestUser(t, "mint_user_cap")

	for i := range 10 {
		status, body := f.mint(t, user, user.Password, "dm.clear")
		require.Equal(t, http.StatusOK, status, "mint %d: %v", i+1, body)
	}
	status, body := f.mint(t, user, user.Password, "dm.clear")

	assert.Equal(t, http.StatusTooManyRequests, status, "the eleventh mint in 15 minutes: %v", body)
}

// regression for #3509 review (security L5): an account with no usable
// password factor got an opaque 500 from the mint, where the route it came
// from answers the actionable 400 NoFactors. The mint must answer that same
// 400, before any verification.
func TestMintRoute_NoPasswordFactorIsTheRoutesNoFactors400(t *testing.T) {
	f := newMintRouterFixture(t)
	user := f.ts.CreateTestUser(t, "mint_no_factor")
	_, err := f.ts.DB.Exec(`UPDATE users SET password_hash = '' WHERE id = $1`, user.ID)
	require.NoError(t, err)

	status, body := f.mint(t, user, "anything", "dm.clear")

	assert.Equal(t, http.StatusBadRequest, status, "%v", body)
	assert.Equal(t,
		"Clear history requires verification, but this account has no password and no MFA method. Set a password, enable MFA, or turn off \"Require authentication before purging\" in Privacy & Security.",
		body["error"], "the copy DM Clear itself answers with")
}

// The mint's other two decisions reach Nightwatch in /login's shapes too: the
// shared lockout's 423 and a successful mint.
//
// Mutants killed: dropping either emit (the event is missing).
func TestMintRoute_LockoutAndSuccessEmitTheirEvents(t *testing.T) {
	f := newMintRouterFixture(t)
	user := f.ts.CreateTestUser(t, "mint_event_paths")

	status, _ := f.mint(t, user, user.Password, "dm.clear")
	require.Equal(t, http.StatusOK, status)
	for range 5 {
		status, _ = f.mint(t, user, "wrong-password", "dm.clear")
		require.Equal(t, http.StatusForbidden, status)
	}
	status, _ = f.mint(t, user, user.Password, "dm.clear")
	require.Equal(t, http.StatusLocked, status)

	events := f.events.onMintRoute()
	assert.Contains(t, events, securityevent.Event{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonAuthenticationSucceeded,
		AuthMethod: securityevent.AuthPassword, RouteTemplate: stepUpMintRoute,
	})
	assert.Contains(t, events, securityevent.Event{
		EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAccountLocked,
		AuthMethod: securityevent.AuthPassword, RouteTemplate: stepUpMintRoute,
	})
	for _, event := range events {
		assert.Empty(t, event.EvidenceRef, "the mint's events carry no dimension")
	}
}
