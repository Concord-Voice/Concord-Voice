package loginfamily_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var ctx = context.Background()

func database(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	// New retained evidence is deliberately outside the baseline fixture's list.
	// This cleanup runs before SetupTestDB's cleanup, in this isolated test DB.
	t.Cleanup(func() {
		_, err := testdb.ExecReplica(db, `TRUNCATE login_family_revocation_targets, login_family_sids, login_families, login_family_revocations CASCADE`)
		require.NoError(t, err)
	})
	return db
}
func transaction(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	})
	return tx
}
func authority(t *testing.T, tx *sql.Tx, id uuid.UUID, mode lf.UserLockMode) lf.UserAuthority {
	t.Helper()
	b, err := lf.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	s, err := lf.LockUsersTx(ctx, tx, b, []uuid.UUID{id}, mode)
	require.NoError(t, err)
	u, err := s.ForUser(id)
	require.NoError(t, err)
	return u
}
func newFamily(t *testing.T, db *sql.DB, user uuid.UUID, expiry time.Time) lf.AccessIdentity {
	t.Helper()
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	f, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	identity := lf.AccessIdentity{UserID: user, SID: uuid.New(), AccessExpiresAt: expiry}
	require.NoError(t, lf.BindSIDTx(ctx, tx, f, identity.SID, expiry))
	require.NoError(t, tx.Commit())
	return identity
}
func expires() time.Time { return time.Now().Add(time.Hour).UTC().Truncate(time.Second) }
func reason(t *testing.T, err error, class error, why lf.ErrorReason) {
	t.Helper()
	require.ErrorIs(t, err, class)
	var classified *lf.Error
	require.ErrorAs(t, err, &classified)
	require.Equal(t, why, classified.Reason())
}
func refresh(t *testing.T, db *sql.DB, identity lf.AccessIdentity, expired bool) {
	t.Helper()
	deadline := time.Now().Add(time.Hour)
	if expired {
		deadline = time.Now().Add(-time.Hour)
	}
	_, err := db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, identity.SID, identity.UserID, identity.SID.String(), deadline)
	require.NoError(t, err)
}
func count(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}
