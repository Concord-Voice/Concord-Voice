//go:build integration

package members_test

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

const flipToggleSQL = `UPDATE servers SET enforce_mfa_dangerous_actions = true WHERE id = $1`

// startToggleFlip turns enforcement on from its own transaction, with its own
// lock_timeout, and reports how it ended.
func startToggleFlip(db *sql.DB, serverID string) <-chan error {
	done := make(chan error, 1)
	go func() {
		tx, err := db.Begin()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SET LOCAL lock_timeout = '10s'`); err != nil {
			done <- err
			return
		}
		if _, err := tx.Exec(flipToggleSQL, serverID); err != nil {
			done <- err
			return
		}
		done <- tx.Commit()
	}()
	return done
}

// waitForFlipLockWait polls until the toggle's UPDATE is parked on a row lock.
func waitForFlipLockWait(db *sql.DB, done <-chan error) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return fmt.Errorf("the toggle did not wait for the gate transaction: %v", err)
		default:
		}
		var waiting bool
		if err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%SET enforce_mfa_dangerous_actions = true%'
		)`).Scan(&waiting); err != nil {
			return err
		}
		if waiting {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("the toggle's UPDATE never waited on a lock")
}

// TestModerationGate_FlipWaitsForTheGate holds each gated transaction open at
// its first write, after the gate read the flag, and proves the toggle's
// UPDATE waits on the gate's servers lock: the flag cannot change between the
// gate's read and the action's commit.
func TestModerationGate_FlipWaitsForTheGate(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	firstWrite := map[string]string{
		"ban":             `INSERT INTO server_bans`,
		"kick with purge": `DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`,
	}
	for _, route := range gateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newGateFixture(t, ts)
			var flip <-chan error
			hook.Arm([]string{firstWrite[route.name]}, func() error {
				flip = startToggleFlip(ts.DB, f.serverID)
				return waitForFlipLockWait(ts.DB, flip)
			}, nil)

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.mod.AccessToken))

			seen, betweenErr := hook.Report()
			require.Equal(t, 1, seen)
			require.NoError(t, betweenErr)
			require.Equal(t, http.StatusOK, w.Code, "the gate read the flag OFF and the action committed: %s", w.Body.String())
			assert.True(t, route.done(t, ts.DB, f))
			select {
			case err := <-flip:
				require.NoError(t, err, "the toggle commits once the gate transaction ends")
			case <-time.After(10 * time.Second):
				t.Fatal("the toggle never resumed")
			}
			var enforcing bool
			require.NoError(t, ts.DB.QueryRow(`SELECT enforce_mfa_dangerous_actions FROM servers WHERE id = $1`, f.serverID).Scan(&enforcing))
			assert.True(t, enforcing)
		})
	}
}

// I-UNGATED, statement by statement: a kick that purges nothing never issues
// the gate's statements; it keeps the plain owner lock.
func TestModerationGate_KickWithoutPurgeTakesNoGateLock(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	f := newGateFixture(t, ts)
	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	enrollGateWebAuthn(t, ts.DB, f.mod.ID)

	hook.Arm([]string{txGateLock}, nil, nil)
	w := ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+f.serverID+"/members/"+f.target.ID, nil,
		testhelpers.AuthHeaders(f.mod.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	seen, _ := hook.Report()
	assert.Zero(t, seen, "an ungated kick must not run LockGateTx")

	hook.Arm([]string{txOwnerLock}, nil, nil)
	other := ts.CreateTestUser(t, "gtu"+f.serverID[:8])
	ts.AddMemberToServer(t, f.serverID, other.ID, "member")
	w = ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+f.serverID+"/members/"+other.ID, nil,
		testhelpers.AuthHeaders(f.mod.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	seen, _ = hook.Report()
	assert.Equal(t, 1, seen, "it keeps the plain owner lock")
}

// moderationFirstWrite is each gated moderation route's first write, after the
// gate's lock and its confirmation.
var moderationFirstWrite = map[string]string{
	"ban":             `INSERT INTO server_bans`,
	"kick with purge": `DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`,
}

// A lock timeout or deadlock inside a GATED moderation transaction, a ban or a
// kick that purges, answers the gate's retryable lock_conflict 503 with
// Retry-After: 1, as on every other gated route (#3454 A-12.3). It used to fall
// through to the presence classifier's 500 (Codex review of #3454). A plain
// kick is ungated and keeps its 500 byte-identical (I-UNGATED).
// Kills: writeGatedLockConflict dropped from either classifier (that gated
// route answers 500); the removal classifier ignoring its gated flag (the
// plain kick answers lock_conflict).
func TestModerationGate_LockConflictIsRetryableOnGatedRoutesOnly(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	lockTimeout := &pq.Error{Code: "55P03", Message: "canceling statement due to lock timeout"}
	for _, route := range gateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newGateFixture(t, ts)
			hook.Arm([]string{moderationFirstWrite[route.name]}, nil, lockTimeout)

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.mod.AccessToken))

			seen, _ := hook.Report()
			require.Equal(t, 1, seen)
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			assert.JSONEq(t, `{"error":"The server is busy. Try again.","lock_conflict":true}`, w.Body.String())
			assert.Equal(t, "1", w.Header().Get("Retry-After"))
			assert.False(t, route.done(t, ts.DB, f), "a refused transaction changes nothing")
		})
	}
	t.Run("plain kick keeps its 500", func(t *testing.T) {
		f := newGateFixture(t, ts)
		hook.Arm([]string{moderationFirstWrite["kick with purge"]}, nil, lockTimeout)

		w := ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+f.serverID+"/members/"+f.target.ID, nil,
			testhelpers.AuthHeaders(f.mod.AccessToken))

		seen, _ := hook.Report()
		require.Equal(t, 1, seen)
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "lock_conflict")
		assert.Empty(t, w.Header().Get("Retry-After"))
	})
}
