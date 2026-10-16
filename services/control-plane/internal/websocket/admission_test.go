package websocket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestHandleWebSocketAdmissionPerUser(t *testing.T) {
	for _, authPath := range []string{"ticket", "JWT"} {
		t.Run(authPath, func(t *testing.T) {
			db := setupHubTestDB(t)
			rc := setupHubTestRedis(t)
			hub := NewHub(db, rc)
			go hub.Run()
			handler := NewHandler(hub, db, rc, testJWTSecret, []string{"*"}, nil, nil)
			server := setupWSTestServer(t, handler, hub)
			userID := presenceTestUser(t, db)
			attempt := 0
			connect := func(id uuid.UUID) (*gorillaWS.Conn, *http.Response, error) {
				attempt++
				identity := id.String()
				if attempt%2 == 0 {
					identity = strings.ToUpper(identity)
				}
				path := wsTokenPath + generateTestJWT(t, identity, testJWTSecret)
				if authPath == "ticket" {
					ticket := fmt.Sprintf("admission-%d", attempt)
					require.NoError(t, rc.Set(context.Background(), wsTicketKeyPfx+ticket, testTicketValue(identity, "", ""), 30*time.Second).Err())
					path = wsTicketPath + ticket
				}
				conn, response, err := gorillaWS.DefaultDialer.Dial("ws"+server.URL[4:]+path, nil)
				if conn != nil {
					t.Cleanup(func() { _ = conn.Close() })
				}
				return conn, response, err
			}

			var first *gorillaWS.Conn
			for i := range maxConnectionsPerUser {
				conn, response, err := connect(userID)
				require.NoError(t, err)
				require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
				if i == 0 {
					first = conn
				}
			}
			_, response, err := connect(userID)
			require.Error(t, err, "one account must not allocate unlimited WebSocket clients")
			require.NotNil(t, response)
			body := response.Body
			defer func() { require.NoError(t, body.Close()) }()
			require.Equal(t, http.StatusTooManyRequests, response.StatusCode)

			_, response, err = connect(presenceTestUser(t, db))
			require.NoError(t, err, "another account must retain its connection budget")
			require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)

			require.NoError(t, first.WriteJSON(IncomingMessage{Type: "connection_ready_probe"}))
			require.NoError(t, first.SetReadDeadline(time.Now().Add(5*time.Second)))
			for {
				var message OutgoingMessage
				require.NoError(t, first.ReadJSON(&message), "existing sockets must keep serving messages after a refusal")
				if message.Type == "connection_ready" {
					break
				}
			}
			require.NoError(t, first.Close())
			require.Eventually(t, func() bool {
				hub.admissionMu.Lock()
				defer hub.admissionMu.Unlock()
				return hub.admittedConnectionsByUser[userID] == maxConnectionsPerUser-1
			}, 5*time.Second, time.Millisecond)
			_, response, err = connect(userID)
			require.NoError(t, err, "disconnect must make room for a replacement")
			require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
		})
	}
}

func TestReserveConnectionLimitsAndDuplicateRelease(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()
	var firstRelease func()
	for i := range maxConnections {
		if i%maxConnectionsPerUser == 0 {
			userID = uuid.New()
		}
		release, ok := hub.reserveConnection(userID)
		require.True(t, ok)
		t.Cleanup(release)
		if i == 0 {
			firstRelease = release
		}
		if (i+1)%maxConnectionsPerUser == 0 {
			_, ok = hub.reserveConnection(userID)
			require.False(t, ok, "per-user limit must include unregistered reservations")
		}
	}
	_, ok := hub.reserveConnection(uuid.New())
	require.False(t, ok, "the process budget must span different accounts")
	firstRelease()
	firstRelease()
	release, ok := hub.reserveConnection(uuid.New())
	require.True(t, ok)
	t.Cleanup(release)
	_, ok = hub.reserveConnection(uuid.New())
	require.False(t, ok, "a duplicate release must not free another connection's slot")
}

