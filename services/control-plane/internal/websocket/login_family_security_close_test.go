package websocket

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This adapter instruments the real gorilla transport's underlying TCP Close.
// In particular, no fake Hub or close-control-frame substitutes for Close.
type familyCloseConn struct {
	net.Conn
	beforeClose  func()
	closeErr     error
	calls        atomic.Int32
	blockWrite   atomic.Bool
	writeEntered chan struct{}
	writeRelease chan struct{}
	writeOnce    sync.Once
}

func (c *familyCloseConn) Close() error {
	c.calls.Add(1)
	if c.beforeClose != nil {
		c.beforeClose()
	}
	err := c.Conn.Close()
	if c.closeErr != nil {
		return c.closeErr
	}
	return err
}
func (c *familyCloseConn) Write(p []byte) (int, error) {
	if c.blockWrite.Load() {
		c.writeOnce.Do(func() { close(c.writeEntered) })
		<-c.writeRelease
	}
	return c.Conn.Write(p)
}

type familyCloseListener struct {
	net.Listener
	accepted chan *familyCloseConn
}

func (l *familyCloseListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &familyCloseConn{Conn: c, writeEntered: make(chan struct{}), writeRelease: make(chan struct{})}
	l.accepted <- wrapped
	return wrapped, nil
}
func familyInstrumentedSocket(t *testing.T) (*ws.Conn, *ws.Conn, *familyCloseConn) {
	t.Helper()
	accepted := make(chan *familyCloseConn, 1)
	upgraded := make(chan *ws.Conn, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&ws.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			upgraded <- c
		}
	}))
	srv.Listener = &familyCloseListener{Listener: srv.Listener, accepted: accepted}
	srv.Start()
	t.Cleanup(srv.Close)
	peer, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	server := <-upgraded
	transport := <-accepted
	t.Cleanup(func() { _ = peer.Close(); _ = transport.Conn.Close() })
	return server, peer, transport
}
func familyAwait(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out: %s", label)
	}
}
func familyHubWithPublished(t *testing.T, clients ...*Client) *Hub {
	t.Helper()
	hub := NewHub(nil, setupHubTestRedis(t))
	for _, c := range clients {
		c.Hub = hub
		hub.clients[c.ID] = c
		if hub.userClients[c.UserID] == nil {
			hub.userClients[c.UserID] = map[uuid.UUID]bool{}
		}
		hub.userClients[c.UserID][c.ID] = true
	}
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	return hub
}
func familyPublishedClient(user, family uuid.UUID, conn *ws.Conn) *Client {
	return &Client{ID: uuid.New(), UserID: user, FamilyID: family, SessionID: uuid.NewString(), TokenExpiresAt: time.Now().Add(time.Hour), Conn: conn, Send: make(chan []byte, 8), Channels: make(map[uuid.UUID]bool)}
}

