package websocket

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

type forcedHiddenSuppressorSetter interface {
	SetRichPresenceHiddenSuppressor(func(uuid.UUID), ...func(uuid.UUID))
}

type localRichPresenceGate interface {
	richPresenceEmissionPermitted(uuid.UUID) bool
}

func hubPermitsRichPresence(hub *Hub, userID uuid.UUID) bool {
	gate, ok := any(hub).(localRichPresenceGate)
	if !ok {
		return true // pre-fix behavior: no Hub-local suppression gate
	}
	return gate.richPresenceEmissionPermitted(userID)
}

type blockingExecDriver struct {
	entered     chan struct{}
	release     chan struct{}
	resultErr   error
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func (d *blockingExecDriver) Open(string) (driver.Conn, error) {
	return &blockingExecConn{driver: d}, nil
}

func (d *blockingExecDriver) unblock() {
	d.releaseOnce.Do(func() { close(d.release) })
}

type blockingExecConn struct {
	driver *blockingExecDriver
}

func (c *blockingExecConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (c *blockingExecConn) Close() error {
	return nil
}

func (c *blockingExecConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *blockingExecConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	c.driver.enterOnce.Do(func() { close(c.driver.entered) })
	select {
	case <-c.driver.release:
		if c.driver.resultErr != nil {
			return nil, c.driver.resultErr
		}
		return driver.RowsAffected(1), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ driver.ExecerContext = (*blockingExecConn)(nil)

func openBlockingExecDB(t *testing.T, resultErr ...error) (*sql.DB, *blockingExecDriver) {
	t.Helper()
	script := &blockingExecDriver{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	if len(resultErr) > 0 {
		script.resultErr = resultErr[0]
	}
	driverName := "websocket-blocking-exec-" + uuid.NewString()
	sql.Register(driverName, script)
	db, err := sql.Open(driverName, "")
	require.NoError(t, err)
	t.Cleanup(func() {
		script.unblock()
		require.NoError(t, db.Close())
	})
	return db, script
}

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

// Regression #2444: a failed offline transition must upgrade an already-queued
// normal suppression so stale recovered Redis cannot make the clear fail open.
func TestHub_OfflineTransitionForcesAlreadyQueuedSuppression(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	require.NoError(t, rdb.Set(ctx, presence.StatusRedisKey(userID), statusOnline, 0).Err())

	db, err := sql.Open("postgres", "")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	hub := NewHub(db, rdb)
	clientID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{clientID: true}
	hub.presenceRecovery[userID] = presenceRecoveryState{status: statusOnline}

	var cleared []presence.Category
	recordClear := func(id uuid.UUID) {
		if id == userID {
			cleared = append(cleared, presence.CategoryServerVoice, presence.CategoryPrivateCall)
		}
	}
	normal := func(id uuid.UUID) {
		status, readErr := rdb.Get(ctx, presence.StatusRedisKey(id)).Result()
		if readErr != nil || presence.EmissionPermittedForStatus(status) {
			return
		}
		recordClear(id)
	}
	if setter, ok := any(hub).(forcedHiddenSuppressorSetter); ok {
		setter.SetRichPresenceHiddenSuppressor(normal, recordClear)
	} else {
		hub.SetRichPresenceHiddenSuppressor(normal)
	}

	hub.suppressorMu.Lock()
	hub.suppressorRunning = richPresenceSuppressorSlots
	hub.suppressorMu.Unlock()
	hub.spawnRichPresenceSuppression(userID)

	mr.Close()
	require.Error(t, db.PingContext(ctx))
	require.Error(t, rdb.Ping(ctx).Err())
	hub.transitionUserOffline(ctx, userID, false)

	require.NoError(t, mr.Restart())
	status, err := rdb.Get(ctx, presence.StatusRedisKey(userID)).Result()
	require.NoError(t, err)
	require.Equal(t, statusOnline, status, "failed offline write must leave the stale visible value")
	assert.False(t, hubPermitsRichPresence(hub, userID),
		"a connected sender without a confirmed visible state must stay suppressed after both offline writes fail")
	hub.presenceRecovery[userID] = presenceRecoveryState{status: statusOnline}
	assert.True(t, hubPermitsRichPresence(hub, userID),
		"a confirmed visible state for the connected sender may emit again")

	hub.suppressorMu.Lock()
	hub.suppressorRunning = 0
	hub.startPendingRichPresenceSuppressions()
	hub.suppressorMu.Unlock()
	hub.drainSuppressors()

	assert.Equal(t, []presence.Category{
		presence.CategoryServerVoice,
		presence.CategoryPrivateCall,
	}, cleared, "expected both categories, got %v", cleared)
}

func TestHub_QueuedForcedSuppressionCannotBeDowngraded(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	called := make(chan string, 1)
	hub.SetRichPresenceHiddenSuppressor(
		func(uuid.UUID) { called <- "normal" },
		func(uuid.UUID) { called <- "forced" },
	)
	hub.suppressorRunning = richPresenceSuppressorSlots

	hub.spawnRichPresenceSuppression(userID, true)
	hub.spawnRichPresenceSuppression(userID)
	hub.suppressorMu.Lock()
	hub.suppressorRunning = 0
	hub.startPendingRichPresenceSuppressions()
	hub.suppressorMu.Unlock()
	hub.drainSuppressors()

	assert.Equal(t, "forced", <-called)
}

// Regression #2444 / CWE-367: process-local authorization must close before
// either durable offline write can block or fail. Otherwise a concurrent
// refresh/bootstrap can publish from stale visible Redis during the I/O window.
func TestHub_HiddenTransitionRevokesLocalAuthorizationBeforeOfflineFenceIO(t *testing.T) {
	for _, test := range []struct {
		name         string
		transition   func(*Hub, uuid.UUID)
		wantRecovery bool
	}{
		{
			name: "offline transition",
			transition: func(hub *Hub, userID uuid.UUID) {
				hub.transitionUserOffline(context.Background(), userID, false)
			},
		},
		{
			name:         "fail-closed heartbeat",
			transition:   func(hub *Hub, userID uuid.UUID) { hub.failClosedPresenceHeartbeat(userID) },
			wantRecovery: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, script := openBlockingExecDB(t)
			hub := NewHub(db, nil)
			userID := uuid.New()
			hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}
			hub.presenceRecovery[userID] = presenceRecoveryState{status: statusOnline}
			require.True(t, hub.richPresenceEmissionPermitted(userID))

			transitionDone := make(chan struct{})
			go func() {
				test.transition(hub, userID)
				close(transitionDone)
			}()
			select {
			case <-script.entered:
			case <-time.After(time.Second):
				t.Fatal("offline fence writer did not reach the DB boundary")
			}

			readerDone := make(chan struct{})
			go func() {
				_ = hub.richPresenceEmissionPermitted(userID)
				close(readerDone)
			}()
			select {
			case <-readerDone:
			case <-time.After(time.Second):
				t.Fatal("offline fence I/O held the Hub mutex")
			}
			assert.False(t, hub.richPresenceEmissionPermitted(userID),
				"local authorization must be revoked before the fallible offline fence write")
			script.unblock()
			select {
			case <-transitionDone:
			case <-time.After(time.Second):
				t.Fatal("offline transition remained blocked after releasing the fence")
			}
			assert.False(t, hub.richPresenceEmissionPermitted(userID))

			if test.wantRecovery {
				hub.mu.RLock()
				recovery := hub.presenceRecovery[userID]
				hub.mu.RUnlock()
				assert.Equal(t, presenceRecoveryState{status: statusOnline}, recovery,
					"fail-closed heartbeat must retain the visible recovery candidate")
			}
		})
	}
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

func TestHub_FirstRegisterFenceClearFailureKeepsRecoveryPending(t *testing.T) {
	db, script := openBlockingExecDB(t, errors.New("fence clear failed"))
	script.unblock()
	hub := NewHub(db, setupHubTestRedis(t))
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.SetRichPresenceHiddenSuppressor(func(uuid.UUID) {})

	hub.handleRegister(client)
	hub.drainSuppressors()
	client.cancelBootstrap()
	client.asyncWg.Wait()

	hub.mu.RLock()
	recovery, known := hub.presenceRecovery[userID]
	hiddenStatus := hub.hiddenPresence[userID]
	hub.mu.RUnlock()
	require.True(t, known)
	assert.True(t, recovery.pending,
		"a failed fence clear must not expose a confirmed visible recovery candidate")
	assert.Equal(t, statusOffline, hiddenStatus)
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
