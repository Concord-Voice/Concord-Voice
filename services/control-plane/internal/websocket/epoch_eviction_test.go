package websocket

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
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

	// A destructive reset has committed: the durable epoch is now newEpoch.
	require.NoError(t, rc.Set(context.Background(),
		credepoch.Key(userID.String()), "active:newEpoch", time.Minute).Err())

	// The socket authenticated under the superseded epoch.
	err := fence.Check(context.Background(), userID.String(), "oldEpoch")
	require.ErrorIs(t, err, credepoch.ErrEpochMismatch,
		"the post-registration re-check must observe the advanced epoch")
}

// TestPostRegistrationKeepsCurrentEpoch is the negative control.
func TestPostRegistrationKeepsCurrentEpoch(t *testing.T) {
	rc, fence := newTestFence(t)
	userID := uuid.New()

	require.NoError(t, rc.Set(context.Background(),
		credepoch.Key(userID.String()), "active:goodEpoch", time.Minute).Err())

	require.NoError(t, fence.Check(context.Background(), userID.String(), "goodEpoch"))
}

// The two tests above cover the fence PREDICATE but never call HandleWebSocket, so
// they pass whether or not the eviction exists (verified by deleting the guard). The
// two end-to-end tests below are the actual regression lock on the call site.
//
// Forcing the race deterministically relies on the register channel being UNBUFFERED:
// the pre-upgrade fence check and the post-registration re-check read the SAME epoch
// value, so a mismatch can only arise if the durable epoch advances between them. By
// leaving hub.Run() unstarted, the handler blocks on `hub.register <- client` after
// the 101 has been written — the test then advances the epoch exactly inside that
// window, as a destructive reset would, and releases the Hub.

func startEvictionTestSocket(t *testing.T, ticket string) (*Hub, *redis.Client, uuid.UUID, *gorillaWS.Conn) {
	t.Helper()
	db := setupHubTestDB(t)
	rc := setupHubTestRedis(t)
	hub := NewHub(db, rc)
	// hub.Run() is deliberately NOT started yet — see the comment above.

	userID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, $4, true, true)`,
		userID.String(), ticket+"@test.concord.chat", ticket, hash)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id = $1`, userID.String()) //nolint:errcheck,gosec
	})

	ctx := context.Background()
	// Ticket value is "userID[:sessionID[:credEpoch]]" — pin the socket to E1.
	require.NoError(t, rc.Set(ctx, wsTicketKeyPfx+ticket,
		userID.String()+":test-session:E1", 30*time.Second).Err())
	require.NoError(t, rc.Set(ctx,
		credepoch.Key(userID.String()), "active:E1", time.Minute).Err())

	fence := credepoch.New(db, rc, logger.NewWithWriter(io.Discard))
	h := NewHandler(hub, db, rc, testJWTSecret, []string{"*"}, fence, nil)
	srv := setupWSTestServer(t, h, hub)

	conn, resp, err := gorillaWS.DefaultDialer.Dial("ws"+srv.URL[4:]+wsTicketPath+ticket, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode,
		"the pre-upgrade fence check passes under E1, so the upgrade must succeed")
	t.Cleanup(func() { _ = conn.Close() })

	return hub, rc, userID, conn
}

func TestHandleWebSocketEvictsWhenEpochAdvancesDuringRegistration(t *testing.T) {
	hub, rc, userID, conn := startEvictionTestSocket(t, "epochevictyes")

	// A destructive reset commits while the handler is blocked on the register send.
	require.NoError(t, rc.Set(context.Background(),
		credepoch.Key(userID.String()), "active:E2", time.Minute).Err())
	go hub.Run()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, readErr := conn.ReadMessage()
	require.Error(t, readErr,
		"the post-registration re-check must observe E2 and close the socket")
}

// TestHandleWebSocketKeepsSocketWhenEpochUnchanged is the negative control for the
// call site: without it, an always-close guard would pass the test above.
func TestHandleWebSocketKeepsSocketWhenEpochUnchanged(t *testing.T) {
	hub, _, _, conn := startEvictionTestSocket(t, "epochevictno")

	// No reset: the epoch stays E1 across the registration window.
	go hub.Run()

	// A registered socket receives the hub's initial frame. Reading it cleanly is
	// positive evidence the connection survived the post-registration re-check —
	// stronger than a read timeout, which would also be produced by a socket that
	// was never written to.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, readErr := conn.ReadMessage()
	require.NoError(t, readErr,
		"a matching epoch must leave the socket open and serving frames")
}
