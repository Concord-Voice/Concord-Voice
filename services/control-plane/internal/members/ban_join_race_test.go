package members

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// banJoinRaceCapture models a separately wired replica: it pauses after the
// join path has checked server_bans and before it writes server_members.
type banJoinRaceCapture struct {
	db      *sql.DB
	entered chan struct{}
	release chan struct{}
	enter   sync.Once
	unlock  sync.Once
}

func (c *banJoinRaceCapture) WithGatedTx(
	ctx context.Context, _ presencecapture.Subject, work func(*sql.Tx) error,
) (returnErr error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, rollbackErr)
		}
	}()
	return work(tx)
}

func (c *banJoinRaceCapture) CaptureInTx(
	ctx context.Context, _ *sql.Tx, _ presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.enter.Do(func() { close(c.entered) })
	select {
	case <-c.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*banJoinRaceCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	return tx.Commit()
}

func (*banJoinRaceCapture) Abandon(presencecapture.Plan, presencecapture.Cause) {}

func (c *banJoinRaceCapture) unblock() {
	c.unlock.Do(func() { close(c.release) })
}

func waitForBanUserLock(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%FROM users%'
			  AND query LIKE '%id IN ($1, $2)%'
			  AND query LIKE '%FOR NO KEY UPDATE%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond, "ban must wait for the joining target's users lock")
}

func waitForJoinUserLock(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%FROM users WHERE id = ANY($1::uuid[])%'
			  AND query LIKE '%FOR NO KEY UPDATE%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond, "join must wait on the target user before locking its invite")
}

func waitForBanCompletion(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("ban did not finish after the joining transaction released its users lock")
		return nil
	}
}

func assertBannedWithoutMembership(t *testing.T, db *sql.DB, serverID, targetID string) {
	t.Helper()
	var member, banned bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, targetID,
	).Scan(&member))
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
		serverID, targetID,
	).Scan(&banned))
	require.False(t, member)
	require.True(t, banned)
}

func TestJoinServerCannotRecreateMembershipAfterConcurrentBan(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	owner := banTestUser(t, db)
	target := banTestUser(t, db)
	serverID := banTestServer(t, db, owner)
	inviteCode := "RACE3142"
	_, err := db.Exec(`
		INSERT INTO server_invites (id, server_id, code, created_by, max_uses, expires_at)
		VALUES ($1, $2, $3, $4, NULL, NOW() + INTERVAL '1 hour')
	`, uuid.NewString(), serverID, inviteCode, owner)
	require.NoError(t, err)

	log := logger.New("test")
	resolver := rbac.NewResolver(db, nil, log)
	hub := websocket.NewHub(db, nil)
	go hub.Run()
	defer hub.Shutdown()
	capture := &banJoinRaceCapture{db: db, entered: make(chan struct{}), release: make(chan struct{})}
	defer capture.unblock()
	joinHandler := invites.NewHandler(db, log, hub, resolver)
	joinHandler.SetGraphPresenceCapture(capture)

	joinResponse := httptest.NewRecorder()
	joinContext, _ := gin.CreateTestContext(joinResponse)
	joinContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/invites/join", strings.NewReader(`{"code":"`+inviteCode+`"}`))
	joinContext.Request.Header.Set("Content-Type", "application/json")
	joinContext.Set("user_id", target.String())
	joinDone := make(chan struct{})
	go func() {
		joinHandler.JoinServer(joinContext)
		close(joinDone)
	}()

	select {
	case <-capture.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("JoinServer did not reach capture after its ban read")
	}

	banHandler := &Handler{db: db, log: log, resolver: resolver}
	banHandler.SetAuthorityHandler(rbac.NewHandler(db, log, nil, hub, resolver, nil, nil))
	banDone := make(chan error, 1)
	go func() {
		banDone <- banHandler.execBanTx(context.Background(), serverID.String(), target.String(), owner.String(), nil, true)
	}()
	waitForBanUserLock(t, db)

	capture.unblock()
	select {
	case <-joinDone:
	case <-time.After(5 * time.Second):
		t.Fatal("JoinServer did not finish after capture resumed")
	}
	require.Equal(t, http.StatusOK, joinResponse.Code, joinResponse.Body.String())
	require.NoError(t, waitForBanCompletion(t, banDone))
	assertBannedWithoutMembership(t, db, serverID.String(), target.String())
}

