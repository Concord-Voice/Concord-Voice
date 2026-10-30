package testdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestModuleDir_FindsModuleRootFromPackageDir(t *testing.T) {
	dir, err := ModuleDir("migrations")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(dir, filepath.Join("services", "control-plane", "migrations")), dir)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, moduleMigrationsDir, dir, "the init-time value must match")
}

func TestModuleDir_NoGoModAboveFails(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := ModuleDir("migrations")
	require.Error(t, err)
}

func TestTruncateAllTables_RemovesRetainedLegacyVoiceSessions(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit isolated database required")
	db, cleanup := SetupTestDB(t)
	defer cleanup()
	generation := uuid.New()
	// Retained evidence deliberately has no user FK, so users CASCADE cannot reset it.
	_, err := db.Exec(`INSERT INTO voice_enforcement_sessions
		(session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id)
		VALUES($1,$2,$3,'channel',$4,'','retained-fixture-socket')`, generation, uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation=$1`, generation).Scan(&count))
	require.Equal(t, 1, count)

	require.NoError(t, TruncateAllTables(db))

	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation=$1`, generation).Scan(&count))
	require.Zero(t, count)
	var prepared bool
	require.NoError(t, db.QueryRow(`SELECT activated_at IS NULL FROM login_family_rollout WHERE id=true`).Scan(&prepared))
	require.True(t, prepared)
}
