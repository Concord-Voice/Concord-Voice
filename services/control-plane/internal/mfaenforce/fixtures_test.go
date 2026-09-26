package mfaenforce_test

// Shared fixtures for the database-backed tests. The gate's job is which rows
// it locks and which factor rows count, so these tests run against the real
// schema: no fake can model PostgreSQL's row-lock conflict table or the
// per-statement snapshots the P1 read depends on.
//
// External test package on purpose: it exercises only the exported contract
// that #3454/#3455 consume, and it can import internal/mfa for the real
// verifier without risking an import cycle as consumers of this package land.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// testBackupCode is a fixed backup code so the rollback test is deterministic.
// Codes stay at or under 20 characters: a longer one routes the real verifier
// to its WebAuthn inline-token lookup, which needs Redis these tests do not
// wire.
const testBackupCode = "MFAENF01"

// probeLockTimeout bounds how long a lock probe waits. A probe on a free row
// returns at once; one on a held row fails with 55P03 when this expires, so a
// missing lock is a fast failure rather than a hang.
const probeLockTimeout = `SET LOCAL lock_timeout = '100ms'`

func gateTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := dbtest.SetupTestDB(t)
	return db
}

// createUser inserts a user. The hash is an opaque stand-in: nothing here
// verifies a password.
func createUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := uuid.New().String()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, 'stored-hash-fixture', true, true)`, id, id+"@mfaenforce.test", "me"+id[:8])
	require.NoError(t, err)
	return id
}

func createServer(t *testing.T, db *sql.DB, ownerID string, enforcing bool) string {
	t.Helper()
	id := uuid.New().String()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id, enforce_mfa_dangerous_actions)
		VALUES ($1, 'mfaenforce fixture', $2, $3)`, id, ownerID, enforcing)
	require.NoError(t, err)
	return id
}

// newVerifier builds the real MFA verifier. The gate's backup-code guarantee
// is a property of VerifyCodeTx writing on the caller's transaction, which a
// fake cannot demonstrate.
func newVerifier(t *testing.T, db *sql.DB) (*mfa.Handler, *mfa.Keyring) {
	t.Helper()
	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	return mfa.NewHandler(db, nil, logger.NewWithWriter(io.Discard), ring, "test-secret", nil, "test"), ring
}

// enrollTOTP gives the user a confirmed TOTP factor sealed under ring, plus one
// unused backup code (testBackupCode). It returns the TOTP secret.
func enrollTOTP(t *testing.T, db *sql.DB, ring *mfa.Keyring, userID string) string {
	t.Helper()
	key, err := mfa.GenerateSecret(userID + "@mfaenforce.test")
	require.NoError(t, err)
	enc, nonce, version, err := ring.Seal([]byte(key.Secret()))
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(testBackupCode))
	_, err = db.Exec(`INSERT INTO user_mfa_totp
		(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
		VALUES ($1, $2, $3, $4, TRUE, TRUE, $5, $6)`,
		userID, enc, nonce, version, pq.Array([]string{hex.EncodeToString(digest[:])}), pq.Array([]bool{false}))
	require.NoError(t, err)
	return key.Secret()
}

func enrollWebAuthn(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, NOW())`, uuid.New().String(), userID, []byte("cred-"+userID))
	require.NoError(t, err)
}

// backupCodeUsed reads the durable flag, so a test asserts what committed.
func backupCodeUsed(t *testing.T, db *sql.DB, userID string) bool {
	t.Helper()
	var used []bool
	require.NoError(t, db.QueryRow(`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).
		Scan(pq.Array(&used)))
	require.Len(t, used, 1)
	return used[0]
}

func beginTx(t *testing.T, db *sql.DB, level sql.IsolationLevel) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: level})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// lockGate opens a READ COMMITTED transaction and runs LockGateTx with the
// weakest locks, which is all the confirmation tests need.
func lockGate(t *testing.T, db *sql.DB, serverID, actorID string) (mfaenforce.Gate, *sql.Tx) {
	t.Helper()
	tx := beginTx(t, db, sql.LevelReadCommitted)
	g, err := mfaenforce.LockGateTx(context.Background(), tx, serverID, actorID,
		stepup.LockForShare, mfaenforce.ServerForShare, "")
	require.NoError(t, err)
	return g, tx
}

// probeBlocks runs stmt in its own transaction under probeLockTimeout and
// reports whether a held lock refused it. Any other failure fails the test.
// It reads the SQLSTATE itself rather than calling IsLockConflict, so a broken
// IsLockConflict cannot make a probe lie.
func probeBlocks(t *testing.T, db *sql.DB, stmt string, args ...any) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, probeLockTimeout)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, stmt, args...)
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr) && pqErr.Code == "55P03",
		"the probe failed for a reason other than a held lock: %v", err)
	return true
}
