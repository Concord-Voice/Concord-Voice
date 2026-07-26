package websocket

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// drainSuppressors waits for every dispatched hidden-sender suppression. The
// dispatch is deliberately off the Run goroutine, so tests must join it.
func (h *Hub) drainSuppressors() {
	h.suppressorWg.Wait()
}

func setStatusMessage(userID uuid.UUID, status string) IncomingMessage {
	return IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data:   map[string]interface{}{keyStatus: status},
	}
}

// hubWithSuppressorCounter returns a Redis-backed hub whose suppressor just
// counts invocations, plus the registered user's id.
func hubWithSuppressorCounter(t *testing.T) (*Hub, uuid.UUID, *int32) {
	t.Helper()
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Username: "hiddenuser",
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	var calls int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) { atomic.AddInt32(&calls, 1) })
	return hub, userID, &calls
}

// E1: only the transition edge fires. A repeated set_status to invisible is a
// LEVEL, and the invisible heartbeat re-asserts that level on every beat -- if
// levels fired, an invisible user sitting in a voice channel would dispatch a
// suppression forever.
func TestHub_InvisibleEdgeSpawnsSuppressorOnce(t *testing.T) {
	hub, userID, calls := hubWithSuppressorCounter(t)

	hub.handleSetStatus(setStatusMessage(userID, statusInvisible))
	hub.handleSetStatus(setStatusMessage(userID, statusInvisible)) // repeat: a level

	hub.drainSuppressors()
	assert.EqualValues(t, 1, atomic.LoadInt32(calls),
		"only the transition edge fires; a repeated set_status is a level")
}

func TestHub_ReturnVisibleDoesNotSpawnSuppressor(t *testing.T) {
	hub, userID, calls := hubWithSuppressorCounter(t)

	hub.handleSetStatus(setStatusMessage(userID, statusInvisible))
	hub.drainSuppressors()
	require.EqualValues(t, 1, atomic.LoadInt32(calls))

	hub.handleSetStatus(setStatusMessage(userID, statusOnline))
	hub.drainSuppressors()

	assert.EqualValues(t, 1, atomic.LoadInt32(calls),
		"returning to visible must not suppress; state reappears via a fresh rebuild")
}

// The suppressor runs only AFTER the Redis write succeeds, so it cannot observe
// the pre-transition status.
func TestHub_InvisibleEdgePersistsBeforeSuppressing(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	hub := NewHub(nil, redisClient)
	userID := uuid.New()
	client := &Client{
		ID: uuid.New(), UserID: userID, Username: "ordered",
		Send: make(chan []byte, 10), Hub: hub, Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	observed := make(chan string, 1)
	hub.SetRichPresenceHiddenSuppressor(func(id uuid.UUID) {
		status, err := redisClient.Get(t.Context(), presence.StatusRedisKey(id)).Result()
		if err != nil {
			observed <- "ERR:" + err.Error()
			return
		}
		observed <- status
	})

	hub.handleSetStatus(setStatusMessage(userID, statusInvisible))
	hub.drainSuppressors()

	require.Len(t, observed, 1)
	assert.Equal(t, statusInvisible, <-observed,
		"the suppressor must read the post-transition status, never a stale one")
}

func TestHub_NilSuppressorIsSkipped(t *testing.T) {
	hub, userID, _ := hubWithSuppressorCounter(t)
	hub.SetRichPresenceHiddenSuppressor(nil)

	assert.NotPanics(t, func() {
		hub.handleSetStatus(setStatusMessage(userID, statusInvisible))
	})
}

// E4: failClosedPresenceHeartbeat is already edge-guarded by its own
// already-hidden early return, so repeated fail-closed beats fire once.
func TestHub_FailClosedHeartbeatSpawnsSuppressorOnce(t *testing.T) {
	hub := newMinimalHub()
	var calls int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) { atomic.AddInt32(&calls, 1) })

	userID := uuid.New()
	for i := 0; i < 5; i++ {
		hub.failClosedPresenceHeartbeat(userID)
	}

	hub.drainSuppressors()
	assert.EqualValues(t, 1, atomic.LoadInt32(&calls),
		"the fail-closed heartbeat is edge-guarded; repeated beats are levels")
}

