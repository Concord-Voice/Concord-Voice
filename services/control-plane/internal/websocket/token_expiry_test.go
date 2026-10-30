package websocket

import (
	"context"
	"database/sql"
	"errors"
	"io"
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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
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

// Prepare the short lifetime before binding it, using the public canonical mint
// sequence. The committed SID and signed bearer share exactly one deadline.
func expiryCanonicalAccess(t *testing.T, db *sql.DB, userID uuid.UUID, deadline time.Time) admissionAccess {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || err == sql.ErrTxDone) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(userID)
	require.NoError(t, err)
	var epoch string
	var verified bool
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COALESCE(credential_epoch, ''), email_verified FROM users WHERE id = $1`, userID).Scan(&epoch, &verified))
	family, err := loginfamily.CreateFamilyTx(ctx, tx, user, epoch)
	require.NoError(t, err)
	sid := uuid.New()
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: userID, SID: sid, CredentialEpoch: epoch, EmailVerified: verified}, deadline.Add(-auth.AccessTokenTTL))
	require.NoError(t, err)
	require.Equal(t, deadline, prepared.ExpiresAt())
	require.NoError(t, loginfamily.BindSIDTx(ctx, tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	token, err := prepared.Sign(testJWTSecret)
	require.NoError(t, err)
	return admissionAccess{userID: userID, sid: sid, epoch: epoch, expiresAt: prepared.ExpiresAt(), token: token}
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
			userID := presenceTestUser(t, db)
			expires := time.Now().UTC().Add(3 * time.Second).Truncate(time.Second)
			access := expiryCanonicalAccess(t, db, userID, expires)
			query := "?token=" + access.token
			if rail == "ticket" {
				query = "?ticket=" + admissionMintTicket(t, db, rc, access)
			}
			registered := make(chan *Client, 1)
			unregistered := make(chan *Client, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				var client *Client
				select {
				case client = <-hub.register:
				case <-hub.done:
					return
				}
				defer client.releaseConnection() // The fixture owns the hub's reference.
				defer client.closeOutbound()
				defer client.stopTokenExpiry()
				defer func() { _ = client.Conn.Close() }()
				registered <- client
				client.Send <- []byte(`{"type":"control"}`)
				select {
				case removed := <-hub.unregister:
					unregistered <- removed
				case <-hub.done:
				}
			}()
			t.Cleanup(func() {
				// There is no Run loop in this fixture; cancel its channel receiver
				// even if authentication fails before registration, then join it.
				hub.closeOnce.Do(func() { close(hub.done) })
				<-joined
			})
			peer, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws"+query, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = peer.Close() })
			var client *Client
			select {
			case client = <-registered:
			case <-time.After(time.Second):
				t.Fatal("authenticated client did not reach registration")
			}
			require.Equal(t, userID, client.UserID)
			require.Equal(t, access.sid.String(), client.SessionID)
			require.True(t, expires.Equal(client.tokenDeadline()), "registered token deadline must equal the signed expiry instant")
			require.NoError(t, peer.SetReadDeadline(expires.Add(time.Second)))
			_, frame, err := peer.ReadMessage()
			require.NoError(t, err)
			require.JSONEq(t, `{"type":"control"}`, string(frame))
			_, _, err = peer.ReadMessage()
			require.True(t, ws.IsCloseError(err, ws.ClosePolicyViolation), "deadline must close the socket: %v", err)
			var closeErr *ws.CloseError
			require.ErrorAs(t, err, &closeErr)
			require.Equal(t, "Access token expired", closeErr.Text)
			select {
			case removed := <-unregistered:
				require.Same(t, client, removed)
			case <-time.After(time.Second):
				t.Fatal("expired client was not unregistered")
			}
			require.Eventually(t, func() bool {
				hub.admissionMu.Lock()
				defer hub.admissionMu.Unlock()
				return hub.admittedConnections == 0 && len(hub.admittedConnectionsByUser) == 0
			}, time.Second, 10*time.Millisecond, "all admission owners must release the expired socket")
		})
	}
}

func TestExpiredTokenUnregistersWhilePostRegistrationFenceStalls(t *testing.T) {
	db := setupHubTestDB(t)
	rc := setupHubTestRedis(t)
	// The stalled Redis read can time out before release, requiring DB fallback.
	fence := credepoch.New(db, rc, logger.NewWithWriter(io.Discard))
	userID := presenceTestUser(t, db)
	deadline := time.Now().UTC().Add(4 * time.Second).Truncate(time.Second)
	access := expiryCanonicalAccess(t, db, userID, deadline)
	ticket := admissionMintTicket(t, db, rc, access)
	require.NoError(t, rc.Set(context.Background(), credepoch.Key(userID.String()), "none", time.Minute).Err())
	hook := &stalledFenceCheckHook{
		key: credepoch.Key(userID.String()), entered: make(chan struct{}), release: make(chan struct{}),
	}
	releaseFence := sync.OnceFunc(func() { close(hook.release) })
	hub := NewHub(db, rc)
	go hub.Run()
	handler := NewHandler(hub, db, rc, testJWTSecret, []string{"*"}, fence, nil)
	handlerDone := make(chan struct{})
	var handlerPanic any
	router := gin.New()
	router.GET("/ws", func(c *gin.Context) {
		defer func() {
			handlerPanic = recover()
			close(handlerDone)
		}()
		handler.HandleWebSocket(c)
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Shutdown)
	// Producer authentication must finish before counting the two handler fence
	// reads; the second read then belongs to the registered socket's recheck.
	rc.AddHook(hook)
	t.Cleanup(func() {
		// Release and join before server/hub cleanup, including fatal paths.
		releaseFence()
		select {
		case <-handlerDone:
			require.Nil(t, handlerPanic, "WebSocket handler must return without panicking")
		case <-time.After(2 * time.Second):
			t.Error("WebSocket handler did not return after releasing the fence")
		}
	})
	peer, response, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+wsTicketPath+ticket, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	t.Cleanup(func() { _ = peer.Close() })
	select {
	case <-hook.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("post-registration credential check did not stall")
	}
	var registeredClient *Client
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		if len(hub.clients) != 1 || len(hub.userClients[userID]) != 1 {
			return false
		}
		for _, client := range hub.clients {
			if client.UserID == userID && client.SessionID == access.sid.String() {
				registeredClient = client
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "client must be registered before expiry assertion")
	require.True(t, deadline.Equal(registeredClient.tokenDeadline()), "registered token deadline must equal the signed expiry instant")
	require.EqualValues(t, 2, hook.checks.Load(), "stall must be the post-registration fence check")
	require.True(t, time.Now().Before(deadline), "registration ownership must be proven before expiry")
	hub.admissionMu.Lock()
	admitted := hub.admittedConnections
	hub.admissionMu.Unlock()
	require.Equal(t, 1, admitted, "the stalled registered socket must still own its admission reservation")
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return len(hub.clients) == 0 && len(hub.userClients) == 0
	}, 6*time.Second, 10*time.Millisecond, "deadline must remove the registered socket before the fence returns")
	select {
	case <-hook.release:
		t.Fatal("expiry assertion requires the post-registration fence to remain stalled")
	default:
	}
	releaseFence()
	select {
	case <-handlerDone:
		require.Nil(t, handlerPanic, "WebSocket handler must return without panicking")
	case <-time.After(2 * time.Second):
		t.Fatal("WebSocket handler did not return after releasing the fence")
	}
	require.Eventually(t, func() bool {
		hub.admissionMu.Lock()
		defer hub.admissionMu.Unlock()
		return hub.admittedConnections == 0 && len(hub.admittedConnectionsByUser) == 0
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
