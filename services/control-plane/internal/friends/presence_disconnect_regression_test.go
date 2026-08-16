// Package friends_test — disconnect-abuse REGRESSIONS for PR #2738 (issue #2446).
//
// These began as adversarial exploit proofs from an @red-team pass and all five
// PASSED against the pre-fix code: an authenticated caller could name a stranger
// and force a full websocket teardown of that stranger's every device, fan a
// disconnect out over their whole Server Voice audience, and time a 404 to learn
// whether they were in voice. Each assertion is now INVERTED — it asserts the
// side effect does NOT land. If any of these starts failing, the vulnerability
// class is back.
//
// The gate that closes all five lives in graphpresence.CaptureInTx: a revoking
// family with a counterpart reconciles nothing unless an ACCEPTED friendship
// edge exists, because the presence audience is derived from accepted rows only.
//
// Run against the standard test database, like every other suite here:
//
//	go test ./internal/friends/ -run 'TestBlockingAStranger|TestRepeatedBlock|TestCaptureNamingAStranger|TestAbortedRemoveFriend|TestRemoveFriendDoesNotLeak' -count=1 -v
//
// (The original PoC header carried an explicit DATABASE_URL with an inline
// password. It is removed rather than allowlisted: detect-secrets is right that
// a credential-shaped string does not belong in the tree, and the suite needs no
// special database.)
package friends_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/graphpresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ---------------------------------------------------------------------------
// Doubles standing in for the production sinks.
// ---------------------------------------------------------------------------

// recordingDisconnector stands in for *websocket.Hub. The production
// implementation is Hub.DisconnectRichPresenceClients
// (internal/websocket/richpresence.go:101), which iterates
// h.userClients[userID] and closes EVERY local device for each included user —
// no rich-presence capability filter. Recording the recipient set therefore
// records exactly whose websocket sessions production would tear down.
type recordingDisconnector struct {
	mu     sync.Mutex
	calls  int
	seen   map[uuid.UUID]int
	global int
}

func newRecordingDisconnector() *recordingDisconnector {
	return &recordingDisconnector{seen: map[uuid.UUID]int{}}
}

func (d *recordingDisconnector) DisconnectRichPresenceClients(
	_ context.Context, recipients map[uuid.UUID]bool,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	for id, included := range recipients {
		if included {
			d.seen[id]++
		}
	}
	return nil
}

// The global escalation MUST be recorded. It returned nil and counted nothing,
// so every assertion in this file — all of which read hits(), populated only by
// the targeted path — stayed green through the worst possible outcome: a
// targeted disconnect that failed and escalated to tearing down every connected
// user. The exploit these tests exist to catch would have been invisible in its
// most severe form (PR #2738 review, CodeRabbit).
func (d *recordingDisconnector) DisconnectAllRichPresenceClients(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.global++
	return nil
}

// globalCalls reports how many times the fail-closed sink escalated to a global
// teardown. Every regression here must assert this is zero.
func (d *recordingDisconnector) globalCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.global
}

func (d *recordingDisconnector) hits(id uuid.UUID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen[id]
}

func (d *recordingDisconnector) distinct() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

type noopRefresher struct{}

func (noopRefresher) RefreshServerVoiceRecheck(
	context.Context, uuid.UUID, presence.Scope, map[uuid.UUID]bool,
) error {
	return nil
}

// alwaysPermittedResolver stands in for presence.SenderPresenceResolver. In
// production this is satisfied by any user whose persisted presence status
// permits emission — i.e. any normal online user.
type alwaysPermittedResolver struct{}

func (alwaysPermittedResolver) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return true
}

func (d alwaysPermittedResolver) RichPresenceEmissionState(
	ctx context.Context, senderID uuid.UUID,
) (bool, error) {
	// Test double: always DETERMINED, so it exercises the
	// suppression path rather than the indeterminate one.
	return d.RichPresenceEmissionPermitted(ctx, senderID), nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type pocEnv struct {
	ts   *testhelpers.TestServer
	rec  *graphpresence.Reconciler
	disc *recordingDisconnector
}

func newPoCEnv(t *testing.T) *pocEnv {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	disc := newRecordingDisconnector()
	rec := graphpresence.New(ts.DB, noopRefresher{}, disc, alwaysPermittedResolver{}, logger.New("poc"))
	t.Cleanup(rec.Close)
	return &pocEnv{ts: ts, rec: rec, disc: disc}
}

// attackerEngine mounts the real friends handler wired to the REAL reconciler,
// authenticated as attackerID. Everything downstream of `c.GetString("user_id")`
// is production code.
func (e *pocEnv) attackerEngine(
	t *testing.T, attackerID string, capture presencecapture.GraphPresenceCapture,
	register func(*gin.Engine, *friends.Handler),
) *gin.Engine {
	t.Helper()
	h := friends.NewHandler(e.ts.DB, logger.New("poc"), nil)
	h.SetGraphPresenceCapture(capture)
	require.True(t, h.HasGraphPresenceCapture())

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("user_id", attackerID)
		c.Next()
	})
	register(engine, h)
	return engine
}

