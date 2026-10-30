package channels_test

// #3459 AC2/AC6 at the channel route: turning include_pinned on deletes the
// pinned messages already past their stamp BEFORE the 200, through the real
// purge engine, writes one truthful expiry audit row and announces exactly one
// server_purged; the pins-only change bumps the revision and writes the
// 000137 system row with the pin fields.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type countingPurgeNotifier struct {
	mu     sync.Mutex
	server []string
	dm     []string
}

func (n *countingPurgeNotifier) EmitServerPurged(_ context.Context, serverID string, _ time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.server = append(n.server, serverID)
}

func (n *countingPurgeNotifier) EmitDMPurged(_ context.Context, conversationID string, _ time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dm = append(n.dm, conversationID)
}

func insertPinnedChannelMessage(t *testing.T, ts *testhelpers.TestServer, channelID, authorID string, expires, held *time.Time) string {
	t.Helper()
	var id string
	require.NoError(t, ts.DB.QueryRow(`
INSERT INTO messages (channel_id, user_id, content, key_version, created_at, pinned_at, pinned_by, expires_at, held_expires_at)
VALUES ($1, $2, 'ciphertext', 1, NOW() - interval '25 hours', CASE WHEN $5 THEN NOW() - interval '25 hours' END,
        CASE WHEN $5 THEN $2::uuid END, $3, $4)
RETURNING id`, channelID, authorID, expires, held, held != nil || expires == nil).Scan(&id))
	return id
}

