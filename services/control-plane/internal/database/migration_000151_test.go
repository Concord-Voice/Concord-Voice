package database_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMigration000151_CredentialEpochVoiceEjectionsContract(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migrationReadFile(t, "../../migrations/000151_add_credential_epoch_voice_ejections.up.sql")
	down := migrationReadFile(t, "../../migrations/000151_add_credential_epoch_voice_ejections.down.sql")
	ctx := t.Context()

	var tableExists bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT to_regclass('public.credential_epoch_voice_ejections') IS NOT NULL`).Scan(&tableExists))
	require.True(t, tableExists)

	var columns int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'credential_epoch_voice_ejections'
		  AND column_name IN ('user_id', 'credential_epoch', 'superseded_credential_epoch', 'generation', 'attempts', 'failure_class', 'reconcile_after', 'created_at', 'updated_at')`).Scan(&columns))
	require.Equal(t, 9, columns)

	var foreignKeys int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		WHERE r.relname = 'credential_epoch_voice_ejections' AND c.contype = 'f'`).Scan(&foreignKeys))
	require.Zero(t, foreignKeys, "account deletion must not discard delivery evidence")

	var dueIndex bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'idx_credential_epoch_voice_ejections_due')`).Scan(&dueIndex))
	require.True(t, dueIndex)

	oldEpoch, newEpoch := "0123456789abcdef0123456789abcdef", "abcdefabcdefabcdefabcdefabcdefab" // pragma: allowlist secret
	userID := uuid.NewString()
	_, err := db.ExecContext(ctx, `INSERT INTO credential_epoch_voice_ejections
		(user_id, credential_epoch, superseded_credential_epoch)
		VALUES ($1, $2, $3)`, userID, newEpoch, oldEpoch)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO credential_epoch_voice_ejections
		(user_id, credential_epoch, superseded_credential_epoch)
		VALUES ($1, $2, '')`, uuid.NewString(), newEpoch)
	require.NoError(t, err, "empty legacy predecessor is valid")

	for _, query := range []string{
		`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch) VALUES ($1, 'too-short', $3)`,
		`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch) VALUES ($1, $2, 'not-an-epoch')`,
		`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch) VALUES ($1, $2, $2)`,
		`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch, attempts) VALUES ($1, $2, '', -1)`,
		`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch, failure_class) VALUES ($1, $2, '', 'transport')`,
	} {
		_, err = db.ExecContext(ctx, query, uuid.NewString(), newEpoch, newEpoch)
		require.Error(t, err, "constraint must reject malformed evidence")
	}

	// A down migration must refuse while delivery evidence remains.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, down)
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	// Once the evidence drains, down/up must both work, proving the guard is
	// protective rather than permanently blocking rollback.
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM credential_epoch_voice_ejections`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, down)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
}
