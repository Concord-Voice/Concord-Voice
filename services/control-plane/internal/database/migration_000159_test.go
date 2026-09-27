package database_test

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestMigration000159_VoiceAuthorizationRevisionRollbackGuard(t *testing.T) {
	db := openMigration159IsolatedDB(t)

	up := migrationReadFile(t, "../../migrations/000159_add_voice_authorization_revision_sequence.up.sql")
	down := migrationReadFile(t, "../../migrations/000159_add_voice_authorization_revision_sequence.down.sql")
	ctx := t.Context()

	// Establish the migration's sequence on the isolated database. The test
	// owns this database, so its rollback checks cannot race another caller.
	_, err := db.ExecContext(ctx, up)
	require.NoError(t, err)

	// An unused sequence may be rolled back and recreated.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, down)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var revision int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT nextval('public.voice_authorization_revision_seq')`).Scan(&revision))
	require.Equal(t, int64(1), revision)

	// Once allocated, rollback must preserve the watermark rather than allowing
	// a later recreation to reuse a revision.
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, down)
	require.ErrorContains(t, err, "refusing to drop consumed")
	require.NoError(t, tx.Rollback())
}

func openMigration159IsolatedDB(t *testing.T) *sql.DB {
	t.Helper()

	databaseURL := dbtest.DatabaseURL()
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	if err := adminDB.PingContext(t.Context()); err != nil {
		_ = adminDB.Close()
		require.NoError(t, err)
	}

	databaseName := "concord_migration_000159_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- unique test database name is safely quoted with pq.QuoteIdentifier
	if _, err := adminDB.ExecContext(t.Context(), `CREATE DATABASE `+pq.QuoteIdentifier(databaseName)); err != nil {
		_ = adminDB.Close()
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- unique test database name is safely quoted with pq.QuoteIdentifier
		_, dropErr := adminDB.ExecContext(cleanupCtx, `DROP DATABASE IF EXISTS `+pq.QuoteIdentifier(databaseName)+` WITH (FORCE)`)
		require.NoError(t, dropErr)
		require.NoError(t, adminDB.Close())
	})

	parsed.Path = "/" + databaseName
	db, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	require.NoError(t, db.PingContext(t.Context()))
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}
