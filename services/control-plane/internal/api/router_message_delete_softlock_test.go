package api_test

// AC1-router for the delete-rate soft-lock (#3455, design spec §5 "Router
// (AC1-router)" and §6). The handler-seam tests in internal/messages and
// internal/dm mount the handlers WITHOUT the route limiter, which is the only
// way to reach a 16th delete there. These tests drive the REAL api.NewRouter,
// so they cover what a handler test structurally cannot: that the route
// limiter RateLimitByUser(10, 1m) and the soft-lock compose in the order the
// spec pins (limiter first), that the router wired each handler's Redis client
// and MFA verifier (a confirmed retry only succeeds through the real
// verifier), and that both delete routes are the ones carrying the soft-lock.
//
// Both routes' soft-lock and limiter share ONE Redis, exactly as in
// production (the router hands its single client to both), and it is an
// in-process miniredis. That is the exception router_klipy_ratelimit_test.go
// records: not isolation, but a frozen clock. TTLs advance only through
// FastForward, so a test can cross the limiter's 60 s window and the
// soft-lock's 30 s window without sleeping. Postgres is the real test
// database.
//
// Why the scenario is shaped as it is. The limiter admits 10 requests per
// fixed 60 s window, so 16 deletes cannot land inside one soft-lock window
// unless the burst straddles a limiter window boundary, which is precisely
// the case the soft-lock exists for (spec §2.1). The scenario builds that
// straddle on the frozen clock:
//
//	t=0     one delete                     limiter 1   soft-lock 1
//	t=31    (soft-lock window has expired) 9 deletes   limiter 10  soft-lock 9
//	t=60.5  (limiter window has expired, soft-lock has 0.5 s left)
//	        6 deletes succeed              limiter 6   soft-lock 15
//	        the 7th is soft-locked         limiter 7   soft-lock 16
//	        a retry with a valid code      limiter 8   key deleted
//	        2 deletes succeed              limiter 10  soft-lock 2
//	        the next hits the limiter      limiter 11  soft-lock still 2

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// The route limiter's cap on both delete routes (router.go), pinned here as
	// a literal so a loosened limiter is a red test rather than a silent change
	// of what "ahead of the soft-lock" means.
	deleteRouteLimit = 10

	channelDeleteLimiterRoute = "/api/v1/messages/:id"
	dmDeleteLimiterRoute      = "/api/v1/dm/conversations/:id/messages/:message_id"

	channelMessageExists = `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`
	dmMessageExists      = `SELECT EXISTS (SELECT 1 FROM dm_messages WHERE id = $1)`
)

// deleteRoute is one delete route under test with its own actor, messages and
// Redis keys, so two routes never share a counter.
type deleteRoute struct {
	name       string
	actor      testhelpers.TestUser
	messageIDs []string
	path       func(messageID string) string
	existsSQL  string
	burstKey   string // the soft-lock counter the route's population uses
	limiterKey string // the route limiter's counter for the actor
	code       func() string
}

// deleteRouterFixture is one real router over the test database and one
// miniredis.
type deleteRouterFixture struct {
	db     *sql.DB
	mr     *miniredis.Miniredis
	router http.Handler
}

func newDeleteRouterFixture(t *testing.T) *deleteRouterFixture {
	t.Helper()
	t.Setenv("CONCORD_ENV", "test")

	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

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
		api.RouterDependencies{PresenceHistory: presencehistory.NewService(
			db, presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}), true,
		)},
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

	return &deleteRouterFixture{db: db, mr: mr, router: router}
}

