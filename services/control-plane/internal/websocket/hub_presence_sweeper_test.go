package websocket

import (
	"bytes"
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInboundApplicationFrameIsRecorded pins the evidence the presence sweeper
// runs on.
//
// It is deliberately APPLICATION-frame liveness, not socket liveness. A protocol
// pong is emitted by Chromium's network service without any renderer JavaScript,
// so a wedged renderer keeps ponging indefinitely; keying presence on that would
// report a dead client as online forever. A valid application frame is the one
// signal that proves the renderer's event loop is running and producing work.

// registerForLiveness makes userID look like a connected user.
// noteInboundApplicationFrame records only for a REGISTERED user, so a test that
// skips this is asserting the gate rather than the sweep.
func registerForLiveness(hub *Hub, userID uuid.UUID) {
	hub.mu.Lock()
	if hub.userClients[userID] == nil {
		hub.userClients[userID] = map[uuid.UUID]bool{}
	}
	hub.userClients[userID][uuid.New()] = true
	hub.mu.Unlock()
}

func TestInboundApplicationFrameIsRecorded(t *testing.T) {
	hub := NewHub(nil, setupHubTestRedis(t))
	userID := uuid.New()

	before := time.Now()
	registerForLiveness(hub, userID)
	hub.noteInboundApplicationFrame(userID)

	hub.mu.RLock()
	at, ok := hub.lastInboundAt[userID]
	hub.mu.RUnlock()

	require.True(t, ok, "a valid application frame must record liveness")
	assert.False(t, at.Before(before), "the recorded time must not predate the call")
}

// TestRemoveUserClientForgetsLivenessOnLastConnection exercises the REAL
// unregister path rather than the helper.
//
// Cleanup cannot go through a helper: removeUserClient runs under h.mu.Lock(),
// and any helper taking the same non-reentrant mutex would deadlock every
// disconnect. The cleanup is therefore an inline delete -- exactly the kind of
// edit a later refactor silently drops -- hence a test on the path. This is the
// ONLY guard against the map accumulating one entry per user ever seen for the
// life of the process.
func TestRemoveUserClientForgetsLivenessOnLastConnection(t *testing.T) {
	hub := NewHub(nil, setupHubTestRedis(t))
	userID := uuid.New()
	client := &Client{ID: uuid.New(), UserID: userID}

	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
	hub.noteInboundApplicationFrame(userID)

	hub.mu.Lock()
	isLast := hub.removeUserClient(client)
	hub.mu.Unlock()

	require.True(t, isLast, "this was the user's only connection")

	hub.mu.RLock()
	_, ok := hub.lastInboundAt[userID]
	hub.mu.RUnlock()
	assert.False(t, ok, "the last connection going away must clear liveness")
}

// TestRemoveUserClientKeepsLivenessWhileAnotherConnectionRemains is the
// other side of the same fence. Concord supports multi-device, so a user closing
// one client must not lose presence on the others -- an over-eager delete here
// would take a user offline every time they closed a second window.
func TestRemoveUserClientKeepsLivenessWhileAnotherConnectionRemains(t *testing.T) {
	hub := NewHub(nil, setupHubTestRedis(t))
	userID := uuid.New()
	closing := &Client{ID: uuid.New(), UserID: userID}
	staying := &Client{ID: uuid.New(), UserID: userID}

	hub.userClients[userID] = map[uuid.UUID]bool{closing.ID: true, staying.ID: true}
	hub.noteInboundApplicationFrame(userID)

	hub.mu.Lock()
	isLast := hub.removeUserClient(closing)
	hub.mu.Unlock()

	require.False(t, isLast, "another connection remains")

	hub.mu.RLock()
	_, ok := hub.lastInboundAt[userID]
	hub.mu.RUnlock()
	assert.True(t, ok, "liveness must survive while any connection remains")
}

// TestSweeperRenewsLiveUsers is the happy path: a user whose renderer produced a
// frame recently keeps their presence, with no client timer involved.
func TestSweeperRenewsLiveUsers(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	key := presence.StatusRedisKey(userID)
	ctx := context.Background()

	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 20*time.Second).Err())
	registerForLiveness(hub, userID)
	hub.noteInboundApplicationFrame(userID)

	hub.sweepPresenceLiveness(ctx)

	assert.Greater(t, redisClient.TTL(ctx, key).Val(), 100*time.Second,
		"a live user's presence must be renewed to the full TTL")
}

