package websocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

var errDeadlineInjection = errors.New("injected deadline failure")
var errCloseInjection = errors.New("injected close-frame failure")

type stalledFenceCheckHook struct {
	key     string
	checks  atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (h *stalledFenceCheckHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *stalledFenceCheckHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && cmd.Args()[1] == h.key && h.checks.Add(1) == 2 {
			close(h.entered)
			<-h.release
		}
		return next(ctx, cmd)
	}
}

func (h *stalledFenceCheckHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type deadlineFailureConn struct {
	net.Conn
	failReadDeadline atomic.Bool
	failWrites       atomic.Bool
	closeAttempts    atomic.Int32
}

func (conn *deadlineFailureConn) Close() error {
	conn.closeAttempts.Add(1)
	return conn.Conn.Close()
}

func (conn *deadlineFailureConn) SetReadDeadline(deadline time.Time) error {
	if conn.failReadDeadline.Load() {
		return errDeadlineInjection
	}
	return conn.Conn.SetReadDeadline(deadline)
}

func (conn *deadlineFailureConn) Write(data []byte) (int, error) {
	if conn.failWrites.Load() {
		return 0, errCloseInjection
	}
	return conn.Conn.Write(data)
}

type deadlineFailureListener struct {
	net.Listener
	accepted chan *deadlineFailureConn
}

func (listener *deadlineFailureListener) Accept() (net.Conn, error) {
	conn, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &deadlineFailureConn{Conn: conn}
	listener.accepted <- wrapped
	return wrapped, nil
}

func startDeadlineFailureSocket(t *testing.T) (*Client, *deadlineFailureConn, *ws.Conn, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	wrappedListener := &deadlineFailureListener{Listener: listener, accepted: make(chan *deadlineFailureConn, 1)}
	startReadPump := make(chan struct{})
	ready := make(chan *Client, 1)
	hub := NewHub(nil, nil)
	go func() { <-hub.unregister }()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, upgradeErr := (&ws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if upgradeErr != nil {
			return
		}
		client := &Client{
			ID: uuid.New(), UserID: uuid.New(), Conn: conn, Hub: hub, Send: make(chan []byte, 1),
			TokenExpiresAt: time.Now().Add(time.Minute),
		}
		ready <- client
		<-startReadPump
		client.readPump()
	}))
	server.Listener = wrappedListener
	server.Start()
	t.Cleanup(func() {
		server.Close()
	})

	peer, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	client := <-ready
	wrapped := <-wrappedListener.accepted
	return client, wrapped, peer, func() { close(startReadPump) }
}

func TestWebSocketClosesAtAccessTokenDeadline(t *testing.T) {
	for _, rail := range []string{"jwt", "ticket"} {
		t.Run(rail, func(t *testing.T) {
			db := setupHubTestDB(t)
			rc := setupHubTestRedis(t)
			hub := NewHub(db, rc)
			h := NewHandler(hub, db, rc, testJWTSecret, nil, nil, nil)
			router := gin.New()
			router.GET("/ws", h.HandleWebSocket)
			srv := httptest.NewServer(router)
			t.Cleanup(srv.Close)
			userID := uuid.NewString()
			expires := time.Now().Add(3 * time.Second).Truncate(time.Second)
			query := ""
			if rail == "ticket" {
				require.NoError(t, rc.Set(context.Background(), wsTicketKeyPfx+"deadline",
					fmt.Sprintf("%s:::%d", userID, expires.Unix()), 30*time.Second).Err())
				query = "?ticket=deadline"
			} else {
				token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
					"iss": middleware.AccessTokenIssuer, "user_id": userID, "exp": expires.Unix(),
				}).SignedString([]byte(testJWTSecret))
				require.NoError(t, err)
				query = "?token=" + token
			}
			closed := make(chan struct{})
			go func() {
				client := <-hub.register
				client.Send <- []byte(`{"type":"control"}`)
				<-hub.unregister
				close(client.Send)
				close(closed)
			}()
			peer, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws"+query, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = peer.Close() })
			require.NoError(t, peer.SetReadDeadline(expires.Add(time.Second)))
			_, frame, err := peer.ReadMessage()
			require.NoError(t, err)
			require.JSONEq(t, `{"type":"control"}`, string(frame))
			_, _, err = peer.ReadMessage()
			require.True(t, ws.IsCloseError(err, ws.ClosePolicyViolation), "deadline must close the socket: %v", err)
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("expired client was not unregistered")
			}
		})
	}
}

