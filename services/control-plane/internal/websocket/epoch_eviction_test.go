package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2414 §C: registerClient and the DisconnectUser handler are both serviced by
// the Hub's single Run goroutine, so a socket whose register lands AFTER a
// reset's disconnect sweep is never swept and would otherwise survive until its
// access token expires. HandleWebSocket re-checks the fence immediately after
// the (unbuffered) register handoff to close that residue.
//
// newTestFence mirrors credepoch_ws_auth_test.go's fenceWiredWSHandler setup: a
// real Redis client (setupHubTestRedis) backing a credepoch.Fence with a nil
// DB — these tests always populate the Redis cache directly, so the DB
// read-through/fallback path is never exercised here.

func newTestFence(t *testing.T) (*redis.Client, *credepoch.Fence) {
	t.Helper()
	rc := setupHubTestRedis(t)
	fence := credepoch.New(nil, rc, logger.NewWithWriter(io.Discard))
	return rc, fence
}

// TestPostRegistrationEvictsSupersededEpoch proves a socket whose register landed
// after a reset's disconnect sweep is closed rather than surviving to token expiry
// (#2414 §C).
func TestPostRegistrationEvictsSupersededEpoch(t *testing.T) {
	rc, fence := newTestFence(t) // mirror credepoch_ws_auth_test.go's setup
	userID := uuid.New()

	// The Redis fence reflects an advanced epoch.
	require.NoError(t, rc.Set(context.Background(),
		credepoch.Key(userID.String()), "active:22222222222222222222222222222222", time.Minute).Err())

	// The socket authenticated under the superseded epoch.
	err := fence.Check(context.Background(), userID.String(), "11111111111111111111111111111111")
	require.ErrorIs(t, err, credepoch.ErrEpochMismatch,
		"the post-registration re-check must observe the advanced epoch")
}

// TestPostRegistrationKeepsCurrentEpoch is the negative control.
func TestPostRegistrationKeepsCurrentEpoch(t *testing.T) {
	rc, fence := newTestFence(t)
	userID := uuid.New()

	require.NoError(t, rc.Set(context.Background(),
		credepoch.Key(userID.String()), "active:11111111111111111111111111111111", time.Minute).Err())

	require.NoError(t, fence.Check(context.Background(), userID.String(), "11111111111111111111111111111111"))
}

// The two tests above cover the fence PREDICATE but never call HandleWebSocket, so
// they pass whether or not the eviction exists (verified by deleting the guard). The
// two end-to-end tests below are the actual regression lock on the call site.
//
// Forcing the race deterministically relies on the register channel being UNBUFFERED:
// the pre-upgrade fence check and the post-registration re-check read the SAME epoch
// value, so a mismatch can only arise if the Redis fence advances between them. By
// leaving hub.Run() unstarted, the handler blocks on `hub.register <- client` after
// the 101 has been written — the test then advances the Redis fence exactly inside that
// window, isolating the fence recheck from the final SQL family recheck, and releases the Hub.

