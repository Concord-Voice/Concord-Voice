package members_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

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
