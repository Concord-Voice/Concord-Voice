package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sourceSettingName is the transaction-local setting the audit triggers read. It is a
// literal here on purpose: deriving it from the code under test would make the pin vacuous.
const sourceSettingName = "concord.auth_change_source"

// rowQuerier is the read surface shared by *sql.DB, *sql.Conn and *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sourceReading reads the setting the way every production reader must (E4):
// NULLIF(current_setting(name, true), ”), so "never set" and "set then reset to ”"
// are one state. Valid is false when the setting is unset.
func sourceReading(t *testing.T, q rowQuerier) sql.NullString {
	t.Helper()
	var got sql.NullString
	require.NoError(t, q.QueryRowContext(context.Background(),
		`SELECT NULLIF(current_setting('`+sourceSettingName+`', true), '')`).Scan(&got))
	return got
}

// rawSourceReading reads current_setting without the NULLIF, to pin the E4 asymmetry:
// a session that has once set the value reads ” (not NULL) after the transaction ends.
func rawSourceReading(t *testing.T, q rowQuerier) sql.NullString {
	t.Helper()
	var got sql.NullString
	require.NoError(t, q.QueryRowContext(context.Background(),
		`SELECT current_setting('`+sourceSettingName+`', true)`).Scan(&got))
	return got
}

// sourceTestDB returns a migrated test database pool.
func sourceTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	return db
}

func beginSourceTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	return tx
}

// admittedSources is the closed admitted set. The wire spellings are literals: the
// database-side reader compares against these strings, so a drifted constant must fail.
var admittedSources = []struct {
	name   string
	source decide.Source
	value  string
}{
	{"user-session", decide.SourceUserSession, "user_session"},
	{"lockdown", decide.SourceLockdown, "lockdown"},
	{"break-glass", decide.SourceBreakGlass, "break_glass"},
	{"recovery-key", decide.SourceRecoveryKey, "recovery_key"},
}

// refusedSources are values sourceTx must never write: the two named non-admitted
// constants and arbitrary strings, including near-misses of an admitted spelling.
var refusedSources = []struct {
	name   string
	source decide.Source
}{
	{"operator", decide.SourceOperator},
	{"unattributed", decide.SourceUnattributed},
	{"empty", decide.Source("")},
	{"uppercase", decide.Source("USER_SESSION")},
	{"trailing-space", decide.Source("user_session ")},
	{"leading-space", decide.Source(" lockdown")},
	{"trailing-newline", decide.Source("break_glass\n")},
	{"hyphenated", decide.Source("recovery-key")},
	{"unknown", decide.Source("something_else")},
}

// ---------------------------------------------------------------------------
// Admitted values
// ---------------------------------------------------------------------------

func TestSourceTx_SetsEachAdmittedValueInsideTheTransaction(t *testing.T) {
	db := sourceTestDB(t)
	for _, tc := range admittedSources {
		t.Run(tc.name, func(t *testing.T) {
			tx := beginSourceTx(t, db)
			defer func() { _ = tx.Rollback() }()

			require.NoError(t, sourceTx(context.Background(), tx, tc.source))

			got := sourceReading(t, tx)
			assert.True(t, got.Valid, "the setting must be visible inside the transaction")
			assert.Equal(t, tc.value, got.String)
		})
	}
}

// The attribution is for the transaction's own writes: another session must not see it.
// This pins PostgreSQL's per-session setting behaviour, not sourceTx: no implementation
// can fail it, so it is a guard against the server changing under the design.
func TestSourceTx_NotVisibleToAnotherSessionWhileOpen(t *testing.T) {
	db := sourceTestDB(t)
	other := extraPool(t)

	tx := beginSourceTx(t, db)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, sourceTx(context.Background(), tx, decide.SourceLockdown))
	require.Equal(t, "lockdown", sourceReading(t, tx).String)

	assert.False(t, sourceReading(t, other).Valid)
	assert.False(t, rawSourceReading(t, other).Valid, "a session that never set it reads NULL")
}

func TestSourceTx_LaterCallInTheSameTransactionWins(t *testing.T) {
	db := sourceTestDB(t)
	tx := beginSourceTx(t, db)
	defer func() { _ = tx.Rollback() }()

	require.NoError(t, sourceTx(context.Background(), tx, decide.SourceUserSession))
	require.NoError(t, sourceTx(context.Background(), tx, decide.SourceRecoveryKey))

	assert.Equal(t, "recovery_key", sourceReading(t, tx).String)
}

// ---------------------------------------------------------------------------
// Gone after the transaction (E4)
// ---------------------------------------------------------------------------

// Pinning one connection is what makes this test meaningful: a pool may hand the read
// a different session, which proves nothing about the session that did the write.
func TestSourceTx_GoneAfterCommitInTheSameSession(t *testing.T) {
	db := sourceTestDB(t)
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	require.NoError(t, sourceTx(ctx, tx, decide.SourceUserSession))
	require.Equal(t, "user_session", sourceReading(t, tx).String, "precondition: set inside the tx")
	require.NoError(t, tx.Commit())

	// E4: the same session reads '' (not NULL) once the transaction is over, which is
	// why every reader applies NULLIF.
	raw := rawSourceReading(t, conn)
	assert.True(t, raw.Valid, "PG16 leaves an empty string, not NULL, in the committing session")
	assert.Equal(t, "", raw.String)

	assert.False(t, sourceReading(t, conn).Valid, "NULLIF(current_setting(...), '') IS NULL after commit")
}

func TestSourceTx_GoneAfterCommitOnAFreshPooledRead(t *testing.T) {
	db := sourceTestDB(t)

	tx := beginSourceTx(t, db)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, sourceTx(context.Background(), tx, decide.SourceBreakGlass))
	require.NoError(t, tx.Commit())

	assert.False(t, sourceReading(t, db).Valid, "a pooled read after commit sees NULL")
	// Pins PostgreSQL's per-session setting behaviour, not sourceTx: no implementation can fail it.
	assert.False(t, sourceReading(t, extraPool(t)).Valid, "a fresh pool sees NULL")
}