// epochFixtureMintAccess creates durable authority before publishing a bearer.
// The prepared expiry is bound exactly; no refresh-token row or private context
// is invented by this fixture.
func epochFixtureMintAccess(t *testing.T, db *sql.DB, label string) (uuid.UUID, uuid.UUID, string, time.Time) {
	t.Helper()
	ctx := context.Background()
	userID, sid := uuid.New(), uuid.New()
	const epoch = "11111111111111111111111111111111"
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, credential_epoch)
   VALUES ($1, $2, $3, $4, true, true, $5)`,
		userID, userID.String()+"@test.concord.chat", label, hash, epoch)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
		require.NoError(t, err)
	})
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(userID)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(ctx, tx, user, epoch)
	require.NoError(t, err)
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{
		UserID: userID, SID: sid, CredentialEpoch: epoch, EmailVerified: true,
	}, time.Now())
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(ctx, tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	token, err := prepared.Sign(testJWTSecret)
	require.NoError(t, err)
	return userID, sid, token, prepared.ExpiresAt()
}

func startEvictionTestSocket(t *testing.T, label string) (*Hub, *redis.Client, uuid.UUID, *gorillaWS.Conn, func()) {
	t.Helper()
	db := setupHubTestDB(t)
	rc := setupHubTestRedis(t)
	hub := NewHub(db, rc)
	var runOnce sync.Once
	runDone := make(chan struct{})
	startRun := func() {
		runOnce.Do(func() {
			go func() {
				defer close(runDone)
				hub.Run()
			}()
		})
	}
	fence := credepoch.New(db, rc, logger.NewWithWriter(io.Discard))
	h := NewHandler(hub, db, rc, testJWTSecret, []string{"*"}, fence, nil)
	router := gin.New()
	router.GET("/ws", h.HandleWebSocket)
	router.POST("/ticket", middleware.AuthRequired(testJWTSecret, rc, db, fence), auth.NewWSTicketHandler(rc, testJWTSecret).IssueTicket)
	srv := httptest.NewServer(router)
	// Register before ANY fatal assertion after hub construction. A failed mint
	// or handshake must still give Shutdown a Run owner that closes stopped.
	t.Cleanup(func() {
		startRun()
		hub.Shutdown()
		<-runDone
		srv.Close()
	})

	userID, sid, bearer, expiry := epochFixtureMintAccess(t, db, label)
	ctx := context.Background()
	const epoch = "11111111111111111111111111111111"
	require.NoError(t, rc.Set(ctx, credepoch.Key(userID.String()), "active:"+epoch, time.Minute).Err())
	req := httptest.NewRequest(http.MethodPost, "/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var issued struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &issued))
	require.NotEmpty(t, issued.Ticket)
	value, err := rc.Get(ctx, wsTicketKeyPfx+issued.Ticket).Result()
	require.NoError(t, err)
	require.Equal(t, []string{userID.String(), sid.String(), epoch, strconv.FormatInt(expiry.Unix(), 10)}, strings.Split(value, ":"))
	ttl, err := rc.PTTL(ctx, wsTicketKeyPfx+issued.Ticket).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, 30*time.Second)
	require.LessOrEqual(t, ttl, time.Until(expiry))

	// Run remains unstarted: the 101 leaves registration blocked until the caller
	// performs its epoch/theft mutation and invokes this same once-only start.
	socketURL := "ws" + srv.URL[4:] + wsTicketPath + issued.Ticket
	conn, resp, err := gorillaWS.DefaultDialer.Dial(socketURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode,
		"canonical current-epoch authority must pass the pre-upgrade gates")
	reused, refusal, err := gorillaWS.DefaultDialer.Dial(socketURL, nil)
	if reused != nil {
		_ = reused.Close()
	}
	require.Error(t, err, "the real minted ticket must be atomically single use")
	require.NotNil(t, refusal)
	require.Equal(t, http.StatusUnauthorized, refusal.StatusCode)
	require.NoError(t, refusal.Body.Close())
	return hub, rc, userID, conn, startRun
}

func TestHandleWebSocketEvictsWhenEpochAdvancesDuringRegistration(t *testing.T) {
	_, rc, userID, conn, startRun := startEvictionTestSocket(t, "epochevictyes")

	// Advance only the Redis fence while registration is blocked, so the final
	// unchanged SQL family cannot mask deletion of the fence recheck.
	require.NoError(t, rc.Set(context.Background(),
		credepoch.Key(userID.String()), "active:22222222222222222222222222222222", time.Minute).Err())
	startRun()

	requireSocketClosedBeforeDeadline(t, conn)
}

// TestHandleWebSocketKeepsSocketWhenEpochUnchanged is the negative control for the
// call site: without it, an always-close guard would pass the test above.
func TestHandleWebSocketKeepsSocketWhenEpochUnchanged(t *testing.T) {
	_, _, _, conn, startRun := startEvictionTestSocket(t, "epochevictno")

	// No reset: the canonical epoch stays unchanged across registration.
	startRun()

	// A registered socket receives the hub's initial frame. Reading it cleanly is
	// positive evidence the connection survived the post-registration re-check —
	// stronger than a read timeout, which would also be produced by a socket that
	// was never written to.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, readErr := conn.ReadMessage()
	require.NoError(t, readErr,
		"a matching epoch must leave the socket open and serving frames")
}

// A completed sweep can remove its SQL row and Redis marker before the socket
// reaches the hub. The durable generation still fences that registration.
func TestHandleWebSocketEvictsWhenTheftSweepCompletesDuringRegistration(t *testing.T) {
	hub, rc, userID, conn, startRun := startEvictionTestSocket(t, "theftevictyes")

	_, err := hub.db.Exec(`UPDATE users SET token_theft_generation = token_theft_generation + 1 WHERE id = $1`, userID)
	require.NoError(t, err)
	blocked, err := rc.Exists(context.Background(), "token_theft_blocked:"+userID.String()).Result()
	require.NoError(t, err)
	require.Zero(t, blocked, "the transient marker has already cleared")
	startRun()

	requireSocketClosedBeforeDeadline(t, conn)
}

func TestHandleWebSocketEvictsWhenTheftPendingDuringRegistration(t *testing.T) {
	hub, _, userID, conn, startRun := startEvictionTestSocket(t, "theftevictpending")
	_, err := hub.db.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, operation_id) VALUES ($1, $2)`, userID, uuid.New(),
	)
	require.NoError(t, err)
	startRun()

	requireSocketClosedBeforeDeadline(t, conn)
}

func requireSocketClosedBeforeDeadline(t *testing.T, conn *gorillaWS.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	for {
		_, _, readErr := conn.ReadMessage()
		if readErr == nil {
			continue // Hub bootstrap may send a frame before the handler evicts.
		}
		var netErr net.Error
		require.False(t, errors.As(readErr, &netErr) && netErr.Timeout(), "socket closure must precede read deadline")
		return
	}
}
