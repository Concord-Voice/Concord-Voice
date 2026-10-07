//go:build integration

package channels_test

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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

const (
	chanGateFlipSQL = `UPDATE servers SET enforce_mfa_dangerous_actions = true WHERE id = $1`
	// chanGateLockSQL matches only LockGateTx's servers read in these
	// transactions.
	chanGateLockSQL = `enforce_mfa_dangerous_actions, current_setting('transaction_isolation')`
)

// startChanGateFlip turns enforcement on from its own transaction, with its
// own lock_timeout, and reports how it ended.
func startChanGateFlip(db *sql.DB, serverID string) <-chan error {
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
		if _, err := tx.Exec(chanGateFlipSQL, serverID); err != nil {
			done <- err
			return
		}
		done <- tx.Commit()
	}()
	return done
}

// waitForChanGateFlipLockWait polls until the toggle's UPDATE is parked on a
// row lock.
func waitForChanGateFlipLockWait(db *sql.DB, done <-chan error) error {
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

// TestChannelGate_FlipWaitsForTheGate holds each gated transaction open at its
// first write, after the gate read the flag, and proves the toggle's UPDATE
// waits on that transaction's servers lock: the flag cannot change between the
// gate's read and the action's commit. For the expiration route that lock is
// the gate's own FOR SHARE; nothing else in that transaction touches servers.
func TestChannelGate_FlipWaitsForTheGate(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	firstWrite := map[string]string{
		"delete channel":     `DELETE FROM channels WHERE id = $1 AND server_id = $2`,
		"shorten expiration": `SET expiration_window_seconds = $2`,
	}
	for _, route := range chanGateRoutes() {
		t.Run(route.name, func(t *testing.T) {
			f := newChanGateFixture(t, ts)
			var flip <-chan error
			hook.Arm([]string{chanGateLockSQL, firstWrite[route.name]}, func() error {
				flip = startChanGateFlip(ts.DB, f.serverID)
				return waitForChanGateFlipLockWait(ts.DB, flip)
			}, nil)

			w := ts.DoRequest(route.method, route.path(f), route.body(""), testhelpers.AuthHeaders(f.mod.AccessToken))

			seen, betweenErr := hook.Report()
			require.Equal(t, 2, seen)
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

// The expiration route has two 503s and they must stay distinct (§7): the
// ambiguous commit answers with the candidate policy and no error key, and
// settles nothing; the gate's lock conflict answers lock_conflict with
// Retry-After and writes nothing. A lock conflict PostgreSQL reports at COMMIT
// is the second kind (Codex review of #3454): the COMMIT was refused, so the
// transaction rolled back, and the client can retry it like the same conflict
// on any earlier statement.
// Kills: the commit-time IsLockConflict branch removed (the lock conflict at
// commit answers the ambiguous policy-shaped 503).
func TestChannelExpirationGate_503RoutingStaysDistinct(t *testing.T) {
	t.Run("ambiguous commit after a verified shortening", func(t *testing.T) {
		ts := setupTS(t)
		f := newChanGateFixture(t, ts)
		testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
		enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)
		installChannelExpirationCommitFailure(t, ts.DB, f.channelID)
		token := mintChanGateToken(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten)

		w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+f.channelID+"/expiration", shortenBody(token),
			testhelpers.AuthHeaders(f.mod.AccessToken))
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		var body map[string]any
		testhelpers.ParseJSON(t, w, &body)
		assert.NotContains(t, body, "error")
		assert.NotContains(t, body, "lock_conflict")
		assert.EqualValues(t, 3600, body["window_seconds"])
		assert.Empty(t, w.Header().Get("Retry-After"))
		assert.True(t, chanGateTokenLive(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten), "the failed commit restores the token")
		assert.Equal(t, "1", chanGateBudget(t, ts, f.mod.ID), "an uncommitted verification clears nothing")
	})

	t.Run("lock conflict at the gate", func(t *testing.T) {
		hook, hookedDB := stmthook.Open(t)
		ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
		f := newChanGateFixture(t, ts)
		hook.Arm([]string{chanGateLockSQL}, nil, &pq.Error{Code: "55P03", Message: "canceling statement due to lock timeout"})

		w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+f.channelID+"/expiration", shortenBody(""),
			testhelpers.AuthHeaders(f.mod.AccessToken))
		seen, _ := hook.Report()
		require.Equal(t, 1, seen)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.JSONEq(t, `{"error":"The server is busy. Try again.","lock_conflict":true}`, w.Body.String())
		assert.Equal(t, "1", w.Header().Get("Retry-After"))
		assertChanGateNothingWritten(t, ts.DB, f)
	})

	for _, sqlstate := range []string{"40P01", "55P03"} {
		t.Run("lock conflict at commit "+sqlstate, func(t *testing.T) {
			ts := setupTS(t)
			f := newChanGateFixture(t, ts)
			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)
			installChannelExpirationCommitLockConflict(t, ts.DB, f.channelID, sqlstate)
			token := mintChanGateToken(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten)

			w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+f.channelID+"/expiration", shortenBody(token),
				testhelpers.AuthHeaders(f.mod.AccessToken))
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			assert.JSONEq(t, `{"error":"The server is busy. Try again.","lock_conflict":true}`, w.Body.String())
			assert.Equal(t, "1", w.Header().Get("Retry-After"))
			assert.Zero(t, chanGateWindow(t, ts.DB, f.channelID), "the refused COMMIT wrote nothing")
			assert.True(t, chanGateTokenLive(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten),
				"the refused COMMIT restores the token")
			assert.Equal(t, "1", chanGateBudget(t, ts, f.mod.ID), "an uncommitted verification clears nothing")
		})
	}
}