func TestExpiredTokenUnregistersWhilePostRegistrationFenceStalls(t *testing.T) {
	db := setupHubTestDB(t)
	rc, fence := newTestFence(t)
	userID := presenceTestUser(t, db)
	require.NoError(t, rc.Set(context.Background(), credepoch.Key(userID.String()), "none", time.Minute).Err())
	hook := &stalledFenceCheckHook{
		key: credepoch.Key(userID.String()), entered: make(chan struct{}), release: make(chan struct{}),
	}
	rc.AddHook(hook)
	releaseFence := sync.OnceFunc(func() { close(hook.release) })
	defer releaseFence()
	hub := NewHub(db, rc)
	go hub.Run()
	handler := NewHandler(hub, db, rc, testJWTSecret, []string{"*"}, fence, nil)
	srv := setupWSTestServer(t, handler, hub)
	deadline := time.Now().Add(4 * time.Second).Truncate(time.Second)
	require.NoError(t, rc.Set(context.Background(), wsTicketKeyPfx+"stalled-fence",
		fmt.Sprintf("%s:::%d", userID, deadline.Unix()), 30*time.Second).Err())
	peer, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+wsTicketPath+"stalled-fence", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	select {
	case <-hook.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("post-registration credential check did not stall")
	}
	hub.mu.RLock()
	registered := len(hub.clients)
	hub.mu.RUnlock()
	require.Equal(t, 1, registered)
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return len(hub.clients) == 0 && len(hub.userClients) == 0
	}, 6*time.Second, 10*time.Millisecond, "deadline must remove the registered socket before the fence returns")
	releaseFence()
	require.Eventually(t, func() bool {
		hub.admissionMu.Lock()
		defer hub.admissionMu.Unlock()
		return hub.admittedConnections == 0
	}, 2*time.Second, 10*time.Millisecond)
}

func TestWebSocketJWTRequiresSignedDeadline(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	claims := jwt.MapClaims{"iss": middleware.AccessTokenIssuer, "user_id": uuid.NewString()}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodGet, "/ws?token="+token, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = request

	_, _, err = h.authenticateWebSocket(c)
	require.ErrorContains(t, err, "invalid token deadline")
}

func TestReadPumpDeadlineFailureClosesTransportBeforeHandlingTraffic(t *testing.T) {
	client, transport, peer, startReadPump := startDeadlineFailureSocket(t)
	transport.failReadDeadline.Store(true)
	startReadPump()

	_ = peer.WriteMessage(ws.TextMessage, []byte(`{"type":"connection_ready_probe"}`))
	select {
	case message := <-client.Hub.incoming:
		t.Fatalf("deadline setup failure admitted traffic: %q", message.Type)
	case <-time.After(250 * time.Millisecond):
	}
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(250*time.Millisecond)))
	_, _, err := peer.ReadMessage()
	require.Error(t, err, "a read-deadline setup failure must close the authenticated transport")
}

func TestExpiredTokenClosesTransportWhenCloseFrameWriteFails(t *testing.T) {
	client, transport, peer, startReadPump := startDeadlineFailureSocket(t)
	startReadPump()
	transport.failWrites.Store(true)

	client.closeExpiredToken()

	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	require.Error(t, err, "the transport must close even when its policy close frame cannot be written")
}

func TestExpiredTokenCloseIsIdempotent(t *testing.T) {
	client, transport, _, _ := startDeadlineFailureSocket(t)
	client.closeExpiredToken()
	client.closeExpiredToken()
	require.EqualValues(t, 1, transport.closeAttempts.Load())
}

func TestHubRejectsExpiredClientBeforeRegistration(t *testing.T) {
	client, transport, _, _ := startDeadlineFailureSocket(t)
	client.TokenExpiresAt = time.Now().Add(-time.Second)
	release, admitted := client.Hub.reserveConnection(client.UserID)
	require.True(t, admitted)
	client.releaseAdmission = release
	client.admissionOwners.Store(1)
	client.Hub.handleRegister(client)
	require.Empty(t, client.Hub.clients)
	require.Empty(t, client.Hub.userClients)
	require.Zero(t, client.Hub.admittedConnections)
	require.EqualValues(t, 1, transport.closeAttempts.Load())
}

func TestHubRejectsFramesQueuedPastAccessTokenDeadline(t *testing.T) {
	hub := NewHub(nil, nil)
	client := &Client{ID: uuid.New(), UserID: uuid.New(), Send: make(chan []byte, 1)}
	hub.clients[client.ID] = client
	msg := IncomingMessage{Type: "connection_ready_probe", ClientID: client.ID, UserID: client.UserID,
		TokenExpiresAt: time.Now().Add(-time.Second)}
	hub.handleIncoming(msg)
	require.Empty(t, client.Send)
	msg.TokenExpiresAt = time.Now().Add(time.Minute)
	hub.handleIncoming(msg)
	require.Len(t, client.Send, 1, "the same frame dispatches while its authenticated deadline is valid")
}

func TestPongDeadlineCannotExtendTokenLifetime(t *testing.T) {
	expires := time.Now().Add(time.Second)
	client := &Client{TokenExpiresAt: expires}
	require.Equal(t, expires, client.connectionDeadline(pongWait))
	client.TokenExpiresAt = time.Now().Add(time.Hour)
	require.WithinDuration(t, time.Now().Add(pongWait), client.connectionDeadline(pongWait), time.Second)
}