// setServerVoiceTier upserts user_presence_settings so the sender's Server
// Voice audience is the whole server (tier 2 = Servers). Any user can set this
// from the client; it is a normal privacy preference.
func setServerVoiceTier(t *testing.T, db *sql.DB, userID uuid.UUID, tier int) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (user_id, master_enabled, server_voice_tier)
		VALUES ($1, TRUE, $2)
		ON CONFLICT (user_id) DO UPDATE
		SET master_enabled = TRUE, server_voice_tier = EXCLUDED.server_voice_tier
	`, userID, tier)
	require.NoError(t, err)
}

func joinVoice(t *testing.T, db *sql.DB, serverID, userID uuid.UUID) {
	t.Helper()
	channelID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO channels (id, server_id, name, type)
		 VALUES ($1, $2, 'v_' || left($3, 8), 'voice')`,
		channelID, serverID, channelID.String(),
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
		channelID, userID,
	)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// PoC 1 — POST /friends/:user_id/block against a total stranger force-closes
// every websocket device the victim has. No relationship required. Repeatable.
// ---------------------------------------------------------------------------

func TestBlockingAStrangerDisconnectsNobody(t *testing.T) {
	env := newPoCEnv(t)

	attacker := testhelpers.CreateUser(t, env.ts.DB)
	victim := testhelpers.CreateUser(t, env.ts.DB)
	// Deliberately NO friendship, NO pending request, NO shared server, NO
	// block. The two accounts have never interacted.

	engine := env.attackerEngine(t, attacker.String(), env.rec,
		func(e *gin.Engine, h *friends.Handler) {
			e.POST("/friends/:user_id/block", h.BlockUser)
		})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(
		http.MethodPost, "/friends/"+victim.String()+"/block", nil))
	require.Equal(t, http.StatusOK, w.Code, "block of a stranger succeeds")

	// Both outcomes are polled over the SAME window. These handler-driven tests
	// commit through presencehook.Complete, which enqueues to the asynchronous
	// dispatch sink, so an immediate sample of globalCalls() reads it at t=0 —
	// before the worker could have escalated — and an escalation milliseconds
	// later would pass unseen (PR #2738 review, CodeRabbit). The direct-Abandon
	// tests below sample immediately on purpose; that path is synchronous.
	require.Never(t, func() bool {
		return env.disc.hits(victim) >= 1 || env.disc.globalCalls() > 0
	},
		3*time.Second, 10*time.Millisecond,
		"REGRESSION: blocking a stranger must disconnect nobody and must not escalate "+
			"to a global teardown — no accepted edge existed, so no visibility was revoked")

	t.Logf("regression: attacker %s blocked stranger %s -> victim disconnect calls=%d (want 0)",
		attacker, victim, env.disc.hits(victim))
}

// Repeating the block is what made the original defect a weapon rather than a
// one-shot: the write is idempotent-SUCCESSFUL, so rowsAffected stays 1 and
// every round re-committed and disconnected the victim again — an unbounded
// teardown loop against any user, from an attacker-supplied path parameter.
//
// The accepted-edge gate makes each round produce an empty plan, so the correct
// count is ZERO, not merely "fewer than the number of rounds".
func TestRepeatedBlockOfAStrangerStaysANoOp(t *testing.T) {
	env := newPoCEnv(t)

	attacker := testhelpers.CreateUser(t, env.ts.DB)
	victim := testhelpers.CreateUser(t, env.ts.DB)

	engine := env.attackerEngine(t, attacker.String(), env.rec,
		func(e *gin.Engine, h *friends.Handler) {
			e.POST("/friends/:user_id/block", h.BlockUser)
		})

	const rounds = 5
	for i := 0; i < rounds; i++ {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(
			http.MethodPost, "/friends/"+victim.String()+"/block", nil))
		require.Equal(t, http.StatusOK, w.Code, "re-block round %d", i)
	}

	// Same window for both outcomes — see the note in
	// TestBlockingAStrangerDisconnectsNobody on why an immediate globalCalls()
	// sample is unsound on the handler-driven path.
	require.Never(t, func() bool {
		return env.disc.hits(victim) > 0 || env.disc.globalCalls() > 0
	},
		3*time.Second, 10*time.Millisecond,
		"REGRESSION: no round of a stranger-block may disconnect the victim or escalate "+
			"to a global teardown — no accepted edge ever existed")

	t.Logf("regression: %d repeat blocks -> %d victim disconnects", rounds, env.disc.hits(victim))
}

