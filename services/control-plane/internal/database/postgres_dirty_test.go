package database_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestRunMigrations_DirtyDatabaseFailsClosed(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)

	var version int
	require.NoError(t, db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version))
	result, err := db.Exec(`UPDATE schema_migrations SET dirty = TRUE WHERE version = $1`, version)
	require.NoError(t, err)
	rowsAffected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)
	t.Cleanup(func() {
		_, restoreErr := db.Exec(`UPDATE schema_migrations SET dirty = FALSE WHERE version = $1`, version)
		require.NoError(t, restoreErr)
	})
	t.Chdir("../..")

	err = database.RunMigrations(db)
	require.Error(t, err)
	require.ErrorContains(t, err, fmt.Sprintf("version %d is dirty", version))

	var observedVersion int
	var dirty bool
	require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&observedVersion, &dirty))
	require.Equal(t, version, observedVersion)
	require.True(t, dirty)
}