func TestHub_FailClosedHeartbeatPersistsOfflineFence(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, setupHubTestRedis(t))
	userID := presenceTestUser(t, db)

	hub.failClosedPresenceHeartbeat(userID)

	var fenced bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM presence_offline_fences WHERE user_id = $1)`, userID,
	).Scan(&fenced))
	assert.True(t, fenced)
}

// Saturated fixed concurrency queues each distinct edge without blocking the Run
// goroutine, and coalesces repeated edges for the same sender.
func TestHub_SaturatedSuppressorQueuesEdgesWithoutBlocking(t *testing.T) {
	hub := newMinimalHub()
	release := make(chan struct{})
	started := make(chan struct{}, richPresenceSuppressorSlots)
	var calls int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) {
		started <- struct{}{}
		<-release
		atomic.AddInt32(&calls, 1)
	})
	for i := 0; i < richPresenceSuppressorSlots; i++ {
		hub.spawnRichPresenceSuppression(uuid.New())
	}
	for i := 0; i < richPresenceSuppressorSlots; i++ {
		select {
		case <-started:
		case <-t.Context().Done():
			t.Fatal("initial suppressors did not start")
		}
	}

	done := make(chan struct{})
	queuedUserID := uuid.New()
	go func() {
		hub.spawnRichPresenceSuppression(queuedUserID)
		hub.spawnRichPresenceSuppression(queuedUserID)
		close(done)
	}()

	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("spawnRichPresenceSuppression blocked the caller when saturated")
	}
	assert.Zero(t, atomic.LoadInt32(&calls))
	close(release)
	hub.drainSuppressors()
	assert.EqualValues(t, richPresenceSuppressorSlots+1, atomic.LoadInt32(&calls),
		"the queued sender runs once after capacity returns")
}

// E3: the offline edge. The last connection going away writes a durable offline
// state before the suppressor runs.
// This one hook also covers handleDisconnectUser and handleDisconnectSession,
// which converge on this block.
func TestHub_LastConnectionUnregisterSpawnsSuppressor(t *testing.T) {
	hub, _, calls := hubWithSuppressorCounter(t)
	client := hub.clients[firstClientID(t, hub)]

	hub.handleUnregister(client)
	hub.drainSuppressors()

	assert.EqualValues(t, 1, atomic.LoadInt32(calls),
		"the last connection going away is the offline edge")
}

// Not the last connection is not an edge: the user is still online.
func TestHub_NonLastConnectionUnregisterDoesNotSpawnSuppressor(t *testing.T) {
	hub, userID, calls := hubWithSuppressorCounter(t)
	first := hub.clients[firstClientID(t, hub)]

	second := &Client{
		ID: uuid.New(), UserID: userID, Username: "hiddenuser",
		Send: make(chan []byte, 10), Hub: hub, Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[second.ID] = second
	hub.userClients[userID][second.ID] = true

	hub.handleUnregister(first)
	hub.drainSuppressors()

	assert.Zero(t, atomic.LoadInt32(calls),
		"a remaining connection means the user is still online; not an edge")
}

// E5: a first registration that cannot persist presence fails closed to offline,
// so activity from a prior session must not keep publishing.
func TestHub_FailClosedRegisterSpawnsSuppressor(t *testing.T) {
	// A dead Redis client makes the presence Set fail, taking the fail-closed
	// branch. A real DB is still required: the register path queries it to
	// broadcast presence.
	db := setupHubTestDB(t)
	hub := NewHub(db, newUnreachableRedisClient(t))
	var calls int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) { atomic.AddInt32(&calls, 1) })

	userID := presenceTestUser(t, db)
	client := &Client{
		ID: uuid.New(), UserID: userID, Username: "failclosed",
		Send: make(chan []byte, 16), Hub: hub, Channels: make(map[uuid.UUID]bool),
	}

	hub.handleRegister(client)
	hub.drainSuppressors()

	assert.EqualValues(t, 1, atomic.LoadInt32(&calls),
		"a fail-closed first registration is the offline edge")
	var fenced bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM presence_offline_fences WHERE user_id = $1)`, userID,
	).Scan(&fenced))
	assert.True(t, fenced)
}