// ---------------------------------------------------------------------------
// PoC 2 — Abandon fan-out. The captured plan for an attacker-NAMED stranger
// contains that stranger's entire Server Voice audience, and every abandon
// terminal disconnects all of them.
// ---------------------------------------------------------------------------

func TestCaptureNamingAStrangerProducesAnEmptyPlan(t *testing.T) {
	env := newPoCEnv(t)
	ctx := context.Background()

	attacker := testhelpers.CreateUser(t, env.ts.DB)
	victim := testhelpers.CreateUser(t, env.ts.DB)

	// The victim is an ordinary user sitting in a busy public voice channel.
	serverID := testhelpers.CreateServer(t, env.ts.DB, victim)
	testhelpers.AddServerMember(t, env.ts.DB, serverID, victim)
	setServerVoiceTier(t, env.ts.DB, victim, 2) // Servers
	const bystanders = 60
	audience := make([]uuid.UUID, 0, bystanders)
	for i := 0; i < bystanders; i++ {
		u := testhelpers.CreateUser(t, env.ts.DB)
		testhelpers.AddServerMember(t, env.ts.DB, serverID, u)
		audience = append(audience, u)
	}
	joinVoice(t, env.ts.DB, serverID, victim)

	// The attacker NAMES the victim. Capture is unconditional and runs before
	// any precondition on the friendship existing.
	tx, err := env.ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	plan, err := env.rec.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		FailPosture: presencecapture.FailClosedBlockWrite,
		Principal:   attacker,
		Counterpart: victim,
	})
	require.NoError(t, err)
	require.False(t, plan.HasWork(),
		"REGRESSION: a capture naming a stranger must produce an empty plan")

	// CauseCommitUnresolved, not CauseWriteFailed: the write-failed and
	// rows-affected causes now take the CauseProvesNoCommit early return, so
	// asserting on them would pass without the fan-out path ever running. The
	// unresolved-commit cause is the one terminal that still fans out, which
	// makes this the strongest available assertion — an empty plan must
	// disconnect nobody even on the path that deliberately fails closed.
	env.rec.Abandon(plan, presencecapture.CauseCommitUnresolved)

	// Counted immediately, deliberately. Abandon -> abandonPlan -> disconnect is
	// fully synchronous, so every disconnect this call will ever make has
	// already happened by the time it returns. The handler-driven PoCs above
	// need require.Never because they reach the sink through Complete, which
	// enqueues to the async dispatch worker; this one does not, and polling for
	// three seconds here would buy no assurance at all (PR #2738 review — the
	// one CodeRabbit sub-point declined, on that distinction).
	hit := 0
	for _, u := range audience {
		if env.disc.hits(u) > 0 {
			hit++
		}
	}
	require.Zero(t, env.disc.globalCalls(),
		"no global teardown may be escalated — that outcome is worse than the targeted one this test measures")
	require.Zero(t, hit,
		"REGRESSION: an abandon over an empty plan must disconnect nobody in the "+
			"named stranger's voice audience")
	t.Logf("regression: 1 abandon over an empty plan -> %d distinct users disconnected, "+
		"%d of the victim's voice audience (both must be 0)",
		env.disc.distinct(), hit)
}

// ---------------------------------------------------------------------------
// PoC 3 — handler-level reachability of the abandon fan-out on DELETE
// /friends/:user_id. The seed fix (affe2b3c3) guarded only rowsAffected == 0;
// the write_failed branch above it is unguarded and reachable by aborting the
// HTTP request after the capture read completes (Gin cancels
// c.Request.Context() when the client hangs up, which fails tx.ExecContext).
//
// cancelAfterCapture pins that timing deterministically. It is a TEST CLOCK,
// not a behaviour change: it delegates every method to the real reconciler and
// only fires the cancel the client would have caused.
// ---------------------------------------------------------------------------

type cancelAfterCapture struct {
	inner  presencecapture.GraphPresenceCapture
	cancel context.CancelFunc
}

func (c *cancelAfterCapture) CaptureInTx(
	ctx context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	plan, err := c.inner.CaptureInTx(ctx, tx, subject)
	c.cancel() // the client hangs up here
	return plan, err
}

