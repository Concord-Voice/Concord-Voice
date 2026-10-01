package database_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration000162_StepUpTokenSchemaIsPinned pins step_up_tokens (#3455,
// PR #3509) as the store relies on it: a SHA-256 primary key, a cascade on
// account deletion, the two factors, the writer-guaranteed CHECKs (#3509
// migration review), both indexes, and a down that drops the table, whose
// indexes go with it.
func TestMigration000162_StepUpTokenSchemaIsPinned(t *testing.T) {
	up := migrationReadFile(t, "../../migrations/000162_add_step_up_tokens.up.sql")
	down := migrationReadFile(t, "../../migrations/000162_add_step_up_tokens.down.sql")

	for _, fragment := range []string{
		"CREATE TABLE public.step_up_tokens",
		"token_hash BYTEA PRIMARY KEY CHECK (octet_length(token_hash) = 32)",
		"user_id UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE",
		"factor TEXT NOT NULL CHECK (factor IN ('password', 'webauthn'))",
		"purpose TEXT NOT NULL CHECK (purpose <> '')",
		"credential_epoch TEXT CHECK (credential_epoch IS NULL OR credential_epoch ~ '^[0-9a-f]{32}$')",
		"expires_at TIMESTAMPTZ NOT NULL",
		"created_at TIMESTAMPTZ NOT NULL DEFAULT now()",
		"CREATE INDEX idx_step_up_tokens_user_expires",
		"ON public.step_up_tokens (user_id, expires_at)",
		"CREATE INDEX idx_step_up_tokens_expires",
		"ON public.step_up_tokens (expires_at)",
	} {
		require.Contains(t, up, fragment)
	}
	require.NotContains(t, up, "INDEX CONCURRENTLY", "the file runs as one transaction")

	var statements []string
	for _, line := range strings.Split(down, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			statements = append(statements, line)
		}
	}
	require.Equal(t, []string{"DROP TABLE IF EXISTS public.step_up_tokens;"}, statements,
		"the down drops exactly the table, whose indexes go with it")
}
