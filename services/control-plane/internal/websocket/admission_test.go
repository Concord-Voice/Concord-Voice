package websocket

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
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
	gorillaWS "github.com/gorilla/websocket"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

type admissionAccess struct {
	userID    uuid.UUID
	sid       uuid.UUID
	epoch     string
	expiresAt time.Time
	token     string
}

func admissionCanonicalAccess(t *testing.T, db *sql.DB, userID uuid.UUID) admissionAccess {
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
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: userID, SID: sid, CredentialEpoch: epoch, EmailVerified: verified}, time.Now())
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(ctx, tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	token, err := prepared.Sign(testJWTSecret)
	require.NoError(t, err)
	return admissionAccess{userID: userID, sid: sid, epoch: epoch, expiresAt: prepared.ExpiresAt(), token: token}
}

func admissionMintTicket(t *testing.T, db *sql.DB, rc *redis.Client, access admissionAccess) string {
	t.Helper()
	router := gin.New()
	router.POST("/ticket", middleware.AuthRequired(testJWTSecret, rc, db, nil), auth.NewWSTicketHandler(rc, testJWTSecret).IssueTicket)
	request := httptest.NewRequest(http.MethodPost, "/ticket", nil)
	request.Header.Set("Authorization", "Bearer "+access.token)
	response := httptest.NewRecorder()
	remainingBeforeIssue := time.Until(access.expiresAt)
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.NotEmpty(t, body.Ticket)
	value, err := rc.Get(context.Background(), wsTicketKeyPfx+body.Ticket).Result()
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%s:%s:%s:%d", access.userID, access.sid, access.epoch, access.expiresAt.Unix()), value)
	ttl, err := rc.PTTL(context.Background(), wsTicketKeyPfx+body.Ticket).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, 30*time.Second)
	// A frozen Redis test clock retains the issued TTL while wall time advances.
	// Bound it by the remaining access lifetime before issuance, with no slack.
	require.LessOrEqual(t, ttl, remainingBeforeIssue)
	return body.Ticket
}

// Only the UUID spelling changes; SID, epoch, expiry and all other claims stay bound.
func admissionUppercaseJWT(t *testing.T, access admissionAccess) string {
	t.Helper()
	claims := &auth.Claims{}
	_, err := jwt.ParseWithClaims(access.token, claims, func(*jwt.Token) (interface{}, error) { return []byte(testJWTSecret), nil }, jwt.WithValidMethods([]string{"HS256"}))
	require.NoError(t, err)
	claims.UserID = strings.ToUpper(claims.UserID)
	parsedUser, err := uuid.Parse(claims.UserID)
	require.NoError(t, err)
	require.Equal(t, access.userID, parsedUser)
	require.Equal(t, access.sid.String(), claims.SessionID)
	require.Equal(t, access.epoch, claims.CredentialEpoch)
	require.Equal(t, access.expiresAt.Unix(), claims.ExpiresAt.Unix())
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	return token
}

// This connector observes the exact downstream profile query while forwarding every
// authority transaction to real PostgreSQL. It does not script authentication SQL.
type admissionProfileConnector struct {
	driver.Connector
	reads atomic.Int64
}

func (c *admissionProfileConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &admissionProfileConn{Conn: conn, reads: &c.reads}, nil
}

type admissionProfileConn struct {
	driver.Conn
	reads *atomic.Int64
}

