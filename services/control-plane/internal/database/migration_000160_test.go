package database_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration000160_PendingAdmissionSchemaIsSymmetric(t *testing.T) {
	up := migrationReadFile(t, "../../migrations/000160_add_voice_pending_admissions.up.sql")
	down := migrationReadFile(t, "../../migrations/000160_add_voice_pending_admissions.down.sql")

	for _, fragment := range []string{
		"CREATE TABLE public.voice_pending_admissions",
		"channel_id UUID NOT NULL REFERENCES public.channels(id) ON DELETE CASCADE",
		"user_id UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE",
		"expires_at TIMESTAMPTZ NOT NULL",
		"PRIMARY KEY (channel_id, user_id)",
		"CREATE INDEX idx_voice_pending_admissions_expires_at",
		"ON public.voice_pending_admissions (expires_at)",
	} {
		require.Contains(t, up, fragment)
	}

	require.True(t, strings.HasPrefix(strings.TrimSpace(down), "DROP INDEX IF EXISTS public.idx_voice_pending_admissions_expires_at;"))
	require.Contains(t, down, "DROP TABLE IF EXISTS public.voice_pending_admissions;")
}
