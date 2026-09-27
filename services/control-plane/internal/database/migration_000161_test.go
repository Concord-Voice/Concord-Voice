package database_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration000161_VersionedAdmissionSchemaIsSymmetric(t *testing.T) {
	up := migrationReadFile(t, "../../migrations/000161_version_voice_pending_admissions.up.sql")
	down := migrationReadFile(t, "../../migrations/000161_version_voice_pending_admissions.down.sql")

	for _, fragment := range []string{
		"ALTER TABLE public.voice_pending_admissions",
		"ADD COLUMN admission_id UUID NOT NULL",
		"ADD COLUMN socket_id TEXT NOT NULL",
		"CHECK (socket_id <> '' AND length(socket_id) <= 128)",
	} {
		require.Contains(t, up, fragment)
	}

	require.Contains(t, down, "DROP COLUMN IF EXISTS socket_id")
	require.Contains(t, down, "DROP COLUMN IF EXISTS admission_id")
	require.NotContains(t, up, "CREATE SEQUENCE")
	require.NotContains(t, down, "DROP SEQUENCE")
}
