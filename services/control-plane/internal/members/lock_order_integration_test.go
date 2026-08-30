package members_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	_ "github.com/lib/pq"
)

func openMembersLockProbe(t *testing.T) *sql.DB {
	t.Helper()
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(2)
	probe.SetMaxIdleConns(2)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	return probe
}

func holdMembersAdvisoryBarrier(t *testing.T, probe *sql.DB, key int64) (*sql.Conn, func()) {
	t.Helper()
	conn, err := probe.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, key)
	require.NoError(t, err)
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
			require.NoError(t, unlockErr)
			require.NoError(t, conn.Close())
		})
	}
	t.Cleanup(release)
	return conn, release
}

func completeMembersOwnershipTransfer(t *testing.T, conn *sql.Conn, serverID, fromUserID, toUserID string) {
	t.Helper()
	tx, err := conn.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback ownership transfer transaction: %v", rollbackErr)
		}
	})
	_, err = tx.ExecContext(context.Background(),
		`UPDATE server_members SET role = 'member' WHERE server_id = $1 AND user_id = $2`, serverID, fromUserID)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(),
		`UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, serverID, toUserID)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(),
		`UPDATE servers SET owner_id = $2 WHERE id = $1`, serverID, toUserID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// AC-4. Deliberately a runtime harness rather than a grep: an FK-induced or
// conditional lock acquisition is invisible to a static scan, which is exactly
// the class of edge the canonical order exists to keep acyclic.
//
// DeleteServer now takes `SELECT owner_id ... FOR UPDATE` inside its
// transaction. Holding that row from a competing transaction must therefore
// block the handler until the competitor commits. Before the fix the ownership
// read was an autocommit statement and sailed straight past.
func TestDeleteServerBlocksOnTheHeldServerRow(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "lockorderowner")
	serverID := ts.CreateTestServer(t, owner.ID, "LockOrderServer")

	competitor, err := ts.DB.Begin()
	require.NoError(t, err)
	// Registered BEFORE the assertions below, not merely deferred to the happy
	// path. Without it a failing Eventually leaves the row locked and the
	// in-flight DELETE parked on it forever, so the test HANGS instead of
	// failing -- which is barely better than passing wrongly. A second rollback
	// on the success path is a harmless ErrTxDone.
	t.Cleanup(func() { _ = competitor.Rollback() })

	var heldOwner string
	require.NoError(t, competitor.QueryRow(
		`SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&heldOwner))

	result := make(chan int, 1)
	go func() {
		w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
			testhelpers.AuthHeaders(owner.AccessToken))
		result <- w.Code
	}()

	// The handler must be parked on the servers row, not racing ahead of it.
	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := ts.DB.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, 3*time.Second, 10*time.Millisecond,
		"the delete must wait on the held servers row; an autocommit read would not")

	require.NoError(t, competitor.Rollback())

	select {
	case status := <-result:
		require.Equal(t, http.StatusOK, status, "the delete resumes once the row is released")
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not resume after the competing transaction released the row")
	}
}

// AC-5. Concurrent additive and destructive writes against the same server must
// produce zero 40P01 (deadlock detected). The canonical order — users first,
// domain parent second — is what makes the lock graph acyclic; an inverted
// acquisition shows up here and nowhere else.
func TestConcurrentAddAndDeleteProduceNoDeadlock(t *testing.T) {
	const iterations = 12
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "deadlockowner")

	// The handler converts a driver error into a 500 with a fixed message, so the
	// SQLSTATE never reaches the wire. Sample the captured server log instead: a
	// 40P01 surfaces there verbatim.
	logs := ts.CaptureLogs(t)

	for i := 0; i < iterations; i++ {
		serverID := ts.CreateTestServer(t, owner.ID, "DeadlockServer")
		joiner := ts.CreateTestUser(t, fmt.Sprintf("deadlockjoiner%d", i))

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			ts.DoRequest("POST", membersPath(serverID),
				map[string]string{"user_id": joiner.ID},
				testhelpers.AuthHeaders(owner.AccessToken))
		}()
		go func() {
			defer wg.Done()
			ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
				testhelpers.AuthHeaders(owner.AccessToken))
		}()
		wg.Wait()
	}

	require.NotContains(t, logs.String(), "40P01",
		"a deadlock means the canonical lock order was violated on one of these paths")
	require.NotContains(t, logs.String(), "deadlock detected",
		"a deadlock means the canonical lock order was violated on one of these paths")
}

