package mfaenforce_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// Statements a lock probe runs against the gated servers row. flipSQL is the
// toggle's own write shape: an UPDATE of a non-key column, which PostgreSQL
// executes under FOR NO KEY UPDATE.
const (
	flipSQL                 = `UPDATE servers SET enforce_mfa_dangerous_actions = NOT enforce_mfa_dangerous_actions WHERE id = $1`
	serverForKeyShareSQL    = `SELECT 1 FROM servers WHERE id = $1 FOR KEY SHARE`
	serverForShareSQL       = `SELECT 1 FROM servers WHERE id = $1 FOR SHARE`
	serverForNoKeyUpdateSQL = `SELECT 1 FROM servers WHERE id = $1 FOR NO KEY UPDATE`
)

func TestLockGateTx_ReturnsTheFlagTheOwnerAndTheP1Subject(t *testing.T) {
	db := gateTestDB(t)
	_, ring := newVerifier(t, db)
	ctx := context.Background()

	cases := []struct {
		name        string
		enforcing   bool
		enroll      func(t *testing.T, userID string)
		wantMethods []string
	}{
		{
			name:        "unenrolled actor, server not enforcing",
			enroll:      func(*testing.T, string) {},
			wantMethods: []string{},
		},
		{
			name:        "TOTP-enrolled actor, enforcing server",
			enforcing:   true,
			enroll:      func(t *testing.T, userID string) { enrollTOTP(t, db, ring, userID) },
			wantMethods: []string{"totp"},
		},
		{
			name:        "WebAuthn-enrolled actor, enforcing server",
			enforcing:   true,
			enroll:      func(t *testing.T, userID string) { enrollWebAuthn(t, db, userID) },
			wantMethods: []string{"webauthn"},
		},
		{
			// users.mfa_methods lists email and SMS, but neither has an inline
			// verifier, so P1 counts the actor as unenrolled.
			name:      "email and SMS alone count as unenrolled",
			enforcing: true,
			enroll: func(t *testing.T, userID string) {
				_, err := db.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{email,sms}' WHERE id = $1`, userID)
				require.NoError(t, err)
			},
			wantMethods: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := createUser(t, db)
			actor := createUser(t, db)
			tc.enroll(t, actor)
			serverID := createServer(t, db, owner, tc.enforcing)
			tx := beginTx(t, db, sql.LevelReadCommitted)

			g, err := mfaenforce.LockGateTx(ctx, tx, serverID, actor, stepup.LockForShare, mfaenforce.ServerForShare, "")

			// NoError, not Nil on a typed value: it fails on a typed-nil
			// *stepup.Error inside the interface, which is the bug the
			// "return only when non-nil" rule exists to prevent.
			require.NoError(t, err)
			require.Equal(t, tc.enforcing, g.Enforcing)
			require.Equal(t, owner, g.OwnerID, "the owner, not the actor")
			require.Equal(t, len(tc.wantMethods) > 0, g.Subject.MFAEnabled)
			require.Equal(t, tc.wantMethods, g.Subject.MFAMethods)
		})
	}
}

func TestLockGateTx_ServerNotFound(t *testing.T) {
	db := gateTestDB(t)
	tx := beginTx(t, db, sql.LevelReadCommitted)

	_, err := mfaenforce.LockGateTx(context.Background(), tx, uuid.New().String(), createUser(t, db),
		stepup.LockForShare, mfaenforce.ServerForShare, "")

	require.ErrorIs(t, err, mfaenforce.ErrServerNotFound)
	var se *stepup.Error
	require.False(t, errors.As(err, &se), "a missing server is not a step-up refusal")
}

// The P1 read in LockSubjectTx sees a factor change that committed while it
// waited only under READ COMMITTED. Any other level is a caller bug and must be
// refused, not quietly served a stale verdict.
func TestLockGateTx_RefusesAnyIsolationButReadCommitted(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	serverID := createServer(t, db, owner, true)

	for _, level := range []sql.IsolationLevel{sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(level.String(), func(t *testing.T) {
			tx := beginTx(t, db, level)

			_, err := mfaenforce.LockGateTx(context.Background(), tx, serverID, owner,
				stepup.LockForShare, mfaenforce.ServerForShare, "")

			require.Error(t, err)
			require.Contains(t, err.Error(), "requires READ COMMITTED")
			require.NotErrorIs(t, err, mfaenforce.ErrServerNotFound)
		})
	}
}