// TestSweeperNeverResurrectsALapsedKey is the load-bearing one.
//
// EXPIRE cannot create a key, and that is the entire reason the
// snapshot-vs-unregister race is benign: a user who disconnects between the
// snapshot and the pipeline gets an EXPIRE against a key their unregister already
// rewrote or deleted, which is a no-op. Relax this to SET and that race becomes a
// presence-RESURRECTION bug -- the hub would announce a departed user as online.
func TestSweeperNeverResurrectsALapsedKey(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	ctx := context.Background()

	// Liveness recorded, but no presence key exists — the shape left behind by a
	// user whose TTL already lapsed, or who unregistered mid-sweep.
	registerForLiveness(hub, userID)
	hub.noteInboundApplicationFrame(userID)

	hub.sweepPresenceLiveness(ctx)

	_, err := redisClient.Get(ctx, presence.StatusRedisKey(userID)).Result()
	assert.ErrorIs(t, err, redis.Nil, "the sweeper must never SET, only EXPIRE")
}

// TestSweeperIgnoresUsersOutsideTheLivenessWindow proves the window is actually
// consulted. Without this assertion a sweeper that renewed unconditionally would
// pass every other test here while keeping departed users online indefinitely.
func TestSweeperIgnoresUsersOutsideTheLivenessWindow(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	key := presence.StatusRedisKey(userID)
	ctx := context.Background()

	require.NoError(t, redisClient.Set(ctx, key, statusOnline, 20*time.Second).Err())
	hub.mu.Lock()
	hub.lastInboundAt[userID] = time.Now().Add(-2 * presenceLivenessWindow)
	hub.mu.Unlock()

	hub.sweepPresenceLiveness(ctx)

	assert.LessOrEqual(t, redisClient.TTL(ctx, key).Val(), 20*time.Second,
		"a user silent beyond the window must be allowed to lapse")
}

// TestSweeperRenewsAnOfflineMarkerToExactlyTheSharedTTL is the "obeyed" half of
// the presence.StatusTTL pair -- status_key_test.go pins the value, this pins
// that the sweeper uses IT and not a literal of its own.
//
// The key is deliberately seeded SHORTER than StatusTTL. Seeding it AT StatusTTL
// and sweeping in the same instant -- as this test originally did -- cannot tell
// a no-op apart from a full renewal, because both land on ~120s; it constrained
// only "the argument is <= StatusTTL". Seeding short makes both directions
// observable in one assertion pair: a sweeper carrying a larger TTL overshoots
// the upper bound, and a smaller one (or none at all) misses the lower.
func TestSweeperRenewsAnOfflineMarkerToExactlyTheSharedTTL(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	key := presence.StatusRedisKey(userID)
	ctx := context.Background()

	seeded := presence.StatusTTL / 3
	require.NoError(t, redisClient.Set(ctx, key, statusOffline, seeded).Err())
	registerForLiveness(hub, userID)
	hub.noteInboundApplicationFrame(userID)

	hub.sweepPresenceLiveness(ctx)

	ttl := redisClient.TTL(ctx, key).Val()
	assert.Greater(t, ttl, seeded, "the sweeper must renew the marker, not leave it")
	assert.LessOrEqual(t, ttl, presence.StatusTTL,
		"and must never carry it past the shared TTL")
}

// TestOrphanedFrameAfterUnregisterDoesNotResurrectLiveness pins the registration
// gate in noteInboundApplicationFrame.
//
// h.incoming is buffered (256) while h.unregister is not, and both are arms of one
// select in Run, so the unregister arm can win while frames for that client are
// still queued; forced disconnects reach handleUnregister directly while readPump
// is still enqueueing. Without the gate such a frame re-creates the entry AFTER
// removeUserClient deleted it -- and handleUnregister returns early at !exists, so
// nothing removes it a second time. The sweeper would then keep EXPIREing the key
// of a user with ZERO connections, which is only benign while transitionUserOffline
// succeeded; it logs and continues when its SET fails, leaving a VISIBLE status
// whose life the sweeper would extend.
func TestOrphanedFrameAfterUnregisterDoesNotResurrectLiveness(t *testing.T) {
	hub := NewHub(nil, setupHubTestRedis(t))
	userID := uuid.New()
	client := &Client{ID: uuid.New(), UserID: userID}

	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
	hub.noteInboundApplicationFrame(userID)

	hub.mu.Lock()
	require.True(t, hub.removeUserClient(client), "this was the user's only connection")
	hub.mu.Unlock()

	// The frame still sitting in h.incoming when the unregister arm won.
	hub.noteInboundApplicationFrame(userID)

	hub.mu.RLock()
	_, resurrected := hub.lastInboundAt[userID]
	hub.mu.RUnlock()
	assert.False(t, resurrected,
		"a frame from a departed client must not re-create its liveness record")
}