func TestReserveConnectionDoesNotWaitForHubLookup(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.mu.Lock() // registerClient holds this while querying the database.
	defer hub.mu.Unlock()
	result := make(chan bool, 1)
	go func() {
		release, ok := hub.reserveConnection(uuid.New())
		if ok {
			release()
		}
		result <- ok
	}()
	select {
	case ok := <-result:
		require.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("admission blocked behind hub registration")
	}
}

func TestReserveConnectionConcurrent(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global=%t", global), func(t *testing.T) {
			hub := NewHub(nil, nil)
			if global {
				for range maxConnections - maxConnectionsPerUser {
					release, ok := hub.reserveConnection(uuid.New())
					require.True(t, ok)
					t.Cleanup(release)
				}
			}
			userID := uuid.New()
			const attempts = 64
			accepted := make(chan func(), attempts)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for range attempts {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					id := userID
					if global {
						id = uuid.New()
					}
					if release, ok := hub.reserveConnection(id); ok {
						accepted <- release
					}
				}()
			}
			close(start)
			workers.Wait()
			close(accepted)
			require.Len(t, accepted, maxConnectionsPerUser)
			for release := range accepted {
				release()
			}
		})
	}
}

func TestClientAdmissionWaitsForEveryOwner(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()
	release, ok := hub.reserveConnection(userID)
	require.True(t, ok)
	t.Cleanup(release)
	client := &Client{releaseAdmission: release}
	client.admissionOwners.Store(4)
	for range maxConnectionsPerUser - 1 {
		release, ok := hub.reserveConnection(userID)
		require.True(t, ok)
		t.Cleanup(release)
	}
	for range 3 {
		client.releaseConnection()
		_, ok := hub.reserveConnection(userID)
		require.False(t, ok, "capacity must remain reserved while any owner is still running")
	}
	client.releaseConnection()
	release, ok = hub.reserveConnection(userID)
	require.True(t, ok)
	t.Cleanup(release)
	client.releaseConnection()
	_, ok = hub.reserveConnection(userID)
	require.False(t, ok)
}

func TestHandleWebSocketAdmissionBeforeLookupAndUpgrade(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global=%t", global), func(t *testing.T) {
			rc := setupHubTestRedis(t)
			hub := NewHub(nil, rc)
			userID := uuid.New()
			limit := maxConnectionsPerUser
			if global {
				limit = maxConnections
			}
			for range limit {
				id := userID
				if global {
					id = uuid.New()
				}
				release, ok := hub.reserveConnection(id)
				require.True(t, ok)
				t.Cleanup(release)
			}
			handler := NewHandler(hub, nil, rc, testJWTSecret, nil, nil, nil)
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Request = httptest.NewRequest(http.MethodGet, wsTokenPath+generateTestJWT(t, userID.String(), testJWTSecret), nil)
			handler.HandleWebSocket(ctx)
			require.Equal(t, http.StatusTooManyRequests, response.Code)
			require.JSONEq(t, `{"error":"WebSocket connection limit reached"}`, response.Body.String())
			require.Empty(t, hub.clients, "a refused upgrade must not allocate or register a client")
		})
	}
}

func TestHandleWebSocketAdmissionUpgradeFailureReleases(t *testing.T) {
	db := setupHubTestDB(t)
	rc := setupHubTestRedis(t)
	hub := NewHub(db, rc)
	handler := NewHandler(hub, db, rc, testJWTSecret, nil, nil, nil)
	userID := presenceTestUser(t, db)
	for range maxConnectionsPerUser + 1 {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodGet, wsTokenPath+generateTestJWT(t, userID.String(), testJWTSecret), nil)
		handler.HandleWebSocket(ctx)
		require.Equal(t, http.StatusBadRequest, response.Code, "malformed upgrades must not fill the quota")
	}
	require.Zero(t, hub.admittedConnections)
	require.Empty(t, hub.admittedConnectionsByUser)
}