// enrollTOTP gives the user a confirmed, verifiable TOTP factor sealed with
// the router's all-zero test keyring, so the router's REAL mfa.Handler
// verifies the code, and returns a generator for the current code. Enroll
// before the user's first request: the permission cache is keyed on MFA
// generation, which a raw insert does not bump.
func (f *deleteRouterFixture) enrollTOTP(t *testing.T, user testhelpers.TestUser) func() string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Concord Voice", AccountName: user.Email})
	require.NoError(t, err)
	sealed, nonce, err := mfa.EncryptSecret([]byte(key.Secret()), make([]byte, 32))
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = ARRAY['totp']::TEXT[] WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, 1, TRUE, TRUE)`, user.ID, sealed, nonce)
	require.NoError(t, err)
	return func() string {
		code, genErr := totp.GenerateCode(key.Secret(), time.Now())
		require.NoError(t, genErr)
		return code
	}
}

func uniqueSuffix() string { return strings.ReplaceAll(uuid.NewString()[:8], "-", "") }

// channelRoute builds a server that enforces MFA on dangerous actions (the
// server rule), an enrolled member, and n of that member's messages. The
// member deletes their own messages, so the permission check passes.
func (f *deleteRouterFixture) channelRoute(t *testing.T, n int) deleteRoute {
	t.Helper()
	ts := &testhelpers.TestServer{DB: f.db}
	suffix := uniqueSuffix()
	owner := ts.CreateTestUser(t, "slr_owner_"+suffix)
	actor := ts.CreateTestUser(t, "slr_member_"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "softlock-router-"+suffix)
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	_, err := f.db.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, serverID)
	require.NoError(t, err)
	code := f.enrollTOTP(t, actor)

	rows, err := f.db.Query(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		SELECT gen_random_uuid(), $1, $2, 'softlock-router', 1, FALSE, NOW() + g * INTERVAL '1 millisecond', NOW()
		FROM generate_series(1, $3) AS g
		RETURNING id`, channelID, actor.ID, n)
	require.NoError(t, err)

	return deleteRoute{
		name:       "channel delete",
		actor:      actor,
		messageIDs: scanIDs(t, rows),
		path:       func(id string) string { return "/api/v1/messages/" + id },
		existsSQL:  channelMessageExists,
		burstKey: stepup.NewDeleteSoftLock(nil).Key(uuid.MustParse(actor.ID),
			stepup.ServerDeleteScope(uuid.MustParse(serverID))),
		limiterKey: "ratelimit:user:" + actor.ID + ":DELETE:" + channelDeleteLimiterRoute,
		code:       code,
	}
}

// dmRoute builds a DM the actor authors n messages in. The actor has no
// privacy_settings row, which reads as require_auth_before_purge ON, so the
// own rule puts them in the population.
func (f *deleteRouterFixture) dmRoute(t *testing.T, n int) deleteRoute {
	t.Helper()
	ts := &testhelpers.TestServer{DB: f.db}
	suffix := uniqueSuffix()
	actor := ts.CreateTestUser(t, "slr_dm_actor_"+suffix)
	peer := ts.CreateTestUser(t, "slr_dm_peer_"+suffix)
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	code := f.enrollTOTP(t, actor)

	rows, err := f.db.Query(`
		INSERT INTO dm_messages (id, conversation_id, user_id, content, type, created_at)
		SELECT gen_random_uuid(), $1, $2, 'softlock-router', 'text', NOW() + g * INTERVAL '1 millisecond'
		FROM generate_series(1, $3) AS g
		RETURNING id`, convID, actor.ID, n)
	require.NoError(t, err)

	return deleteRoute{
		name:       "DM delete",
		actor:      actor,
		messageIDs: scanIDs(t, rows),
		path:       func(id string) string { return "/api/v1/dm/conversations/" + convID + "/messages/" + id },
		existsSQL:  dmMessageExists,
		burstKey:   stepup.NewDeleteSoftLock(nil).Key(uuid.MustParse(actor.ID), stepup.DMDeleteScope()),
		limiterKey: "ratelimit:user:" + actor.ID + ":DELETE:" + dmDeleteLimiterRoute,
		code:       code,
	}
}