func TestHub_VisibleStatusClearsOfflineFence(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	hub := NewHub(db, rdb)
	userID := presenceTestUser(t, db)
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT INTO presence_offline_fences (user_id) VALUES ($1)`, userID,
	)
	require.NoError(t, err)

	hub.handleSetStatus(setStatusMessage(userID, statusOnline))

	var fenced bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM presence_offline_fences WHERE user_id = $1)`, userID,
	).Scan(&fenced))
	assert.False(t, fenced)
}

// U19: shutdown joins in-flight suppressions, so none outlives the hub. Run under
// -race, where an unjoined goroutine touching test state would be reported.
func TestHub_ShutdownJoinsInFlightSuppressors(t *testing.T) {
	hub := newMinimalHub()
	release := make(chan struct{})
	var completed int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) {
		<-release
		atomic.AddInt32(&completed, 1)
	})

	hub.spawnRichPresenceSuppression(uuid.New())

	joined := make(chan struct{})
	go func() {
		hub.shutdownClients()
		close(joined)
	}()

	select {
	case <-joined:
		t.Fatal("shutdown returned before the in-flight suppression finished")
	default:
	}

	close(release)
	select {
	case <-joined:
	case <-t.Context().Done():
		t.Fatal("shutdown did not join the suppression")
	}
	assert.EqualValues(t, 1, atomic.LoadInt32(&completed))
}

func TestHub_ShutdownDrainsQueuedSuppressors(t *testing.T) {
	hub := newMinimalHub()
	hub.done = make(chan struct{})
	releaseInitial := make(chan struct{})
	releaseQueued := make(chan struct{})
	started := make(chan struct{}, richPresenceSuppressorSlots)
	queuedStarted := make(chan struct{}, 1)
	var calls int32
	var completed int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) {
		call := atomic.AddInt32(&calls, 1)
		if call <= richPresenceSuppressorSlots {
			started <- struct{}{}
			<-releaseInitial
		} else {
			queuedStarted <- struct{}{}
			<-releaseQueued
		}
		atomic.AddInt32(&completed, 1)
	})
	for i := 0; i < richPresenceSuppressorSlots; i++ {
		hub.spawnRichPresenceSuppression(uuid.New())
	}
	for i := 0; i < richPresenceSuppressorSlots; i++ {
		select {
		case <-started:
		case <-t.Context().Done():
			t.Fatal("initial suppressors did not start")
		}
	}
	hub.spawnRichPresenceSuppression(uuid.New())
	close(hub.done)

	joined := make(chan struct{})
	go func() {
		hub.shutdownClients()
		close(joined)
	}()
	close(releaseInitial)
	select {
	case <-queuedStarted:
	case <-t.Context().Done():
		t.Fatal("queued suppression did not start after capacity returned")
	}
	select {
	case <-joined:
		t.Fatal("shutdown returned before the queued suppression finished")
	default:
	}
	close(releaseQueued)
	select {
	case <-joined:
	case <-t.Context().Done():
		t.Fatal("shutdown did not drain the queued suppression")
	}
	assert.EqualValues(t, richPresenceSuppressorSlots+1, atomic.LoadInt32(&completed))
}

func TestHub_ShutdownTransitionsUsersOfflineAndSuppresses(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	userID := presenceTestUser(t, db)
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
	require.NoError(t, redisClient.Set(context.Background(), presence.StatusRedisKey(userID), statusOnline, 0).Err())

	var suppressions int32
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) { atomic.AddInt32(&suppressions, 1) })
	hub.shutdownClients()

	status, err := redisClient.Get(context.Background(), presence.StatusRedisKey(userID)).Result()
	require.NoError(t, err)
	assert.Equal(t, statusOffline, status)
	var fenced bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM presence_offline_fences WHERE user_id = $1)`, userID,
	).Scan(&fenced))
	assert.True(t, fenced)
	assert.EqualValues(t, 1, atomic.LoadInt32(&suppressions))
}

func firstClientID(t *testing.T, hub *Hub) uuid.UUID {
	t.Helper()
	for id := range hub.clients {
		return id
	}
	t.Fatal("hub has no registered client")
	return uuid.Nil
}
