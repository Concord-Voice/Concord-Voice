package stepup

// Database-backed tests for the P1 subject readers. They need the real schema:
// the predicate's whole point is which factor-table rows count, and the
// lock-then-read ordering is a property of PostgreSQL's READ COMMITTED
// snapshots that no fake can model.

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func subjectTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbtest.SetupTestDB(t)
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// subjectTestUser inserts a user. The hash is an opaque stand-in: these tests
// read it back, they never verify it.
func subjectTestUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := uuid.New().String()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, 'stored-hash-fixture', true, true)`, id, id+"@stepup-subject.test", "su"+id[:8])
	require.NoError(t, err)
	return id
}

func subjectTestTOTP(t *testing.T, db *sql.DB, userID string, enabled, confirmed bool) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, '\x00', '\x00', 1, $2, $3)`, userID, enabled, confirmed)
	require.NoError(t, err)
}

func subjectTestWebAuthn(t *testing.T, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, userID string) {
	t.Helper()
	_, err := q.ExecContext(context.Background(), `INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, NOW())`, uuid.New().String(), userID, []byte("cred-"+userID))
	require.NoError(t, err)
}

func TestLoadSubject_P1FromTheFactorTables(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()

	t.Run("password only, stale flags listing email: no MFA leg", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		_, err := db.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{email,sms}' WHERE id = $1`, userID)
		require.NoError(t, err)

		s, e := LoadSubject(ctx, db, userID)

		require.Nil(t, e)
		require.Equal(t, "stored-hash-fixture", s.PasswordHash)
		require.False(t, s.MFAEnabled)
		require.NotNil(t, s.MFAMethods)
		require.Empty(t, s.MFAMethods)
	})

	t.Run("pending TOTP enrollment verifies nothing", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestTOTP(t, db, userID, true, false)

		s, e := LoadSubject(ctx, db, userID)

		require.Nil(t, e)
		require.False(t, s.MFAEnabled)
	})

	t.Run("confirmed TOTP despite flags saying off, plus a key", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestTOTP(t, db, userID, true, true)
		subjectTestWebAuthn(t, db, userID)

		s, e := LoadSubject(ctx, db, userID)

		require.Nil(t, e)
		require.True(t, s.MFAEnabled)
		require.Equal(t, []string{"totp", "webauthn"}, s.MFAMethods)
	})

	t.Run("deleted account is 401, not 500", func(t *testing.T) {
		_, e := LoadSubject(ctx, db, uuid.New().String())

		require.NotNil(t, e)
		require.Equal(t, http.StatusUnauthorized, e.Status)
		require.Equal(t, ErrMsgSessionNoLongerValid, e.Body["error"])
		require.Nil(t, e.Cause)
	})

	t.Run("read failure is a 500 with a cause, never 'no MFA'", func(t *testing.T) {
		closed := subjectTestDB(t)
		require.NoError(t, closed.Close())

		_, e := LoadSubject(ctx, closed, uuid.New().String())

		require.NotNil(t, e)
		require.Equal(t, http.StatusInternalServerError, e.Status)
		require.Equal(t, ErrMsgVerificationFailed, e.Body["error"])
		require.NotNil(t, e.Cause)
	})
}

func TestLockSubjectTx_Refusals(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	begin := func(t *testing.T) *sql.Tx {
		t.Helper()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		return tx
	}

	t.Run("both locks accept a matching epoch and carry the P1 set", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestWebAuthn(t, db, userID)
		for _, lock := range []Lock{LockForShare, LockForNoKeyUpdate} {
			// Release each lock before taking the next: FOR SHARE conflicts
			// with FOR NO KEY UPDATE, so holding both would self-deadlock.
			tx := begin(t)
			s, e := LockSubjectTx(ctx, tx, userID, lock, "")
			require.NoError(t, tx.Rollback())
			require.Nil(t, e)
			require.Equal(t, []string{"webauthn"}, s.MFAMethods)
			require.True(t, s.MFAEnabled)
		}
	})

	t.Run("rotated epoch is the tagged 401", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		_, err := db.Exec(`UPDATE users SET credential_epoch = 'server-epoch' WHERE id = $1`, userID)
		require.NoError(t, err)

		_, e := LockSubjectTx(ctx, begin(t), userID, LockForShare, "stale-epoch")

		require.NotNil(t, e)
		require.Equal(t, http.StatusUnauthorized, e.Status)
		require.Equal(t, ErrMsgAuthenticationRequired, e.Body["error"])
		require.True(t, e.EpochMismatch())
		require.Nil(t, e.Cause)
	})

	t.Run("deleted account is an untagged 401", func(t *testing.T) {
		_, e := LockSubjectTx(ctx, begin(t), uuid.New().String(), LockForNoKeyUpdate, "")

		require.NotNil(t, e)
		require.Equal(t, http.StatusUnauthorized, e.Status)
		require.Equal(t, ErrMsgSessionNoLongerValid, e.Body["error"])
		require.False(t, e.EpochMismatch())
	})

	t.Run("an unknown lock fails closed", func(t *testing.T) {
		_, e := LockSubjectTx(ctx, begin(t), subjectTestUser(t, db), Lock(99), "")

		require.NotNil(t, e)
		require.Equal(t, http.StatusInternalServerError, e.Status)
		require.NotNil(t, e.Cause)
	})

	t.Run("nil error is nil-safe for EpochMismatch", func(t *testing.T) {
		var e *Error
		require.False(t, e.EpochMismatch())
	})
}

// TestLockSubjectTx_SeesAFactorCommittedWhileItWaited pins the ordering
// invariant in LockSubjectTx's doc: the P1 read is a separate statement AFTER
// the lock, so a factor write that held the users row while the gate waited is
// visible once the gate proceeds. Folding the EXISTS subqueries into the lock
// statement keeps them on the pre-wait snapshot, and this test goes red.
func TestLockSubjectTx_SeesAFactorCommittedWhileItWaited(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	userID := subjectTestUser(t, db)

	writer, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback() })
	var locked string
	require.NoError(t, writer.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&locked))
	subjectTestWebAuthn(t, writer, userID)

	gate, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { _ = gate.Rollback() })

	type result struct {
		s Subject
		e *Error
	}
	done := make(chan result, 1)
	go func() {
		s, e := LockSubjectTx(ctx, gate, userID, LockForShare, "")
		done <- result{s, e}
	}()

	// The gate must be blocked on the writer's row lock, not finished.
	select {
	case r := <-done:
		t.Fatalf("gate did not wait for the writer's users-row lock: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, writer.Commit())

	select {
	case r := <-done:
		require.Nil(t, r.e)
		require.Equal(t, []string{"webauthn"}, r.s.MFAMethods,
			"the P1 read must see the factor committed while the gate waited")
	case <-time.After(10 * time.Second):
		t.Fatal("gate never proceeded after the writer committed")
	}
}
