package stmthook_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func admissionFixture(t *testing.T) (*testhelpers.TestServer, *stmthook.Hook, *sql.DB, loginfamily.AccessIdentity) {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ts := &testhelpers.TestServer{DB: db}
	user := ts.CreateTestUser(t, "admissionhook")
	claims, err := auth.ValidateAccessToken(user.AccessToken, testhelpers.TestJWTSecret)
	require.NoError(t, err)
	identity := loginfamily.AccessIdentity{UserID: uuid.MustParse(claims.UserID), SID: uuid.MustParse(claims.SessionID), CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time}
	hook, wrapped := stmthook.Open(t)
	return ts, hook, wrapped, identity
}

// Calling back before COMMIT, or while holding Hook.mu, breaks this control.
func TestAdmissionCommit_DefiniteCommitOneShotAndExplicitRearm(t *testing.T) {
	ts, hook, wrapped, identity := admissionFixture(t)
	wrapped.SetMaxOpenConns(1)
	var pid int
	require.NoError(t, wrapped.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	callback := func() error {
		// Report must be callable from the callback, without a wrapper mutex held.
		if !hook.AdmissionReport().Running {
			return errors.New("callback not reported running")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var state string
		if err := ts.DB.QueryRowContext(ctx, `SELECT state FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&state); err != nil {
			return err
		}
		if state != "idle" {
			return errors.New("auth transaction has not committed")
		}
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		require.NoError(t, hook.ArmAdmission(identity.UserID.String(), identity.SID.String(), callback))
		_, err := loginfamily.ResolveVerifiedAccess(context.Background(), wrapped, identity)
		require.NoError(t, err)
		report := hook.AdmissionReport()
		require.Equal(t, 1, report.Fired)
		require.True(t, report.Completed)
		require.False(t, report.Running)
		require.False(t, report.Pending)
		require.NoError(t, report.CallbackErr)
		assert.Equal(t, identity.UserID.String(), report.UserID)
		assert.Equal(t, identity.SID.String(), report.SID)
		_, err = loginfamily.ResolveVerifiedAccess(context.Background(), wrapped, identity)
		require.NoError(t, err)
		assert.Equal(t, 1, hook.AdmissionReport().Fired, "must rearm explicitly")
	}
}

// An unrelated commit, identity, isolation level, or incomplete/rolled-back
// sequence must not consume the arm intended for the real HTTP admission.
func TestAdmissionCommit_UnmatchedTransactionsCannotConsumeArm(t *testing.T) {
	for _, scenario := range []string{"unrelated commit", "partial sequence", "wrong SID", "wrong user", "read write", "wrong isolation", "rollback", "failed commit"} {
		t.Run(scenario, func(t *testing.T) {
			ts, hook, wrapped, identity := admissionFixture(t)
			require.NoError(t, hook.ArmAdmission(identity.UserID.String(), identity.SID.String(), func() error { return nil }))
			opts := &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true}
			if scenario == "read write" {
				opts.ReadOnly = false
			}
			if scenario == "wrong isolation" {
				opts.Isolation = sql.LevelSerializable
			}
			tx, err := wrapped.BeginTx(context.Background(), opts)
			require.NoError(t, err)
			t.Cleanup(func() {
				err := tx.Rollback()
				if err != nil && !errors.Is(err, sql.ErrTxDone) {
					t.Errorf("rollback: %v", err)
				}
			})
			if scenario == "unrelated commit" {
				var one int
				require.NoError(t, tx.QueryRow(`SELECT 1`).Scan(&one))
			} else {
				candidate := identity
				if scenario == "wrong SID" {
					candidate.SID = uuid.New()
				}
				if scenario == "wrong user" {
					candidate.UserID = uuid.New()
				}
				var family uuid.UUID
				var expiry time.Time
				err = tx.QueryRow(`SELECT family_id, access_expires_at FROM login_family_sids WHERE user_id=$1 AND sid=$2`, candidate.UserID, candidate.SID).Scan(&family, &expiry)
				if scenario == "wrong SID" || scenario == "wrong user" {
					require.ErrorIs(t, err, sql.ErrNoRows)
				} else if scenario != "partial sequence" {
					require.NoError(t, err)
					var userID, familyID uuid.UUID
					var epoch string
					var terminal sql.NullTime
					var operation, generation uuid.NullUUID
					require.NoError(t, tx.QueryRow(`SELECT user_id, family_id, credential_epoch, terminal_at, auth_operation_id, epoch_generation FROM login_families WHERE user_id=$1 AND family_id=$2`, identity.UserID, family).Scan(&userID, &familyID, &epoch, &terminal, &operation, &generation))
					var now time.Time
					require.NoError(t, tx.QueryRow(`SELECT clock_timestamp()`).Scan(&now))
					require.NoError(t, tx.QueryRow(`SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`, identity.UserID).Scan(&epoch))
				}
			}
			if scenario == "rollback" {
				require.NoError(t, tx.Rollback())
			} else {
				if scenario == "failed commit" {
					var n int
					require.Error(t, tx.QueryRow(`SELECT 1 / 0`).Scan(&n))
					require.Error(t, tx.Commit())
				} else {
					require.NoError(t, tx.Commit())
				}
			}
			assert.True(t, hook.AdmissionReport().Pending)
			assert.Zero(t, hook.AdmissionReport().Fired)
			// A real subsequent resolution must still consume this exact same arm.
			_, err = loginfamily.ResolveVerifiedAccess(context.Background(), wrapped, identity)
			require.NoError(t, err)
			require.Equal(t, 1, hook.AdmissionReport().Fired)
			require.NoError(t, hook.AdmissionReport().CallbackErr)
			var users int
			require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM users WHERE id=$1`, identity.UserID).Scan(&users))
			assert.Equal(t, 1, users)
		})
	}
}

func TestAdmissionCommit_CallbackFaultDoesNotManufactureAuthFailure(t *testing.T) {
	_, hook, wrapped, identity := admissionFixture(t)
	fault := errors.New("rotation fixture failed")
	require.NoError(t, hook.ArmAdmission(identity.UserID.String(), identity.SID.String(), func() error { return fault }))
	_, err := loginfamily.ResolveVerifiedAccess(context.Background(), wrapped, identity)
	require.NoError(t, err, "auth commit succeeded; callback failure belongs in fixture report")
	report := hook.AdmissionReport()
	assert.True(t, report.Completed)
	assert.ErrorIs(t, report.CallbackErr, fault)
	assert.Equal(t, 1, report.Fired)
}

func TestAdmissionCommit_ConflictingArmsAreRejected(t *testing.T) {
	_, hook, _, identity := admissionFixture(t)
	callback := func() error { return nil }
	require.Error(t, hook.ArmAdmission("", identity.SID.String(), callback))
	require.Error(t, hook.ArmAdmission(identity.UserID.String(), identity.SID.String(), nil))
	hook.ArmCommit(false, stmthook.ErrInjected)
	require.Error(t, hook.ArmAdmission(identity.UserID.String(), identity.SID.String(), callback))
	// Disarm the legacy fault using its existing API.
	hook.ArmCommit(false, nil)
	require.NoError(t, hook.ArmAdmission(identity.UserID.String(), identity.SID.String(), callback))
	require.Error(t, hook.ArmAdmission(identity.UserID.String(), identity.SID.String(), callback))
	assert.Panics(t, func() { hook.ArmCommit(false, stmthook.ErrInjected) })
}