// TestSweeperGraceMatchesItsConstants pins the derived grace window.
//
// The binding refresh is the LAST sweep still inside W, so expiry falls in
// lastInbound + [W-S+T, W+T]. At W=T=120s, S=30s that is 210-240s against the old
// deterministic 120s. That widening IS the headroom this change buys against
// renderer throttling, so it is asserted rather than left emergent -- a later
// tuning of S or W silently moves a user-visible grace period.
func TestSweeperGraceMatchesItsConstants(t *testing.T) {
	assert.Equal(t, 120*time.Second, presenceLivenessWindow)
	assert.Equal(t, 30*time.Second, presenceSweepInterval)

	worstCaseGrace := presenceLivenessWindow + presence.StatusTTL
	bestCaseGrace := presenceLivenessWindow - presenceSweepInterval + presence.StatusTTL
	assert.Equal(t, 240*time.Second, worstCaseGrace)
	assert.Equal(t, 210*time.Second, bestCaseGrace)
}

// TestSweeperSkipsWhenRedisIsUnavailable covers the nil guard, which review asked
// for and which nothing exercised until this test existed. It matters more than a
// nil check usually would: the sweeper runs on its own goroutine with no recover,
// so a nil dereference here is not a failed sweep, it is a dead control plane.
func TestSweeperSkipsWhenRedisIsUnavailable(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()
	registerForLiveness(hub, userID)
	hub.noteInboundApplicationFrame(userID)

	assert.NotPanics(t, func() { hub.sweepPresenceLiveness(context.Background()) },
		"a nil Redis client must skip the sweep, not take the process down")
}

// TestSweeperReportsAFailedPipelineWithTheTrueCount covers the failure branch and
// pins the count in the log line.
//
// The count is load-bearing rather than cosmetic: review argued it should be an
// upper bound because a pipeline can fail partially. Not this pipeline -- EXPIRE
// has no per-key error path and the client is single-node, so a failure is
// connection-level and every queued command fails with it. Asserting "1 user(s)"
// against exactly one live user is what keeps that claim honest.
func TestSweeperReportsAFailedPipelineWithTheTrueCount(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	registerForLiveness(hub, userID)
	hub.noteInboundApplicationFrame(userID)

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	_ = redisClient.Close() // every queued EXPIRE now fails at the connection level

	assert.NotPanics(t, func() { hub.sweepPresenceLiveness(context.Background()) })
	assert.Contains(t, logged.String(), "presence liveness sweep failed for 1 user(s)")
	assert.NotContains(t, logged.String(), userID.String(),
		"a presence log line must never name a participant")
}

// TestSweeperLoopRenewsOnItsTickerAndExitsOnDone drives runPresenceLivenessSweeper
// itself, which nothing else does.
//
// Both arms are asserted because both can fail silently. A loop that never renews
// leaves every presence key to lapse -- the whole defect this change exists to fix
// -- and a loop that ignores h.done leaks a goroutine that keeps writing to Redis
// after the hub is gone. Neither shows up in a test that calls the sweep directly.
func TestSweeperLoopRenewsOnItsTickerAndExitsOnDone(t *testing.T) {
	restore := presenceSweepTickInterval
	presenceSweepTickInterval = 100 * time.Millisecond
	t.Cleanup(func() { presenceSweepTickInterval = restore })

	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	key := presence.StatusRedisKey(userID)
	ctx := context.Background()

	seeded := presence.StatusTTL / 3
	require.NoError(t, redisClient.Set(ctx, key, statusOnline, seeded).Err())
	registerForLiveness(hub, userID)
	hub.noteInboundApplicationFrame(userID)

	stopped := make(chan struct{})
	go func() { hub.runPresenceLivenessSweeper(); close(stopped) }()

	require.Eventually(t, func() bool {
		return redisClient.TTL(ctx, key).Val() > seeded
	}, 3*time.Second, 25*time.Millisecond,
		"the ticker arm must renew presence with no external call")

	// close h.done directly rather than calling Shutdown(): Shutdown waits on
	// h.stopped, which ONLY the Run loop closes, and this test deliberately runs
	// the sweeper goroutine alone with no Run loop to join. Calling it here hangs
	// until the package test timeout.
	hub.closeOnce.Do(func() { close(hub.done) })
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the sweeper goroutine must exit once h.done closes")
	}
}