func scanIDs(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// deleteAs sends DELETE for messageID as the route's actor through the real
// router. body is JSON-encoded when non-nil.
func (f *deleteRouterFixture) deleteAs(t *testing.T, r deleteRoute, messageID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(http.MethodDelete, r.path(messageID), bytes.NewReader(payload))
	req.Header = testhelpers.AuthHeaders(r.actor.AccessToken)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

// counter reads a miniredis counter, "" when the key is absent.
func (f *deleteRouterFixture) counter(key string) string {
	if !f.mr.Exists(key) {
		return ""
	}
	v, _ := f.mr.Get(key)
	return v
}

// requireCounts pins both counters at once: the limiter's and the soft-lock's.
// "" means the key is absent.
func (f *deleteRouterFixture) requireCounts(t *testing.T, r deleteRoute, step, limiter, burst string) {
	t.Helper()
	require.Equal(t, limiter, f.counter(r.limiterKey), "%s: route limiter counter", step)
	require.Equal(t, burst, f.counter(r.burstKey), "%s: soft-lock counter", step)
}

func (f *deleteRouterFixture) messageExists(t *testing.T, r deleteRoute, id string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.db.QueryRow(r.existsSQL, id).Scan(&exists))
	return exists
}

func (f *deleteRouterFixture) requireDeleted(t *testing.T, r deleteRoute, index int, step string) {
	t.Helper()
	w := f.deleteAs(t, r, r.messageIDs[index], nil)
	require.Equal(t, http.StatusOK, w.Code, "%s: delete %d: %s", step, index, w.Body.String())
	require.False(t, f.messageExists(t, r, r.messageIDs[index]), "%s: delete %d must remove the row", step, index)
}

// TestRouter_DeleteSoftLockAcrossLimiterWindowBoundary is AC1-router: through
// the real limiter and handler, the soft-lock is reachable across a limiter
// window boundary, a limiter 429 is never counted, and a confirmed retry
// resets the counter.
//
// Mutants it kills, each on the step named. Those marked (run) were applied
// to production, seen red and restored byte-identical:
//   - the route's limiter dropped from either route (run): step 1, the
//     limiter counter is absent;
//   - the channel limiter loosened past 10 (run, 20): the last step returns
//     200, not 429;
//   - the soft-lock tripping at 15 rather than 16 (run, threshold 14): step 4,
//     the 15th delete is refused;
//   - the handler skipping Reset after a verified confirmation (run): step 5,
//     the counter reads 17 rather than being deleted;
//   - the router not wiring the messages handler's Redis (run): the boot
//     guard exits the test binary before any request;
//   - the soft-lock counting a request the limiter refused, spec §5's "count
//     before the limiter": last step, the counter reads 3 rather than 2;
//   - the soft-lock's window tied to the limiter's rather than its own 30 s:
//     step 3 or step 4, where the counter would have restarted or the 7th
//     delete would not trip;
//   - a verifier that cannot verify a real TOTP, or a refusal that omits
//     delete_rate_limited, mfa_required or Retry-After, or a router layer that
//     rewrites them: steps 4 and 5.
func TestRouter_DeleteSoftLockAcrossLimiterWindowBoundary(t *testing.T) {
	f := newDeleteRouterFixture(t)
	const messageCount = 20

	routes := map[string]deleteRoute{
		"channel": f.channelRoute(t, messageCount),
		"dm":      f.dmRoute(t, messageCount),
	}
	for name, r := range routes {
		t.Run(name, func(t *testing.T) {
			// 1. One delete opens both windows.
			f.requireDeleted(t, r, 0, "step 1")
			f.requireCounts(t, r, "step 1", "1", "1")

			// 2. The soft-lock window (30 s) lapses inside the limiter's (60 s).
			// Nine deletes fill the limiter to its cap of 10 and start a fresh
			// soft-lock window at 9.
			f.mr.FastForward(31 * time.Second)
			require.False(t, f.mr.Exists(r.burstKey), "the soft-lock window must have lapsed")
			require.True(t, f.mr.Exists(r.limiterKey), "the limiter window must still be open")
			for i := 1; i <= 9; i++ {
				f.requireDeleted(t, r, i, "step 2")
			}
			f.requireCounts(t, r, "step 2", "10", "9")

			// 3. The limiter's window lapses with 0.5 s left on the soft-lock's:
			// the boundary straddle.
			f.mr.FastForward(29500 * time.Millisecond)
			require.False(t, f.mr.Exists(r.limiterKey), "the limiter window must have lapsed")
			require.Positive(t, f.mr.TTL(r.burstKey), "the soft-lock window must still be open")

			// 4. Soft-lock counts 10 to 15 pass; the 16th is refused.
			for i := 10; i <= 15; i++ {
				f.requireDeleted(t, r, i, "step 4")
			}
			f.requireCounts(t, r, "step 4 before the trip", "6", "15")
			refused := f.deleteAs(t, r, r.messageIDs[16], nil)
			require.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(refused.Body.Bytes(), &body), refused.Body.String())
			assert.Equal(t, true, body["mfa_required"])
			assert.Equal(t, true, body["delete_rate_limited"])
			assert.Equal(t, []any{"totp"}, body["methods"])
			assert.Equal(t, "1", refused.Header().Get("Retry-After"), "0.5 s left rounds up to the 1 s minimum")
			assert.True(t, f.messageExists(t, r, r.messageIDs[16]), "a soft-locked delete must not remove the row")
			f.requireCounts(t, r, "step 4 after the trip", "7", "16")

			// 5. The retry with a valid code passes the limiter, is verified by
			// the router's own MFA handler, deletes, and resets the counter.
			confirmed := f.deleteAs(t, r, r.messageIDs[16], map[string]string{"mfa_code": r.code()})
			require.Equal(t, http.StatusOK, confirmed.Code, confirmed.Body.String())
			assert.False(t, f.messageExists(t, r, r.messageIDs[16]))
			f.requireCounts(t, r, "step 5", "8", "")
			budget := stepup.NewBudget(nil, stepup.MFASettingsBudgetPrefix).Key(r.actor.ID)
			assert.Empty(t, f.counter(budget), "a verified confirmation clears the step-up budget")

			// 6. The counter restarted: two more deletes are counted 1 and 2.
			f.requireDeleted(t, r, 17, "step 6")
			f.requireCounts(t, r, "step 6a", "9", "1")
			f.requireDeleted(t, r, 18, "step 6")
			f.requireCounts(t, r, "step 6b", "10", "2")

			// 7. The limiter is at its cap. Its 429 is never counted.
			limited := f.deleteAs(t, r, r.messageIDs[19], nil)
			require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.Body.String())
			assert.Contains(t, limited.Body.String(), "Rate limit exceeded")
			assert.True(t, f.messageExists(t, r, r.messageIDs[19]))
			f.requireCounts(t, r, "step 7", "11", "2")
		})
	}
}