// The owner check in each handler is intentionally a preflight optimization;
// the destructive transaction must revalidate ownership after it acquires the
// same serializer used by ownership transfers. This barrier changes ownership
// only after the request is waiting on that serializer, reproducing the exact
// check-then-use window without sleeps or scheduler assumptions.
func TestRemoveMemberRechecksOwnerAfterPreflight(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickraceowner")
	target := ts.CreateTestUser(t, "kickracetarget")
	serverID := ts.CreateTestServer(t, owner.ID, "KickOwnerRaceServer")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	probe := openMembersLockProbe(t)
	barrierConn, release := holdMembersAdvisoryBarrier(t, probe, key)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- ts.DoRequest("DELETE", memberPath(serverID, target.ID), nil,
			testhelpers.AuthHeaders(owner.AccessToken))
	}()
	dbtest.WaitForAdvisoryLockWaiter(t, probe, key)

	completeMembersOwnershipTransfer(t, barrierConn, serverID, owner.ID, target.ID)
	release()

	var response *httptest.ResponseRecorder
	select {
	case response = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("kick did not finish after the serializer was released")
	}
	require.Equal(t, http.StatusForbidden, response.Code)
	require.JSONEq(t, `{"error":"Cannot remove the server owner"}`, response.Body.String())
	var memberExists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, target.ID).Scan(&memberExists))
	require.True(t, memberExists, "a target who became owner while the request waited must remain a member")
}

func TestBanMemberRechecksOwnerAfterPreflight(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banraceowner")
	target := ts.CreateTestUser(t, "banracetarget")
	serverID := ts.CreateTestServer(t, owner.ID, "BanOwnerRaceServer")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	probe := openMembersLockProbe(t)
	barrierConn, release := holdMembersAdvisoryBarrier(t, probe, key)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- ts.DoRequest("POST", banPath(serverID, target.ID), nil,
			testhelpers.AuthHeaders(owner.AccessToken))
	}()
	dbtest.WaitForAdvisoryLockWaiter(t, probe, key)

	completeMembersOwnershipTransfer(t, barrierConn, serverID, owner.ID, target.ID)
	release()

	var response *httptest.ResponseRecorder
	select {
	case response = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("ban did not finish after the serializer was released")
	}
	require.Equal(t, http.StatusForbidden, response.Code)
	require.JSONEq(t, `{"error":"Cannot ban the server owner"}`, response.Body.String())
	var memberExists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, target.ID).Scan(&memberExists))
	require.True(t, memberExists, "a target who became owner while the request waited must remain a member")
	var banExists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
		serverID, target.ID).Scan(&banExists))
	require.False(t, banExists, "a target who became owner while the request waited must not be banned")
}

