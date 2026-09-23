package database_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMigration000148To150_GenerationRolloutPhases(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	down134 := migrationReadFile(t, "../../migrations/000150_enforce_dm_block_voice_ejection_generation.down.sql")
	down133 := migrationReadFile(t, "../../migrations/000149_backfill_dm_block_voice_ejection_generation.down.sql")
	down132 := migrationReadFile(t, "../../migrations/000148_add_dm_block_voice_ejection_generation.down.sql")
	up132 := migrationReadFile(t, "../../migrations/000148_add_dm_block_voice_ejection_generation.up.sql")
	up133 := migrationReadFile(t, "../../migrations/000149_backfill_dm_block_voice_ejection_generation.up.sql")
	up134 := migrationReadFile(t, "../../migrations/000150_enforce_dm_block_voice_ejection_generation.up.sql")

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, migration := range []string{down134, down133, down132, up132} {
		_, err = tx.Exec(migration)
		require.NoError(t, err)
	}

	var isNullable bool
	var defaultExpr *string
	require.NoError(t, tx.QueryRow(`
		SELECT is_nullable = 'YES', column_default
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'dm_block_voice_ejections'
		  AND column_name = 'generation'`).Scan(&isNullable, &defaultExpr))
	require.True(t, isNullable)
	require.Nil(t, defaultExpr)

	_, err = tx.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id)
		VALUES ($1, $2), ($3, $4)`, uuid.New(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	_, err = tx.Exec(up133)
	require.NoError(t, err)

	var generations []uuid.UUID
	rows, err := tx.Query(`SELECT generation FROM dm_block_voice_ejections ORDER BY conversation_id, user_id`)
	require.NoError(t, err)
	for rows.Next() {
		var generation uuid.UUID
		require.NoError(t, rows.Scan(&generation))
		generations = append(generations, generation)
	}
	require.NoError(t, rows.Close())
	require.Len(t, generations, 2)
	require.NotEqual(t, uuid.Nil, generations[0])
	require.NotEqual(t, generations[0], generations[1])

	_, err = tx.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id)
		VALUES ($1, $2)`, uuid.New(), uuid.New())
	require.NoError(t, err)
	_, err = tx.Exec(up134)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(`
		SELECT is_nullable = 'NO'
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'dm_block_voice_ejections'
		  AND column_name = 'generation'`).Scan(&isNullable))
	require.True(t, isNullable, "000150 must enforce NOT NULL")
}

func TestMigration000148To150_DownRestoresParentSchema(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	down134 := migrationReadFile(t, "../../migrations/000150_enforce_dm_block_voice_ejection_generation.down.sql")
	down133 := migrationReadFile(t, "../../migrations/000149_backfill_dm_block_voice_ejection_generation.down.sql")
	down132 := migrationReadFile(t, "../../migrations/000148_add_dm_block_voice_ejection_generation.down.sql")
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	for _, migration := range []string{down134, down133, down132} {
		_, err = tx.Exec(migration)
		require.NoError(t, err)
	}
	_, err = tx.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id)
		VALUES ($1, $2)`, uuid.New(), uuid.New())
	require.NoError(t, err)

	for _, name := range []string{
		"000148_add_dm_block_voice_ejection_generation.up.sql",
		"000149_backfill_dm_block_voice_ejection_generation.up.sql",
		"000150_enforce_dm_block_voice_ejection_generation.up.sql",
	} {
		_, err = tx.Exec(migrationReadFile(t, "../../migrations/"+name))
		require.NoError(t, err)
	}
	_, err = tx.Exec(`DELETE FROM dm_block_voice_ejections`)
	require.NoError(t, err)
	for _, migration := range []string{down134, down133, down132} {
		_, err = tx.Exec(migration)
		require.NoError(t, err)
	}
	_, err = tx.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id)
		VALUES ($1, $2)`, uuid.New(), uuid.New())
	require.NoError(t, err)
}

func TestMigration000148_DownRetainsNonEmptyEvidence(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, name := range []string{
		"000150_enforce_dm_block_voice_ejection_generation.down.sql",
		"000149_backfill_dm_block_voice_ejection_generation.down.sql",
	} {
		_, err = tx.Exec(migrationReadFile(t, "../../migrations/"+name))
		require.NoError(t, err)
	}
	_, err = tx.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id, generation)
		VALUES ($1, $2, $3)`, uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000148_add_dm_block_voice_ejection_generation.down.sql"))
	require.Error(t, err, "000148 down must retain non-empty delivery evidence")
}