func (c *admissionProfileConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if query == "SELECT username, display_name, avatar_url FROM users WHERE id = $1" {
		c.reads.Add(1)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *admissionProfileConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *admissionProfileConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

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
			accesses := map[uuid.UUID]admissionAccess{userID: admissionCanonicalAccess(t, db, userID)}
			attempt := 0
			connect := func(id uuid.UUID) (*gorillaWS.Conn, *http.Response, error) {
				attempt++
				access, ok := accesses[id]
				if !ok {
					access = admissionCanonicalAccess(t, db, id)
					accesses[id] = access
				}
				token := access.token
				if attempt%2 == 0 {
					token = admissionUppercaseJWT(t, access)
				}
				path := wsTokenPath + token
				if authPath == "ticket" {
					ticket := admissionMintTicket(t, db, rc, access)
					if attempt%2 == 0 {
						// Consumer spelling variation of a genuinely produced ticket:
						// retain the same SID, epoch, deadline and Redis TTL.
						value, err := rc.Get(context.Background(), wsTicketKeyPfx+ticket).Result()
						require.NoError(t, err)
						parts := strings.Split(value, ":")
						require.Len(t, parts, 4)
						parts[0] = strings.ToUpper(parts[0])
						parsedUser, err := uuid.Parse(parts[0])
						require.NoError(t, err)
						require.Equal(t, access.userID, parsedUser)
						require.Equal(t, access.sid.String(), parts[1])
						require.Equal(t, access.epoch, parts[2])
						require.Equal(t, fmt.Sprint(access.expiresAt.Unix()), parts[3])
						require.NoError(t, rc.SetArgs(context.Background(), wsTicketKeyPfx+ticket, strings.Join(parts, ":"), redis.SetArgs{KeepTTL: true}).Err())
					}
					path = wsTicketPath + ticket
				}
				conn, response, err := gorillaWS.DefaultDialer.Dial("ws"+server.URL[4:]+path, nil)
				if conn != nil {
					t.Cleanup(func() { _ = conn.Close() })
				}
				if authPath == "ticket" && err == nil {
					duplicate, replayResponse, replayErr := gorillaWS.DefaultDialer.Dial("ws"+server.URL[4:]+path, nil)
					if duplicate != nil {
						require.NoError(t, duplicate.Close())
					}
					require.Error(t, replayErr, "a successfully redeemed ticket must be single use")
					require.NotNil(t, replayResponse)
					require.Equal(t, http.StatusUnauthorized, replayResponse.StatusCode)
					require.NoError(t, replayResponse.Body.Close())
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
			refusal, readErr := io.ReadAll(body)
			require.NoError(t, readErr)
			require.JSONEq(t, `{"error":"WebSocket connection limit reached"}`, string(refusal))

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
			db := setupHubTestDB(t)
			rc := setupHubTestRedis(t)
			userID := presenceTestUser(t, db)
			access := admissionCanonicalAccess(t, db, userID)
			connector, err := pq.NewConnector(dbtest.DatabaseURL())
			require.NoError(t, err)
			profile := &admissionProfileConnector{Connector: connector}
			observedDB := sql.OpenDB(profile)
			t.Cleanup(func() { require.NoError(t, observedDB.Close()) })
			hub := NewHub(observedDB, rc)
			limit := maxConnectionsPerUser
			if global {
				limit = maxConnections
			}
			releases := make([]func(), 0, limit)
			for range limit {
				id := userID
				if global {
					id = uuid.New()
				}
				release, ok := hub.reserveConnection(id)
				require.True(t, ok)
				t.Cleanup(release)
				releases = append(releases, release)
			}
			handler := NewHandler(hub, observedDB, rc, testJWTSecret, nil, nil, nil)
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Request = httptest.NewRequest(http.MethodGet, wsTokenPath+access.token, nil)
			handler.HandleWebSocket(ctx)
			require.Equal(t, http.StatusTooManyRequests, response.Code)
			require.JSONEq(t, `{"error":"WebSocket connection limit reached"}`, response.Body.String())
			require.Empty(t, hub.clients, "a refused upgrade must not allocate or register a client")
			require.Zero(t, profile.reads.Load(), "quota refusal must precede the profile lookup; required authority SQL remains allowed")
			require.Equal(t, limit, hub.admittedConnections, "refusal must not allocate a reservation")
			for _, release := range releases {
				release()
			}
			control := httptest.NewRecorder()
			controlCtx, _ := gin.CreateTestContext(control)
			controlCtx.Request = httptest.NewRequest(http.MethodGet, wsTokenPath+access.token, nil)
			handler.HandleWebSocket(controlCtx)
			require.Equal(t, http.StatusBadRequest, control.Code, "uncapped valid authority must reach the malformed upgrader")
			require.EqualValues(t, 1, profile.reads.Load(), "the profile hook must observe the uncapped control")
			require.Zero(t, hub.admittedConnections)
		})
	}
}

func TestHandleWebSocketAdmissionUpgradeFailureReleases(t *testing.T) {
	db := setupHubTestDB(t)
	rc := setupHubTestRedis(t)
	hub := NewHub(db, rc)
	handler := NewHandler(hub, db, rc, testJWTSecret, nil, nil, nil)
	userID := presenceTestUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	for range maxConnectionsPerUser + 1 {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodGet, wsTokenPath+access.token, nil)
		handler.HandleWebSocket(ctx)
		require.Equal(t, http.StatusBadRequest, response.Code, "malformed upgrades must not fill the quota")
		require.Zero(t, hub.admittedConnections)
		require.Empty(t, hub.admittedConnectionsByUser)
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
	access := admissionCanonicalAccess(t, db, userID)
	url := "ws" + server.URL[4:] + wsTokenPath + access.token
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
	refusal, readErr := io.ReadAll(response.Body)
	require.NoError(t, readErr)
	require.JSONEq(t, `{"error":"WebSocket connection limit reached"}`, string(refusal))
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
			access := admissionCanonicalAccess(t, db, userID)
			ticket := admissionMintTicket(t, db, rc, access)
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
				hub.DisconnectSession(access.sid.String())
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