func channelMessageExists(t *testing.T, ts *testhelpers.TestServer, id string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`, id).Scan(&exists))
	return exists
}

// Mutant killed: the expirer call removed from UpdateExpiration (the due pin
// survives the 200 and nothing is announced).
func TestChannelExpiration_TogglingPinnedOnExpiresDuePinsBeforeTheResponse(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	log := logger.NewWithWriter(&bytes.Buffer{})
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	handler := channels.NewHandler(ts.DB, log, ts.Hub, resolver, ts.Redis)
	handler.SetMFAVerifier(&testhelpers.FakeMFAVerifier{})
	engine := purge.NewEngine(ts.DB, log, purge.NewReaper(ts.DB, log, nil), purge.MaxExpiryCandidateIDs)
	notifier := &countingPurgeNotifier{}
	expirer, err := expiration.NewContextExpirer(context.Background(), engine, notifier, log)
	require.NoError(t, err)
	handler.SetPinnedExpirer(expirer)
	require.True(t, handler.HasPinnedExpiryRunner())

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", f.mod.ID)
		testhelpers.AttachUserIdentity(t, c, f.mod.ID)
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"sid": uuid.NewString()})
		c.Next()
	})
	router.PATCH("/api/v1/channels/:id/expiration", handler.UpdateExpiration)
	patch := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/channels/"+f.channelID+"/expiration", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	w := patch(pinnedSetBody(86400, nil, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var keep expiration.Policy
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &keep))
	require.False(t, keep.IncludePinned)

	past, future, stampPast := time.Now().Add(-time.Hour), time.Now().Add(time.Hour), time.Now().Add(-2*time.Hour)
	duePin := insertPinnedChannelMessage(t, ts, f.channelID, f.owner.ID, nil, &past)
	futurePin := insertPinnedChannelMessage(t, ts, f.channelID, f.owner.ID, nil, &future)
	dueUnpinned := insertPinnedChannelMessage(t, ts, f.channelID, f.owner.ID, &stampPast, nil)

	w = patch(pinnedSetBody(86400, boolRef(true), ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var on expiration.Policy
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &on))

	// AC2: deleted before the 200, nothing beyond the due pins, one audit row
	// recording that pinned messages were in scope, one announcement.
	assert.False(t, channelMessageExists(t, ts, duePin), "the due pin is gone when the PATCH answers")
	assert.True(t, channelMessageExists(t, ts, futurePin))
	assert.True(t, channelMessageExists(t, ts, dueUnpinned), "the expirer never widens past its candidates")
	var audits, truthful int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*), count(*) FILTER (WHERE include_pinned)
	   FROM message_purges WHERE context_id = $1 AND reason = 'expiry'`, f.channelID).Scan(&audits, &truthful))
	assert.Equal(t, 1, audits)
	assert.Equal(t, 1, truthful)
	assert.Equal(t, []string{f.serverID}, notifier.server, "exactly one server_purged")
	assert.Empty(t, notifier.dm)

	// AC6: the pins-only change bumped the revision and wrote the system row
	// with the pin fields, in the same transaction as the policy.
	assert.True(t, on.IncludePinned)
	assert.Equal(t, keep.Revision+1, on.Revision)
	var payload json.RawMessage
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_event_payload FROM messages
	   WHERE channel_id = $1 AND type = 'expiration_event' ORDER BY created_at DESC LIMIT 1`, f.channelID).Scan(&payload))
	var event map[string]any
	require.NoError(t, json.Unmarshal(payload, &event))
	assert.Equal(t, "changed", event["kind"])
	assert.Equal(t, true, event["include_pinned"])
	assert.Equal(t, false, event["previous_include_pinned"])
	assert.EqualValues(t, 86400, event["previous_window_seconds"])
}

type deadlineRecordingRunner struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *deadlineRecordingRunner) RunExpiryBatch(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	deadline, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, deadline)
	return purge.Result{DeletedCount: len(plan.CandidateIDs)}, nil
}

// The pin deletion after commit runs inside the PATCH's response budget, not
// on a fresh 10 seconds of its own (#3459 Codex review). With the budget cut to
// 3 s the run must end about 2 s after the request starts; a handler that did
// not hand its deadline over would give the run its own 10 s.
//
// Mutant killed: answerCtx replaced by c.Request.Context() in UpdateExpiration.
func TestChannelExpiration_PinDeletionRunsInsideTheResponseBudget(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	log := logger.NewWithWriter(&bytes.Buffer{})
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	handler := channels.NewHandler(ts.DB, log, ts.Hub, resolver, ts.Redis)
	handler.SetMFAVerifier(&testhelpers.FakeMFAVerifier{})
	runner := &deadlineRecordingRunner{}
	expirer, err := expiration.NewContextExpirer(context.Background(), runner, &countingPurgeNotifier{}, log)
	require.NoError(t, err)
	handler.SetPinnedExpirer(expirer)
	handler.SetResponseBudgetForTest(3 * time.Second)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", f.mod.ID)
		testhelpers.AttachUserIdentity(t, c, f.mod.ID)
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"sid": uuid.NewString()})
		c.Next()
	})
	router.PATCH("/api/v1/channels/:id/expiration", handler.UpdateExpiration)
	patch := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/channels/"+f.channelID+"/expiration", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	require.Equal(t, http.StatusOK, patch(pinnedSetBody(86400, nil, "")).Code)
	past := time.Now().Add(-time.Hour)
	insertPinnedChannelMessage(t, ts, f.channelID, f.owner.ID, nil, &past)

	sent := time.Now()
	w := patch(pinnedSetBody(86400, boolRef(true), ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, runner.deadlines, 1, "the due pin reached the expirer")
	assert.True(t, runner.deadlines[0].Before(sent.Add(3*time.Second)),
		"the run ends inside the 3 s budget (got %s after the request)", runner.deadlines[0].Sub(sent))
}

// heldExpiryRunner stands in for the purge engine and holds its first batch
// until Release, so a test can look at everything the handler did BEFORE the
// deletion while the deletion is still running.
type heldExpiryRunner struct {
	entered chan struct{}
	release chan struct{}
	enter   sync.Once
	let     sync.Once
}

func newHeldExpiryRunner() *heldExpiryRunner {
	return &heldExpiryRunner{entered: make(chan struct{}), release: make(chan struct{})}
}

func (r *heldExpiryRunner) Release() { r.let.Do(func() { close(r.release) }) }

func (r *heldExpiryRunner) RunExpiryBatch(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
	r.enter.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return purge.Result{DeletedCount: len(plan.CandidateIDs)}, nil
	case <-ctx.Done():
		return purge.Result{}, ctx.Err()
	}
}

// #3459 §8 orders what the handler ISSUES: announce the policy change, then
// delete the due pins, so a slow best-effort deletion (up to 10 s) cannot hold
// the announcement back. The frame test does not assert which frame a client
// receives first, because the hub delivers them on separate lanes, so this is
// where the issue order is pinned: with the deletion held in flight, the policy
// frame has already reached the observer.
//
// Mutant killed: UpdateExpiration broadcasting the policy after
// expireDuePinnedMessages instead of before it.
func TestChannelExpiration_PolicyIsAnnouncedWhileTheDueDeletionIsStillHeld(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	log := logger.NewWithWriter(&bytes.Buffer{})
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	handler := channels.NewHandler(ts.DB, log, ts.Hub, resolver, ts.Redis)
	handler.SetMFAVerifier(&testhelpers.FakeMFAVerifier{})
	runner := newHeldExpiryRunner()
	expirer, err := expiration.NewContextExpirer(context.Background(), runner, &countingPurgeNotifier{}, log)
	require.NoError(t, err)
	handler.SetPinnedExpirer(expirer)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", f.mod.ID)
		testhelpers.AttachUserIdentity(t, c, f.mod.ID)
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"sid": uuid.NewString()})
		c.Next()
	})
	router.PATCH("/api/v1/channels/:id/expiration", handler.UpdateExpiration)
	patch := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/channels/"+f.channelID+"/expiration", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	tap := dialChannelFrameObserver(t, ts, f.owner, f.serverID, f.channelID)
	require.Equal(t, http.StatusOK, patch(pinnedSetBody(86400, nil, "")).Code)
	require.Eventually(t, func() bool { return len(tap.ofTypes("expiration_event")) == 1 }, 3*time.Second, 20*time.Millisecond)
	tap.reset()

	past := time.Now().Add(-time.Hour)
	insertPinnedChannelMessage(t, ts, f.channelID, f.owner.ID, nil, &past)

	var toggled *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() {
		defer close(done)
		toggled = patch(pinnedSetBody(86400, boolRef(true), ""))
	}()
	t.Cleanup(func() {
		runner.Release()
		<-done
	})

	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the due pin never reached the expirer, so there is no held deletion to look past")
	}
	require.Eventually(t, func() bool { return len(tap.ofTypes("expiration_event")) == 1 }, 5*time.Second, 20*time.Millisecond,
		"the policy frame must reach the observer while the deletion is still running (§8: announce first)")

	runner.Release()
	<-done
	assert.Equal(t, http.StatusOK, toggled.Code, toggled.Body.String())
}