func TestHandleWebSocketAdmissionPendingShutdown(t *testing.T) {
	db := setupHubTestDB(t)
	rc := setupHubTestRedis(t)
	hub := NewHub(db, rc)
	handler := NewHandler(hub, db, rc, testJWTSecret, []string{"*"}, nil, nil)
	finished := make(chan struct{}, maxConnectionsPerUser+3)
	cancelRequests := make(chan context.CancelFunc, maxConnectionsPerUser+3)
	router := gin.New()
	router.GET("/ws", func(ctx *gin.Context) {
		requestCtx, cancel := context.WithCancel(ctx.Request.Context())
		defer cancel()
		ctx.Request = ctx.Request.WithContext(requestCtx)
		cancelRequests <- cancel
		handler.HandleWebSocket(ctx)
		finished <- struct{}{}
	})
	server := httptest.NewServer(router)
	stop := sync.OnceFunc(func() { close(hub.done) })
	t.Cleanup(func() {
		stop()
		server.Close()
	})
	userID := presenceTestUser(t, db)
	url := "ws" + server.URL[4:] + wsTokenPath + generateTestJWT(t, userID.String(), testJWTSecret)
	// No Run goroutine: every successful upgrade is still awaiting registration.
	for range maxConnectionsPerUser {
		conn, _, err := gorillaWS.DefaultDialer.Dial(url, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
	}
	conn, response, err := gorillaWS.DefaultDialer.Dial(url, nil)
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	require.Error(t, err)
	require.NotNil(t, response)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode)
	require.Empty(t, hub.clients)
	(<-cancelRequests)()
	require.Eventually(t, func() bool {
		hub.admissionMu.Lock()
		defer hub.admissionMu.Unlock()
		return hub.admittedConnectionsByUser[userID] == maxConnectionsPerUser-1
	}, 5*time.Second, time.Millisecond, "request cancellation must free a pending slot without hub shutdown")
	conn, _, err = gorillaWS.DefaultDialer.Dial(url, nil)
	require.NoError(t, err, "a canceled request must make room for a replacement")
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.Close(), "simulate a peer disappearing after upgrade")
	require.Eventually(t, func() bool {
		hub.admissionMu.Lock()
		defer hub.admissionMu.Unlock()
		return hub.admittedConnections == 0
	}, pendingRegistrationTimeout+5*time.Second, 10*time.Millisecond,
		"server timeout must release pending registrations after a hijacked peer disconnects")
	conn, _, err = gorillaWS.DefaultDialer.Dial(url, nil)
	require.NoError(t, err, "expired pending registrations must make room for a replacement")
	t.Cleanup(func() { _ = conn.Close() })
	stop()
	for range maxConnectionsPerUser + 3 {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("pending registration did not stop with the hub")
		}
	}
	require.Zero(t, hub.admittedConnections)
	require.Empty(t, hub.admittedConnectionsByUser)
	_, admitted := hub.reserveConnection(userID)
	require.False(t, admitted, "a stopped hub must refuse new reservations")
}

func TestHandleWebSocketAdmissionDisconnectReleases(t *testing.T) {
	for _, mode := range []string{"user", "session", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			db := setupHubTestDB(t)
			rc := setupHubTestRedis(t)
			hub := NewHub(db, rc)
			go hub.Run()
			handler := NewHandler(hub, db, rc, testJWTSecret, []string{"*"}, nil, nil)
			server := setupWSTestServer(t, handler, hub)
			userID := presenceTestUser(t, db)
			const session = "admission-session"
			ticket := "admission-disconnect"
			require.NoError(t, rc.Set(context.Background(), wsTicketKeyPfx+ticket, testTicketValue(userID.String(), session, ""), 30*time.Second).Err())
			conn, _, err := gorillaWS.DefaultDialer.Dial("ws"+server.URL[4:]+wsTicketPath+ticket, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, _, err = conn.ReadMessage()
			require.NoError(t, err, "the connection must be registered before disconnecting")
			switch mode {
			case "user":
				hub.DisconnectUser(userID)
			case "session":
				hub.DisconnectSession(session)
			case "shutdown":
				hub.Shutdown()
			}
			require.Eventually(t, func() bool {
				hub.admissionMu.Lock()
				defer hub.admissionMu.Unlock()
				return hub.admittedConnections == 0 && len(hub.admittedConnectionsByUser) == 0
			}, 5*time.Second, time.Millisecond, "both pumps and hub cleanup must release the slot exactly once")
		})
	}
}