func TestJoinServerLocksTargetBeforeInviteDuringServerDelete(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	owner := banTestUser(t, db)
	target := banTestUser(t, db)
	serverID := banTestServer(t, db, owner)
	_, err := db.Exec(`
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'owner', NOW()), ($1, $3, 'member', NOW())
	`, serverID, owner, target)
	require.NoError(t, err)
	channelID := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'voice', 'voice')`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, NOW(), NOW())
	`, channelID, target)
	require.NoError(t, err)
	inviteID := uuid.New()
	inviteCode := "LOCK3142"
	_, err = db.Exec(`
		INSERT INTO server_invites (id, server_id, code, created_by, max_uses, expires_at)
		VALUES ($1, $2, $3, $4, NULL, NOW() + INTERVAL '1 hour')
	`, inviteID, serverID, inviteCode, owner)
	require.NoError(t, err)

	// DeleteServer locks every Server Voice participant before deleting the
	// server, whose cascade then needs this invite row. Holding that same user
	// lock proves JoinServer cannot take the invite lock first and form a cycle.
	blocker, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := blocker.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback voice-user blocker: %v", rollbackErr)
		}
	}()
	var locked uuid.UUID
	require.NoError(t, blocker.QueryRow(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, target).Scan(&locked))
	require.Equal(t, target, locked)

	joinHandler := invites.NewHandler(db, logger.New("test"), nil, nil)
	joinResponse := httptest.NewRecorder()
	joinContext, _ := gin.CreateTestContext(joinResponse)
	joinContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/invites/join", strings.NewReader(`{"code":"`+inviteCode+`"}`))
	joinContext.Request.Header.Set("Content-Type", "application/json")
	joinContext.Set("user_id", target.String())
	joinDone := make(chan struct{})
	go func() {
		joinHandler.JoinServer(joinContext)
		close(joinDone)
	}()

	waitForJoinUserLock(t, db)
	probe, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	var lockedInvite uuid.UUID
	require.NoError(t, probe.QueryRow(`SELECT id FROM server_invites WHERE id = $1 FOR UPDATE NOWAIT`, inviteID).Scan(&lockedInvite))
	require.Equal(t, inviteID, lockedInvite)
	require.NoError(t, probe.Commit())
	require.NoError(t, blocker.Commit())

	select {
	case <-joinDone:
	case <-time.After(5 * time.Second):
		t.Fatal("JoinServer did not finish after the server-delete user lock released")
	}
	require.Equal(t, http.StatusConflict, joinResponse.Code, joinResponse.Body.String())
}

func TestAddMemberCannotRecreateMembershipAfterConcurrentBan(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	owner := banTestUser(t, db)
	target := banTestUser(t, db)
	serverID := banTestServer(t, db, owner)
	_, err := db.Exec(`
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', NOW())
	`, serverID, owner)
	require.NoError(t, err)
	log := logger.New("test")
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), log)
	capture := &banJoinRaceCapture{db: db, entered: make(chan struct{}), release: make(chan struct{})}
	defer capture.unblock()
	addHandler := &Handler{db: db, log: log, resolver: resolver}
	addHandler.SetGraphPresenceCapture(capture)

	addResponse := httptest.NewRecorder()
	addContext, _ := gin.CreateTestContext(addResponse)
	addContext.Request = httptest.NewRequest(http.MethodPost, "/servers/"+serverID.String()+"/members", strings.NewReader(`{"user_id":"`+target.String()+`"}`))
	addContext.Request.Header.Set("Content-Type", "application/json")
	addContext.Params = gin.Params{{Key: "id", Value: serverID.String()}}
	addContext.Set("user_id", owner.String())
	addDone := make(chan struct{})
	go func() {
		addHandler.AddMember(addContext)
		close(addDone)
	}()

	select {
	case <-capture.entered:
	case <-addDone:
		t.Fatalf("AddMember returned before capture: %d %s", addResponse.Code, addResponse.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("AddMember did not reach capture after its ban read")
	}

	banHandler := &Handler{db: db, log: log, resolver: resolver}
	banHandler.SetAuthorityHandler(rbac.NewHandler(db, log, nil, nil, resolver, nil, nil))
	banDone := make(chan error, 1)
	go func() {
		banDone <- banHandler.execBanTx(context.Background(), serverID.String(), target.String(), owner.String(), nil, true)
	}()
	waitForBanUserLock(t, db)

	capture.unblock()
	select {
	case <-addDone:
	case <-time.After(5 * time.Second):
		t.Fatal("AddMember did not finish after capture resumed")
	}
	require.Equal(t, http.StatusCreated, addResponse.Code, addResponse.Body.String())
	require.NoError(t, waitForBanCompletion(t, banDone))
	assertBannedWithoutMembership(t, db, serverID.String(), target.String())
}
