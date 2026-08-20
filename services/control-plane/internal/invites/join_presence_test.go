package invites_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// Concurrency regression for the join path: no 5xx, exactly one membership row.
//
// Scope, stated honestly rather than overclaimed. checkBanAndMembership does a
// SELECT EXISTS on server_members and returns 409 when the row is there, so a
// SEQUENTIAL retry never reaches the insert — a sequential test of the ON
// CONFLICT clause passes with the clause removed and proves nothing. The clause
// guards the RACE instead: that existence check is a read-then-write with no
// lock, so two concurrent joins can both pass it and both insert, and without
// ON CONFLICT the loser takes a duplicate-key 500 on a legitimate request.
//
// This test does NOT reliably reproduce that window — it still passes with the
// clause removed, because the requests do not interleave inside it often enough.
// It is kept as a genuine concurrency regression (a handler that deadlocked or
// double-inserted would fail here), NOT as proof of the ON CONFLICT guard. The
// guard is required by the spec's 3.3 and justified by the unlocked
// read-then-write above; treat it as defense in depth with no automated
// falsification at this layer.
func TestConcurrentJoinsDoNotCollide(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinraceowner")
	joiner := ts.CreateTestUser(t, "joinracejoiner")
	serverID := ts.CreateTestServer(t, owner.ID, "JoinRaceServer")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	const attempts = 6
	codes := make(chan int, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := ts.DoRequest("POST", "/api/v1/invites/join",
				map[string]string{"code": code}, testhelpers.AuthHeaders(joiner.AccessToken))
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)

	for status := range codes {
		require.Less(t, status, 500,
			"a concurrent duplicate join must not 500; ON CONFLICT is what prevents it")
	}

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, joiner.ID).Scan(&rows))
	require.Equal(t, 1, rows, "exactly one membership row survives the race")
}

// The rejection paths used to write their response inline inside the
// transaction. Now they ride out of the closure as a typed joinRejection, so
// they must still produce their own status rather than collapsing to 500.
func TestJoinServerRejectionsKeepTheirStatus(t *testing.T) {
	ts := setupTS(t)
	joiner := ts.CreateTestUser(t, "joinrejjoiner")

	w := ts.DoRequest("POST", "/api/v1/invites/join",
		map[string]string{"code": "ZZZZZZZZ"}, testhelpers.AuthHeaders(joiner.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code,
		"an unknown invite code is 404, not a generic 500")
}