func TestSourceTx_GoneAfterRollbackInTheSameSession(t *testing.T) {
	db := sourceTestDB(t)
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	require.NoError(t, sourceTx(ctx, tx, decide.SourceRecoveryKey))
	require.Equal(t, "recovery_key", sourceReading(t, tx).String, "precondition: set inside the tx")
	require.NoError(t, tx.Rollback())

	// E4 holds for a rollback as for a commit: the session reads '' (not NULL).
	raw := rawSourceReading(t, conn)
	assert.True(t, raw.Valid, "the rolled-back session leaves an empty string, not NULL")
	assert.Equal(t, "", raw.String)

	assert.False(t, sourceReading(t, conn).Valid)
	assert.False(t, sourceReading(t, db).Valid)
}

// A second transaction on the same session must not inherit the first one's attribution:
// that is the cross-request leak the transaction-local flag exists to prevent.
func TestSourceTx_DoesNotLeakIntoTheNextTransactionOnTheSession(t *testing.T) {
	db := sourceTestDB(t)
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	first, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = first.Rollback() }()
	require.NoError(t, sourceTx(ctx, first, decide.SourceLockdown))
	require.NoError(t, first.Commit())

	second, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = second.Rollback() }()

	assert.False(t, sourceReading(t, second).Valid, "the next transaction starts unattributed")
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestSourceTx_RefusesUnattributedOperatorAndUnknown(t *testing.T) {
	db := sourceTestDB(t)
	for _, tc := range refusedSources {
		t.Run(tc.name, func(t *testing.T) {
			tx := beginSourceTx(t, db)
			defer func() { _ = tx.Rollback() }()

			err := sourceTx(context.Background(), tx, tc.source)

			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrSourceRefused), "got %v", err)
			// No statement ran: nothing was written, not even the refused spelling.
			assert.False(t, sourceReading(t, tx).Valid, "a refusal must leave the setting unset")
		})
	}
}

// A refusal must not disturb an attribution already in place: a buggy implementation
// that ran set_config before checking would overwrite it.
func TestSourceTx_RefusalLeavesAnExistingValueUntouched(t *testing.T) {
	db := sourceTestDB(t)
	for _, tc := range refusedSources {
		t.Run(tc.name, func(t *testing.T) {
			tx := beginSourceTx(t, db)
			defer func() { _ = tx.Rollback() }()
			require.NoError(t, sourceTx(context.Background(), tx, decide.SourceUserSession))

			err := sourceTx(context.Background(), tx, tc.source)

			require.ErrorIs(t, err, ErrSourceRefused)
			assert.Equal(t, "user_session", sourceReading(t, tx).String)
		})
	}
}

// A finished transaction would fail any statement with sql.ErrTxDone. A refusal that
// still returns ErrSourceRefused therefore proves the check ran before any statement.
func TestSourceTx_RefusalRunsNoStatement(t *testing.T) {
	db := sourceTestDB(t)
	tx := beginSourceTx(t, db)
	require.NoError(t, tx.Rollback())

	for _, tc := range refusedSources {
		t.Run(tc.name, func(t *testing.T) {
			err := sourceTx(context.Background(), tx, tc.source)

			require.ErrorIs(t, err, ErrSourceRefused)
			assert.False(t, errors.Is(err, sql.ErrTxDone), "the refusal must come before the statement")
		})
	}
}

// ---------------------------------------------------------------------------
// Failures
// ---------------------------------------------------------------------------

func TestSourceTx_FinishedTransactionIsAnErrorNotARefusal(t *testing.T) {
	db := sourceTestDB(t)
	tx := beginSourceTx(t, db)
	require.NoError(t, tx.Rollback())

	err := sourceTx(context.Background(), tx, decide.SourceUserSession)

	require.Error(t, err)
	assert.ErrorIs(t, err, sql.ErrTxDone)
	assert.False(t, errors.Is(err, ErrSourceRefused), "an admitted value is never refused")
}

func TestSourceTx_CancelledContextReturnsAnErrorAndSetsNothing(t *testing.T) {
	db := sourceTestDB(t)
	tx := beginSourceTx(t, db)
	defer func() { _ = tx.Rollback() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sourceTx(ctx, tx, decide.SourceUserSession)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, errors.Is(err, ErrSourceRefused), "a cancelled context is not a refusal")
	assert.False(t, sourceReading(t, tx).Valid, "nothing may be set when the call was cancelled")
}

// ---------------------------------------------------------------------------
// E4: an empty value reads as NULL
// ---------------------------------------------------------------------------

// Pins the PG16 behaviour the whole design leans on (spec row E4): after a
// transaction-local set_config and COMMIT, the same session reads ” rather than NULL,
// and NULLIF(current_setting(name, true), ”) collapses both to NULL. This test uses
// the raw statement, not sourceTx, so it fails on a server change independent of the
// code under test.
func TestSourceTx_EmptyReadsAsNull(t *testing.T) {
	db := sourceTestDB(t)
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	before := rawSourceReading(t, conn)
	require.False(t, before.Valid, "precondition: a session that never set it reads NULL")

	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SELECT set_config('`+sourceSettingName+`', 'user_session', true)`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	after := rawSourceReading(t, conn)
	assert.True(t, after.Valid)
	assert.Equal(t, "", after.String, "the committing session reads an empty string, not NULL")

	var isNull bool
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT NULLIF(current_setting('`+sourceSettingName+`', true), '') IS NULL`).Scan(&isNull))
	assert.True(t, isNull)
}