// After A transfers ownership to B, A's preflight authorization must not be
// reused for a moderation write against C. A retains the moderation permission
// but has a lower role position than C, so the transaction must evaluate the
// current owner and hierarchy rather than the stale owner snapshot.
func TestFormerOwnerCannotModerateHigherRoleAfterTransfer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   func(string, string) string
		perm   int64
		ban    bool
	}{
		{name: "kick", method: http.MethodDelete, path: memberPath, perm: int64(rbac.PermKick)},
		{name: "ban", method: http.MethodPost, path: banPath, perm: int64(rbac.PermBan), ban: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			a := ts.CreateTestUser(t, "formerowner"+tc.name)
			b := ts.CreateTestUser(t, "newowner"+tc.name)
			c := ts.CreateTestUser(t, "higherrole"+tc.name)
			serverID := ts.CreateTestServer(t, a.ID, "FormerOwnerRace"+tc.name)
			ts.AddMemberToServer(t, serverID, b.ID, "member")
			ts.AddMemberToServer(t, serverID, c.ID, "member")
			aRole := ts.CreateTestRole(t, serverID, "former-owner-moderator", 5, tc.perm)
			cRole := ts.CreateTestRole(t, serverID, "higher-target", 10, 0)
			ts.AssignRoleToUser(t, serverID, a.ID, aRole)
			ts.AssignRoleToUser(t, serverID, c.ID, cRole)

			key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
			require.NoError(t, err)
			probe := openMembersLockProbe(t)
			barrierConn, release := holdMembersAdvisoryBarrier(t, probe, key)
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- ts.DoRequest(tc.method, tc.path(serverID, c.ID), nil,
					testhelpers.AuthHeaders(a.AccessToken))
			}()
			dbtest.WaitForAdvisoryLockWaiter(t, probe, key)
			completeMembersOwnershipTransfer(t, barrierConn, serverID, a.ID, b.ID)
			release()

			var response *httptest.ResponseRecorder
			select {
			case response = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("moderation request did not finish after the serializer was released")
			}
			require.Equal(t, http.StatusForbidden, response.Code)
			wantBody := `{"error":"Cannot remove a member with equal or higher role position"}`
			if tc.ban {
				wantBody = `{"error":"Cannot ban a member with equal or higher role position"}`
			}
			require.JSONEq(t, wantBody, response.Body.String())
			var memberExists bool
			require.NoError(t, ts.DB.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
				serverID, c.ID).Scan(&memberExists))
			require.True(t, memberExists, "the higher-role target must remain a member")
			if tc.ban {
				var banExists bool
				require.NoError(t, ts.DB.QueryRow(
					`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
					serverID, c.ID).Scan(&banExists))
				require.False(t, banExists, "the higher-role target must not be banned")
			}
		})
	}
}

func TestFormerOwnerCannotModerateWithoutPermissionAfterTransfer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   func(string, string) string
		ban    bool
	}{
		{name: "kick", method: http.MethodDelete, path: memberPath},
		{name: "ban", method: http.MethodPost, path: banPath, ban: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			a := ts.CreateTestUser(t, "formerowner-no-perm"+tc.name)
			b := ts.CreateTestUser(t, "newowner-no-perm"+tc.name)
			c := ts.CreateTestUser(t, "target-no-perm"+tc.name)
			serverID := ts.CreateTestServer(t, a.ID, "FormerOwnerNoPermission"+tc.name)
			ts.AddMemberToServer(t, serverID, b.ID, "member")
			ts.AddMemberToServer(t, serverID, c.ID, "member")

			key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
			require.NoError(t, err)
			probe := openMembersLockProbe(t)
			barrierConn, release := holdMembersAdvisoryBarrier(t, probe, key)
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- ts.DoRequest(tc.method, tc.path(serverID, c.ID), nil,
					testhelpers.AuthHeaders(a.AccessToken))
			}()
			dbtest.WaitForAdvisoryLockWaiter(t, probe, key)
			completeMembersOwnershipTransfer(t, barrierConn, serverID, a.ID, b.ID)
			release()

			var response *httptest.ResponseRecorder
			select {
			case response = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("moderation request did not finish after the serializer was released")
			}
			require.Equal(t, http.StatusForbidden, response.Code)
			require.JSONEq(t, `{"error":"insufficient permissions"}`, response.Body.String())
			var memberExists bool
			require.NoError(t, ts.DB.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
				serverID, c.ID).Scan(&memberExists))
			require.True(t, memberExists, "the target must remain a member")
			if tc.ban {
				var banExists bool
				require.NoError(t, ts.DB.QueryRow(
					`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
					serverID, c.ID).Scan(&banExists))
				require.False(t, banExists, "the target must not be banned")
			}
		})
	}
}