func (c *cancelAfterCapture) Complete(
	ctx context.Context, tx *sql.Tx, plan presencecapture.Plan,
) error {
	return c.inner.Complete(ctx, tx, plan)
}

func (c *cancelAfterCapture) Abandon(plan presencecapture.Plan, cause presencecapture.Cause) {
	c.inner.Abandon(plan, cause)
}

func TestAbortedRemoveFriendDisconnectsNobody(t *testing.T) {
	env := newPoCEnv(t)

	attacker := testhelpers.CreateUser(t, env.ts.DB)
	victim := testhelpers.CreateUser(t, env.ts.DB)

	serverID := testhelpers.CreateServer(t, env.ts.DB, victim)
	testhelpers.AddServerMember(t, env.ts.DB, serverID, victim)
	setServerVoiceTier(t, env.ts.DB, victim, 2)
	const bystanders = 40
	audience := make([]uuid.UUID, 0, bystanders)
	for i := 0; i < bystanders; i++ {
		u := testhelpers.CreateUser(t, env.ts.DB)
		testhelpers.AddServerMember(t, env.ts.DB, serverID, u)
		audience = append(audience, u)
	}
	joinVoice(t, env.ts.DB, serverID, victim)

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := &cancelAfterCapture{inner: env.rec, cancel: cancel}

	engine := env.attackerEngine(t, attacker.String(), capture,
		func(e *gin.Engine, h *friends.Handler) {
			e.DELETE("/friends/:user_id", h.RemoveFriend)
		})

	req := httptest.NewRequest(http.MethodDelete, "/friends/"+victim.String(), nil).
		WithContext(reqCtx)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// The attacker has NO friendship with the victim; the write would have been
	// a 404 no-op. The aborted request instead reaches the write_failed abandon.
	require.Equal(t, http.StatusInternalServerError, w.Code)

	hit := 0
	for _, u := range audience {
		if env.disc.hits(u) > 0 {
			hit++
		}
	}
	require.Zero(t, env.disc.globalCalls(),
		"no global teardown may be escalated — that outcome is worse than the targeted one this test measures")
	require.Zero(t, hit,
		"REGRESSION: an aborted DELETE proves no commit, so it must disconnect nobody")
	t.Logf("regression check: aborted DELETE /friends/%s -> %d disconnected (want 0)",
		victim, env.disc.distinct())
}

// ---------------------------------------------------------------------------
// PoC 4 — presence/voice oracle. The capture runs BEFORE the friendship
// precondition, so DELETE /friends/<stranger> does O(audience) database work
// against a user the caller has no edge to. The response is the same 404 in
// both cases; only the latency differs. This measures the oracle.
// ---------------------------------------------------------------------------

func TestRemoveFriendDoesNotLeakVoiceStateByTiming(t *testing.T) {
	env := newPoCEnv(t)

	attacker := testhelpers.CreateUser(t, env.ts.DB)
	idle := testhelpers.CreateUser(t, env.ts.DB)
	inVoice := testhelpers.CreateUser(t, env.ts.DB)

	serverID := testhelpers.CreateServer(t, env.ts.DB, inVoice)
	testhelpers.AddServerMember(t, env.ts.DB, serverID, inVoice)
	setServerVoiceTier(t, env.ts.DB, inVoice, 2)
	for i := 0; i < 400; i++ {
		u := testhelpers.CreateUser(t, env.ts.DB)
		testhelpers.AddServerMember(t, env.ts.DB, serverID, u)
	}
	joinVoice(t, env.ts.DB, serverID, inVoice)

	engine := env.attackerEngine(t, attacker.String(), env.rec,
		func(e *gin.Engine, h *friends.Handler) {
			e.DELETE("/friends/:user_id", h.RemoveFriend)
		})

	probe := func(target uuid.UUID) time.Duration {
		best := time.Hour
		for i := 0; i < 7; i++ {
			w := httptest.NewRecorder()
			start := time.Now()
			engine.ServeHTTP(w, httptest.NewRequest(
				http.MethodDelete, "/friends/"+target.String(), nil))
			d := time.Since(start)
			require.Equal(t, http.StatusNotFound, w.Code,
				"both probes must return the same status")
			if d < best {
				best = d
			}
		}
		return best
	}

	idleT := probe(idle)
	voiceT := probe(inVoice)

	t.Logf("oracle: idle stranger=%v, in-voice stranger=%v, ratio=%.1fx",
		idleT, voiceT, float64(voiceT)/float64(idleT))
	assert.Less(t, float64(voiceT), 3*float64(idleT),
		"REGRESSION: latency must not disclose whether a stranger is in Server Voice — the gate returns before any audience work")
}