func TestLoginFamilySecurityReceiptFencesEverySiblingBeforeTransportClose(t *testing.T) {
	user, family := uuid.New(), uuid.New()
	var selected []*Client
	var transports []*familyCloseConn
	for i := 0; i < 3; i++ {
		conn, _, tr := familyInstrumentedSocket(t)
		selected = append(selected, familyPublishedClient(user, family, conn))
		transports = append(transports, tr)
	}
	freshConn, freshPeer, freshTransport := familyInstrumentedSocket(t)
	fresh := familyPublishedClient(user, uuid.New(), freshConn)
	foreignConn, _, foreignTransport := familyInstrumentedSocket(t)
	foreign := familyPublishedClient(uuid.New(), family, foreignConn)
	for _, tr := range transports {
		tr.beforeClose = func() {
			for _, c := range selected {
				assert.True(t, c.securityTerminal.Load(), "all sibling fences precede first Close")
			}
		}
	}
	// An unfinished bootstrap and queue owner must not hold the security leaf.
	for _, c := range selected {
		c.asyncWg.Add(1)
		c.sendMu.Lock()
	}
	hub := familyHubWithPublished(t, append(selected, fresh, foreign)...)
	t.Cleanup(func() {
		for _, c := range selected {
			c.sendMu.Unlock()
			c.asyncWg.Done()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, hub.DisconnectFamilyContext(ctx, user, family))
	for _, tr := range transports {
		assert.Positive(t, tr.calls.Load(), "success requires every transport Close")
	}
	assert.Zero(t, freshTransport.calls.Load())
	assert.Zero(t, foreignTransport.calls.Load())
	assert.False(t, fresh.securityTerminal.Load())
	assert.False(t, foreign.securityTerminal.Load())
	// A positive wire probe proves the fresh family transport really survives.
	require.NoError(t, freshConn.WriteMessage(ws.TextMessage, []byte("fresh-family-alive")))
	require.NoError(t, freshPeer.SetReadDeadline(time.Now().Add(time.Second)))
	_, payload, err := freshPeer.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "fresh-family-alive", string(payload))
	require.NoError(t, hub.DisconnectFamilyContext(ctx, user, family), "retry joins the same terminal clients")
	for _, c := range selected {
		assert.False(t, c.extendDeadline(time.Now().Add(2*time.Hour)), "terminal client cannot renew its deadline")
	}
}

func TestLoginFamilySecurityReceiptClosesDespiteBlockedWriter(t *testing.T) {
	conn, peer, tr := familyInstrumentedSocket(t)
	user, family := uuid.New(), uuid.New()
	c := familyPublishedClient(user, family, conn)
	// Keep cleanup pending independently of the writer, exactly as hung bootstrap.
	c.asyncWg.Add(1)
	hub := familyHubWithPublished(t, c)
	t.Cleanup(c.asyncWg.Done)
	tr.blockWrite.Store(true)
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); _ = conn.WriteMessage(ws.TextMessage, []byte("pending")) }()
	familyAwait(t, tr.writeEntered, "writer enters underlying transport")
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(tr.writeRelease) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, hub.DisconnectFamilyContext(ctx, user, family))
	require.Positive(t, tr.calls.Load())
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	require.Error(t, err, "actual peer sees closed transport while writer is still blocked")
	release()
	familyAwait(t, writerDone, "blocked writer exits")
}

func TestLoginFamilySecurityReceiptContinuesAfterCloseFailureAndMissingTransport(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "close error", true: "missing transport"}[missing], func(t *testing.T) {
			user, family := uuid.New(), uuid.New()
			conn, _, tr := familyInstrumentedSocket(t)
			bad := familyPublishedClient(user, family, conn)
			if missing {
				bad.Conn = nil
			} else {
				tr.closeErr = errors.New("test transport close failure")
			}
			siblingConn, _, siblingTransport := familyInstrumentedSocket(t)
			sibling := familyPublishedClient(user, family, siblingConn)
			bad.asyncWg.Add(1)
			sibling.asyncWg.Add(1)
			hub := familyHubWithPublished(t, bad, sibling)
			t.Cleanup(func() { bad.asyncWg.Done(); sibling.asyncWg.Done() })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.Error(t, hub.DisconnectFamilyContext(ctx, user, family))
			assert.True(t, bad.securityTerminal.Load())
			assert.True(t, sibling.securityTerminal.Load())
			assert.Positive(t, siblingTransport.calls.Load(), "failure must not skip sibling Close")
		})
	}
}