// TestRouter_DeleteRouteLimiterRunsAheadOfSoftLock pins the ordering on its
// own, with no clock movement: the limiter runs first and its refusals are
// never counted (design spec §2.8, "the route limiter RateLimitByUser(10,1m)
// is unchanged"). Ten deletes fill the limiter; the eleventh is a 429 that
// leaves the soft-lock counter at 10.
//
// Mutants it kills: the limiter dropped from either route or loosened (the
// eleventh delete succeeds and the counter reads 11); the soft-lock moved
// ahead of the limiter or counting its refusals (the counter reads 11 behind
// a 429).
func TestRouter_DeleteRouteLimiterRunsAheadOfSoftLock(t *testing.T) {
	f := newDeleteRouterFixture(t)

	routes := map[string]deleteRoute{
		"channel": f.channelRoute(t, deleteRouteLimit+1),
		"dm":      f.dmRoute(t, deleteRouteLimit+1),
	}
	for name, r := range routes {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < deleteRouteLimit; i++ {
				f.requireDeleted(t, r, i, "within the limiter")
			}
			f.requireCounts(t, r, "at the limiter cap", "10", "10")

			limited := f.deleteAs(t, r, r.messageIDs[deleteRouteLimit], nil)
			require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.Body.String())
			assert.NotEmpty(t, limited.Header().Get("Retry-After"))
			assert.True(t, f.messageExists(t, r, r.messageIDs[deleteRouteLimit]))
			f.requireCounts(t, r, "behind the limiter's 429", "11", "10")
		})
	}
}
