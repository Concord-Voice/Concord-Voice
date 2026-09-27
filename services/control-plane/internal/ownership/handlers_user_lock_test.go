package ownership

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestLockOwnershipUsersTxLocksBothSubjectsBeforeServerWork(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	first := dbtest.CreateUser(t, db)
	second := dbtest.CreateUser(t, db)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback ownership user lock transaction: %v", rollbackErr)
		}
	}()
	require.NoError(t, lockOwnershipUsersTx(context.Background(), tx, second.String(), first.String()))

	for _, userID := range []uuid.UUID{first, second} {
		probe, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		var locked uuid.UUID
		err = probe.QueryRowContext(context.Background(),
			`SELECT id FROM users WHERE id = $1 FOR UPDATE NOWAIT`, userID).Scan(&locked)
		require.Error(t, err)
		var pqErr *pq.Error
		require.ErrorAs(t, err, &pqErr)
		require.Equal(t, "55P03", string(pqErr.Code))
		require.NoError(t, probe.Rollback())
	}
}

func TestLockOwnershipUsersTxRejectsMissingSubject(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	err = lockOwnershipUsersTx(context.Background(), tx, uuid.NewString())
	require.EqualError(t, err, "ownership transfer user no longer exists")
}

func TestLockOwnershipUsersTxSkipsEmptySubjects(t *testing.T) {
	require.NoError(t, lockOwnershipUsersTx(context.Background(), nil, "", ""))
}

func TestLockOwnershipUsersTxRejectsMalformedSubject(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	err = lockOwnershipUsersTx(context.Background(), tx, "not-a-uuid")
	require.Error(t, err)
	require.Contains(t, err.Error(), "lock ownership users")
}