func TestLoginFamilySecurityReceiptCancellationAndShutdownAreIncomplete(t *testing.T) {
	user, family := uuid.New(), uuid.New()
	t.Run("canceled before acceptance", func(t *testing.T) {
		hub := NewHub(nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.Error(t, hub.DisconnectFamilyContext(ctx, user, family))
	})
	t.Run("shutdown without receipt", func(t *testing.T) {
		hub := NewHub(nil, nil)
		hub.closeOnce.Do(func() { close(hub.done) })
		require.Error(t, hub.DisconnectFamilyContext(context.Background(), user, family))
	})
	t.Run("cancellation during close does not interrupt siblings", func(t *testing.T) {
		firstConn, _, firstTransport := familyInstrumentedSocket(t)
		secondConn, _, secondTransport := familyInstrumentedSocket(t)
		first := familyPublishedClient(user, family, firstConn)
		second := familyPublishedClient(user, family, secondConn)
		entered, release := make(chan struct{}), make(chan struct{})
		var firstClose sync.Once
		callback := func() { firstClose.Do(func() { close(entered); <-release }) }
		firstTransport.beforeClose = callback
		secondTransport.beforeClose = callback
		first.asyncWg.Add(1)
		second.asyncWg.Add(1)
		hub := familyHubWithPublished(t, first, second)
		t.Cleanup(func() { first.asyncWg.Done(); second.asyncWg.Done() })
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- hub.DisconnectFamilyContext(ctx, user, family) }()
		familyAwait(t, entered, "Run accepted and started Close")
		assert.True(t, first.securityTerminal.Load())
		assert.True(t, second.securityTerminal.Load())
		cancel()
		select {
		case err := <-result:
			require.Error(t, err)
		case <-time.After(time.Second):
			t.Fatal("canceled receipt did not return")
		}
		unblock()
		retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
		defer retryCancel()
		require.NoError(t, hub.DisconnectFamilyContext(retryCtx, user, family))
		assert.Positive(t, firstTransport.calls.Load())
		assert.Positive(t, secondTransport.calls.Load())
	})
}

func TestLoginFamilySecurityCleanupRetainsReservationAndPreservesNewConnection(t *testing.T) {
	r := loginFamilyRig(t)
	hub := r.h.hub
	oldConn, _, _ := familyInstrumentedSocket(t)
	old := familyPublishedClient(r.user, r.family, oldConn)
	old.Hub = hub
	release, ok := hub.reserveConnection(r.user)
	require.True(t, ok)
	old.releaseAdmission = release
	old.admissionOwners.Store(1)
	require.NoError(t, hub.redis.Set(context.Background(), presence.StatusRedisKey(r.user), statusOnline, presence.StatusTTL).Err(), "published old connection already established online presence")
	old.asyncWg.Add(1)
	var joinOnce sync.Once
	finishBootstrap := func() { joinOnce.Do(old.asyncWg.Done) }
	hub.clients[old.ID] = old
	hub.userClients[r.user] = map[uuid.UUID]bool{old.ID: true}
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	t.Cleanup(finishBootstrap)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, hub.DisconnectFamilyContext(ctx, r.user, r.family))
	hub.admissionMu.Lock()
	retained := hub.admittedConnectionsByUser[r.user]
	hub.admissionMu.Unlock()
	require.Equal(t, 1, retained, "security receipt cannot release the unfinished bootstrap reservation")
	freshConn, freshPeer, _ := familyInstrumentedSocket(t)
	fresh := familyPublishedClient(r.user, uuid.New(), freshConn)
	fresh.Hub = hub
	freshRelease, ok := hub.reserveConnection(r.user)
	require.True(t, ok)
	fresh.releaseAdmission = freshRelease
	fresh.admissionOwners.Store(1)
	select {
	case hub.register <- fresh:
	case <-ctx.Done():
		t.Fatal("new connection registration stalled behind old bootstrap")
	}
	// A no-match command orders after Run finishes registration and before the
	// controlled old bootstrap join is released.
	require.NoError(t, hub.DisconnectFamilyContext(ctx, uuid.New(), uuid.New()))
	finishBootstrap()
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		_, oldExists := hub.clients[old.ID]
		newClient := hub.clients[fresh.ID]
		hub.mu.RUnlock()
		hub.admissionMu.Lock()
		count := hub.admittedConnectionsByUser[r.user]
		hub.admissionMu.Unlock()
		return !oldExists && newClient == fresh && count == 1
	}, 3*time.Second, time.Millisecond, "old cleanup removes exactly its map entry and releases exactly its reservation")
	assert.False(t, fresh.securityTerminal.Load())
	require.NoError(t, freshConn.WriteMessage(ws.TextMessage, []byte("fresh-after-old-cleanup")))
	require.NoError(t, freshPeer.SetReadDeadline(time.Now().Add(time.Second)))
	_, payload, err := freshPeer.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "fresh-after-old-cleanup", string(payload))
	status, err := hub.redis.Get(context.Background(), presence.StatusRedisKey(r.user)).Result()
	require.NoError(t, err)
	assert.Equal(t, statusOnline, status, "stale cleanup must not mark the remaining connection offline")
}
