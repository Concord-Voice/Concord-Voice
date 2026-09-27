package users

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	erasureBlockBarrierLockID   = "-2907002"
	erasureBlockBarrierFunction = `
		CREATE FUNCTION account_erasure_marker_pause_test() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(` + erasureBlockBarrierLockID + `);
			RETURN OLD;
		END;
		$$ LANGUAGE plpgsql`
)

func TestDeleteAccountDrainsDMBlockMarkerForThirdPartyGroup(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	owner := testdb.CreateUser(t, db)
	erased := testdb.CreateUser(t, db)
	survivor := testdb.CreateUser(t, db)
	conversationID := seedErasureThirdPartyGroup(t, db, owner, erased, survivor)
	insertErasureBlockMarker(t, db, erased, survivor)

	service := NewAccountService(db, logger.New("test"))
	require.NoError(t, service.DeleteAccount(context.Background(), erased.String()))

	assert.Zero(t, countUsers(t, db, erased))
	assert.Zero(t, countErasureBlockMarkers(t, db, erased))
	assert.Zero(t, countErasureParticipants(t, db, conversationID, erased))
	assert.Equal(t, 1, countConversation(t, db, conversationID),
		"a third-party-owned group parent must survive account erasure")
	assert.Equal(t, 1, countErasureParticipants(t, db, conversationID, owner))
	assert.Equal(t, 1, countErasureParticipants(t, db, conversationID, survivor))
}

func TestDeleteAccountRollbackRestoresDMBlockMarkerAndParticipant(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	owner := testdb.CreateUser(t, db)
	erased := testdb.CreateUser(t, db)
	survivor := testdb.CreateUser(t, db)
	conversationID := seedErasureThirdPartyGroup(t, db, owner, erased, survivor)
	insertErasureBlockMarker(t, db, erased, survivor)
	installErasureAuditFailure(t, db)

	service := NewAccountService(db, logger.New("test"))
	err := service.DeleteAccount(context.Background(), erased.String())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "erasure marker rollback sentinel")
	assert.Equal(t, 1, countUsers(t, db, erased))
	assert.Equal(t, 1, countErasureBlockMarkers(t, db, erased))
	assert.Equal(t, 1, countErasureParticipants(t, db, conversationID, erased))
	assert.Equal(t, 1, countConversation(t, db, conversationID))
}

func TestConcurrentBlockCannotRecreateMarkerAcrossAccountErasure(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	owner := testdb.CreateUser(t, db)
	erased := testdb.CreateUser(t, db)
	peer := testdb.CreateUser(t, db)
	seedErasureThirdPartyGroup(t, db, owner, erased, peer)
	installErasureDeletePause(t, db)
	releaseErasure := holdErasureBlockBarrier(t)

	service := NewAccountService(db, logger.New("test"))
	erasureDone := make(chan error, 1)
	go func() { erasureDone <- service.DeleteAccount(context.Background(), erased.String()) }()
	assertErasureIsBlockedAtDelete(t, db)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", peer.String())
		c.Next()
	})
	router.POST("/friends/:user_id/block", friends.NewHandler(db, logger.New("test"), nil).BlockUser)
	blockDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost, "/friends/"+erased.String()+"/block", nil,
		))
		blockDone <- response
	}()
	assertBlockWaitsOnErasureUserLock(t, db)
	releaseErasure()

	select {
	case err := <-erasureDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("account erasure did not complete after the competing Block")
	}
	var response *httptest.ResponseRecorder
	select {
	case response = <-blockDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Block did not complete after account erasure")
	}
	assert.Equal(t, http.StatusInternalServerError, response.Code,
		"Block must fail closed once its target was erased")
	assert.Zero(t, countErasureBlockMarkers(t, db, erased),
		"a concurrent Block must not recreate durable evidence for an erased subject")
	assert.Zero(t, countErasureFriendships(t, db, peer, erased),
		"a concurrent Block must not create a friendship for an erased subject")
}

func seedErasureThirdPartyGroup(t *testing.T, db *sql.DB, owner, erased, survivor uuid.UUID) uuid.UUID {
	t.Helper()
	conversationID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO dm_conversations (id, is_group, is_personal, created_by)
		VALUES ($1, TRUE, FALSE, $2)
	`, conversationID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2), ($1, $3), ($1, $4)
	`, conversationID, owner, erased, survivor)
	require.NoError(t, err)
	return conversationID
}

func insertErasureBlockMarker(t *testing.T, db *sql.DB, blocker, blocked uuid.UUID) {
	t.Helper()
	low, high := blocker, blocked
	removeLow := true
	if low.String() > high.String() {
		low, high = high, low
		removeLow = false
	}
	_, err := db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES ($1, $2, $3, $4, $5)
	`, low, high, uuid.New(), removeLow, !removeLow)
	require.NoError(t, err)
}

func countErasureBlockMarkers(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM dm_block_reconciliations WHERE user_a_id = $1 OR user_b_id = $1
	`, userID).Scan(&count))
	return count
}

func countErasureFriendships(t *testing.T, db *sql.DB, first, second uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)
	`, first, second).Scan(&count))
	return count
}

func countErasureParticipants(t *testing.T, db *sql.DB, conversationID, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, userID).Scan(&count))
	return count
}

func installErasureAuditFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		CREATE FUNCTION account_erasure_marker_rollback_test() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'erasure marker rollback sentinel';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER account_erasure_marker_rollback_test
		BEFORE INSERT ON account_deletions
		FOR EACH ROW EXECUTE FUNCTION account_erasure_marker_rollback_test();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := db.Exec(`
			DROP TRIGGER IF EXISTS account_erasure_marker_rollback_test ON account_deletions;
			DROP FUNCTION IF EXISTS account_erasure_marker_rollback_test();
		`)
		assert.NoError(t, dropErr)
	})
}

func installErasureDeletePause(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(erasureBlockBarrierFunction)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TRIGGER account_erasure_marker_pause_test
		BEFORE DELETE ON users
		FOR EACH ROW EXECUTE FUNCTION account_erasure_marker_pause_test();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := db.Exec(`
			DROP TRIGGER IF EXISTS account_erasure_marker_pause_test ON users;
			DROP FUNCTION IF EXISTS account_erasure_marker_pause_test();
		`)
		assert.NoError(t, dropErr)
	})
}

func holdErasureBlockBarrier(t *testing.T) func() {
	t.Helper()
	lockDB, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	lockDB.SetMaxOpenConns(1)
	conn, err := lockDB.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(),
		`SELECT pg_advisory_lock($1::bigint)`, erasureBlockBarrierLockID)
	require.NoError(t, err)

	var once sync.Once
	release := func() {
		once.Do(func() {
			_, unlockErr := conn.ExecContext(context.Background(),
				`SELECT pg_advisory_unlock($1::bigint)`, erasureBlockBarrierLockID)
			require.NoError(t, unlockErr)
			require.NoError(t, conn.Close())
			require.NoError(t, lockDB.Close())
		})
	}
	t.Cleanup(release)
	return release
}

func assertErasureIsBlockedAtDelete(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND wait_event = 'advisory'
			  AND query LIKE '%DELETE FROM users WHERE id = $1%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond,
		"erasure did not reach the post-user-lock delete barrier")
}

func assertBlockWaitsOnErasureUserLock(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond,
		"Block did not wait on account erasure's canonical user lock")
}