// installChannelExpirationCommitLockConflict makes the channel's policy UPDATE
// fail AT COMMIT with sqlstate, through a deferred constraint trigger, the way
// deferred work that waits on a lock would: PostgreSQL refuses the COMMIT and
// rolls the transaction back.
func installChannelExpirationCommitLockConflict(t *testing.T, db *sql.DB, channelID, sqlstate string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := db.Exec(`
DROP TRIGGER IF EXISTS expiration_commit_lock_trigger ON channels;
DROP FUNCTION IF EXISTS expiration_commit_lock_fail();
DROP TABLE IF EXISTS expiration_commit_lock_control;`)
		assert.NoError(t, err)
	})
	_, err := db.Exec(`
CREATE TABLE expiration_commit_lock_control (scope_id UUID PRIMARY KEY, sqlstate TEXT NOT NULL);
CREATE OR REPLACE FUNCTION expiration_commit_lock_fail() RETURNS trigger AS $$
DECLARE code TEXT;
BEGIN
  SELECT c.sqlstate INTO code FROM expiration_commit_lock_control c WHERE c.scope_id = NEW.id;
  IF code IS NOT NULL THEN
    RAISE EXCEPTION 'expiration commit lock conflict' USING ERRCODE = code;
  END IF;
  RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE CONSTRAINT TRIGGER expiration_commit_lock_trigger
AFTER UPDATE ON channels DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION expiration_commit_lock_fail()`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO expiration_commit_lock_control (scope_id, sqlstate) VALUES ($1, $2)`, channelID, sqlstate)
	require.NoError(t, err)
}

// TestChannelExpiration_GuardReadFailureIsTheRoutesOwn500: a failed
// credential-epoch read inside the expiration transaction answers this
// route's 500, not CreateChannel's. It used to share CreateChannel's
// responder and said "Failed to create channel" on a PATCH that creates
// nothing.
//
// Mutant killed: passing errMsgFailedCreateChannel at the expiration call site.
func TestChannelExpiration_GuardReadFailureIsTheRoutesOwn500(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	f := newChanGateFixture(t, ts)
	hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`}, f.mod.ID, nil,
		errors.New("injected guard read failure"))

	w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+f.channelID+"/expiration", shortenBody(""),
		testhelpers.AuthHeaders(f.mod.AccessToken))
	seen, _ := hook.Report()
	require.Equal(t, 1, seen)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"Failed to update channel"}`, w.Body.String())
	assertChanGateNothingWritten(t, ts.DB, f)
}
