package sessions_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	revokeAllMintBarrierLockID   = "2457001"
	revokeAllMintBarrierFunction = `
		CREATE OR REPLACE FUNCTION revoke_all_mint_barrier() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(` + revokeAllMintBarrierLockID + `);
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql`
)

type revokeAllRaceDisconnector struct{}

func (revokeAllRaceDisconnector) DisconnectUser(uuid.UUID) {}

// TestRevokeAllSessions_ConcurrentMintDoesNotSurvive is a regression for #2457.
// It holds a real mint after its refresh row is inserted but before commit, then
// proves revoke-all waits, commits afterward, and revokes that row.
func TestRevokeAllSessions_ConcurrentMintDoesNotSurvive(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeallmint")
	ctx := context.Background()

	installRevokeAllMintBarrier(t, ts.DB)
	releaseMint := holdRevokeAllMintBarrier(t)

	mintHandler := auth.NewHandler(
		ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, revokeAllRaceDisconnector{},
	)
	mintDone := make(chan mintResult, 1)
	go func() {
		_, refresh, _, err := mintHandler.IssueAccessAndRefresh(ctx, user.ID)
		mintDone <- mintResult{refresh: refresh, err: err}
	}()

	assertMintIsBlockedAtInsert(t, ts.DB)

	revokeDone := make(chan int, 1)
	go func() {
		w := ts.DoRequest("POST", revokeAllPath, map[string]any{
			"password":        testhelpers.TestAuthPlaintext,
			"include_current": true,
		}, testhelpers.AuthHeaders(user.AccessToken))
		revokeDone <- w.Code
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		err := ts.DB.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond,
		"revoke-all must wait on the users-row lock held by the concurrent mint")

	releaseMint()

	var mint mintResult
	select {
	case mint = <-mintDone:
		require.NoError(t, mint.err)
	case <-time.After(10 * time.Second):
		t.Fatal("mint did not finish after releasing its insert barrier")
	}

	select {
	case code := <-revokeDone:
		require.Equal(t, http.StatusOK, code)
	case <-time.After(10 * time.Second):
		t.Fatal("revoke-all did not finish after the mint committed")
	}

	var revokedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE token_hash = $1`, auth.HashRefreshToken(mint.refresh),
	).Scan(&revokedAt))
	require.True(t, revokedAt.Valid, "the refresh session minted during revoke-all must be revoked")
}

type mintResult struct {
	refresh string
	err     error
}

func installRevokeAllMintBarrier(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(revokeAllMintBarrierFunction)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TRIGGER revoke_all_mint_barrier AFTER INSERT ON refresh_tokens
		FOR EACH ROW EXECUTE FUNCTION revoke_all_mint_barrier()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, dropErr := db.Exec(`DROP TRIGGER IF EXISTS revoke_all_mint_barrier ON refresh_tokens`); dropErr != nil {
			t.Errorf("drop revoke-all mint barrier trigger: %v", dropErr)
		}
		if _, dropErr := db.Exec(`DROP FUNCTION IF EXISTS revoke_all_mint_barrier()`); dropErr != nil {
			t.Errorf("drop revoke-all mint barrier function: %v", dropErr)
		}
	})
}

func holdRevokeAllMintBarrier(t *testing.T) func() {
	t.Helper()
	lockDB, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	lockDB.SetMaxOpenConns(1)
	conn, err := lockDB.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1::bigint)`, revokeAllMintBarrierLockID)
	require.NoError(t, err)

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1::bigint)`, revokeAllMintBarrierLockID); unlockErr != nil {
			t.Errorf("release revoke-all mint barrier: %v", unlockErr)
		}
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("close revoke-all barrier connection: %v", closeErr)
		}
		if closeErr := lockDB.Close(); closeErr != nil {
			t.Errorf("close revoke-all barrier database: %v", closeErr)
		}
	}
	t.Cleanup(release)
	return release
}

func assertMintIsBlockedAtInsert(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND wait_event = 'advisory'
			  AND query LIKE 'INSERT INTO refresh_tokens%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond, "mint did not reach the refresh-token insert barrier")
}