// An unknown lock is refused before the first statement, so the refusal can
// never leave the actor's users row locked. A row lock needs a transaction id,
// so "no id assigned" proves neither of LockGateTx's locking reads ran; the
// actor exists, so LockSubjectTx would have succeeded had it been reached.
func TestLockGateTx_RefusesAnUnknownServerLockBeforeAnyStatement(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	serverID := createServer(t, db, owner, true)

	for _, lock := range []mfaenforce.ServerLock{0, 99} {
		t.Run(fmt.Sprintf("ServerLock(%d)", lock), func(t *testing.T) {
			ctx := context.Background()
			tx := beginTx(t, db, sql.LevelReadCommitted)

			_, err := mfaenforce.LockGateTx(ctx, tx, serverID, owner, stepup.LockForShare, lock, "")

			require.Error(t, err)
			require.Contains(t, err.Error(), "unknown server lock")
			var noXID bool
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT txid_current_if_assigned() IS NULL`).Scan(&noXID))
			require.True(t, noXID, "a row lock was taken before the ServerLock was validated")
		})
	}
}

func TestLockGateTx_EpochMismatchIsTheTagged401(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	serverID := createServer(t, db, owner, true)
	_, err := db.Exec(`UPDATE users SET credential_epoch = 'server-epoch' WHERE id = $1`, owner)
	require.NoError(t, err)
	tx := beginTx(t, db, sql.LevelReadCommitted)

	_, err = mfaenforce.LockGateTx(context.Background(), tx, serverID, owner,
		stepup.LockForShare, mfaenforce.ServerForShare, "stale-epoch")

	var se *stepup.Error
	require.True(t, errors.As(err, &se), "want a *stepup.Error, got %v", err)
	require.Equal(t, http.StatusUnauthorized, se.Status)
	require.True(t, se.EpochMismatch())
}

// Each ServerLock must take exactly the row lock it names. The flip probe is
// the requirement (the flag cannot change under the gate); the other probes
// tell the three locks apart, so a swapped mapping fails here rather than as a
// production deadlock.
func TestLockGateTx_TakesTheRequestedServerLock(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	serverID := createServer(t, db, owner, true)

	cases := []struct {
		lock   mfaenforce.ServerLock
		name   string
		blocks []string
		passes []string
	}{
		{mfaenforce.ServerForShare, "ServerForShare",
			[]string{flipSQL, serverForNoKeyUpdateSQL}, []string{serverForShareSQL, serverForKeyShareSQL}},
		{mfaenforce.ServerForNoKeyUpdate, "ServerForNoKeyUpdate",
			[]string{flipSQL, serverForShareSQL}, []string{serverForKeyShareSQL}},
		{mfaenforce.ServerForUpdate, "ServerForUpdate",
			[]string{flipSQL, serverForKeyShareSQL}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := beginTx(t, db, sql.LevelReadCommitted)
			_, err := mfaenforce.LockGateTx(context.Background(), tx, serverID, owner,
				stepup.LockForShare, tc.lock, "")
			require.NoError(t, err)

			for _, stmt := range tc.blocks {
				require.True(t, probeBlocks(t, db, stmt, serverID), "should block: %s", stmt)
			}
			for _, stmt := range tc.passes {
				require.False(t, probeBlocks(t, db, stmt, serverID), "should pass: %s", stmt)
			}
		})
	}

	// The control arm, and the reason FOR KEY SHARE is not offered: held at
	// KEY SHARE the flag can still flip, so a gate there would read a value
	// that can change before it commits. It also proves the flip probe can
	// pass, so its "blocks" results above are not an artefact of the probe.
	t.Run("FOR KEY SHARE does not hold the flag", func(t *testing.T) {
		tx := beginTx(t, db, sql.LevelReadCommitted)
		_, err := tx.Exec(serverForKeyShareSQL, serverID)
		require.NoError(t, err)

		require.False(t, probeBlocks(t, db, flipSQL, serverID))
	})
}

// userLock reaches stepup.LockSubjectTx unchanged. FOR SHARE and FOR NO KEY
// UPDATE are told apart by a FOR SHARE probe, which only the stronger blocks.
func TestLockGateTx_TakesTheRequestedUsersLock(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	serverID := createServer(t, db, owner, true)
	const usersForShare = `SELECT 1 FROM users WHERE id = $1 FOR SHARE`
	const usersForNoKeyUpdate = `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`

	cases := []struct {
		lock            stepup.Lock
		name            string
		shareProbeBlock bool
	}{
		{stepup.LockForShare, "LockForShare", false},
		{stepup.LockForNoKeyUpdate, "LockForNoKeyUpdate", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := beginTx(t, db, sql.LevelReadCommitted)
			_, err := mfaenforce.LockGateTx(context.Background(), tx, serverID, owner, tc.lock, mfaenforce.ServerForShare, "")
			require.NoError(t, err)

			require.True(t, probeBlocks(t, db, usersForNoKeyUpdate, owner), "a destructive reset must wait on the gate")
			require.Equal(t, tc.shareProbeBlock, probeBlocks(t, db, usersForShare, owner))
		})
	}
}

// A lock timeout at either row is a lock conflict. At the users row it arrives
// wrapped in a 500 *stepup.Error, which is why the package comment has callers
// test IsLockConflict before errors.As.
func TestLockGateTx_ALockTimeoutIsALockConflictAtEitherRow(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	serverID := createServer(t, db, owner, true)

	cases := []struct {
		name          string
		blockerSQL    string
		blockerArg    string
		wantStepupErr bool
	}{
		{"users row", `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, owner, true},
		{"servers row", `SELECT 1 FROM servers WHERE id = $1 FOR UPDATE`, serverID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			blocker := beginTx(t, db, sql.LevelReadCommitted)
			_, err := blocker.ExecContext(ctx, tc.blockerSQL, tc.blockerArg)
			require.NoError(t, err)
			tx := beginTx(t, db, sql.LevelReadCommitted)
			_, err = tx.ExecContext(ctx, probeLockTimeout)
			require.NoError(t, err)

			_, err = mfaenforce.LockGateTx(ctx, tx, serverID, owner, stepup.LockForShare, mfaenforce.ServerForShare, "")

			require.True(t, mfaenforce.IsLockConflict(err), "want a lock conflict, got %v", err)
			var se *stepup.Error
			require.Equal(t, tc.wantStepupErr, errors.As(err, &se))
			if tc.wantStepupErr {
				require.Equal(t, http.StatusInternalServerError, se.Status)
			}
		})
	}
}
