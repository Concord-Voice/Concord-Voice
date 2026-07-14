package database_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	opsMetricsReaderRolePrefix  = "concord_ops_metrics_reader_"
	opsMetricsReaderOwnerMarker = "concord-voice:ops-metrics-reader:v2:"
)

func TestMigration000088_FilesAndPrivilegeLock(t *testing.T) {
	up := migration000088SQL(t, "up")
	down := migration000088SQL(t, "down")
	readme := migration000088ReadFile(t, filepath.Join("..", "..", "migrations", "README.md"))

	for _, fragment := range []string{
		"md5(current_database())",
		opsMetricsReaderOwnerMarker,
		"shobj_description",
		"pg_auth_members",
		"has_schema_privilege",
		"information_schema.table_privileges",
		"pg_attribute",
		"pg_default_acl",
		"pg_shdepend",
		"aclexplode",
		"COMMENT ON ROLE",
		"CREATE ROLE %I",
		"NOLOGIN",
		"NOSUPERUSER",
		"NOCREATEDB",
		"NOCREATEROLE",
		"NOINHERIT",
		"NOREPLICATION",
		"NOBYPASSRLS",
		"REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC",
		"GRANT USAGE ON SCHEMA public TO %I",
		"GRANT SELECT ON ops_metric_samples, ops_metric_rollups TO %I",
		"ALTER ROLE %I SET default_transaction_read_only = on",
	} {
		assert.Contains(t, up, fragment)
	}
	assert.NotContains(t, strings.ToUpper(up), "ALTER DEFAULT PRIVILEGES")
	assert.NotContains(t, up, "CREATE ROLE concord_ops_metrics_reader ")
	assert.Contains(t, down, "md5(current_database())")
	assert.Contains(t, down, opsMetricsReaderOwnerMarker)
	assert.Contains(t, down, "shobj_description")
	assert.Contains(t, down, "GRANT TEMPORARY ON DATABASE %I TO PUBLIC")
	assert.Contains(t, down, "pg_shdepend")
	assert.Contains(t, down, "DROP ROLE %I")
	assert.NotContains(t, down, "DROP ROLE concord_ops_metrics_reader;")
	assert.Contains(t, readme, "| 000088 | ops_metrics_reader | Restricted admin metrics reader role (#1690) |")
}

func TestMigration000088_IntegrationOwnershipPrivilegesAndSymmetry(t *testing.T) {
	db := migration000086OpenIsolatedDB(t)
	ctx := context.Background()
	up := migration000088SQL(t, "up")
	down := migration000088SQL(t, "down")
	migration000088PrepareMetricsTables(t, db)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), down)
		_, _ = db.ExecContext(context.Background(), migration000086SQL(t, "down"))
		assert.NoError(t, db.Close())
	})

	_, err := db.ExecContext(ctx, down)
	require.NoError(t, err)
	publicTempBefore := migration000088PublicHasTemp(t, db)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	roleName := migration000088RoleName(t, db)
	require.True(t, strings.HasPrefix(roleName, opsMetricsReaderRolePrefix))
	var (
		canLogin, isSuperuser, canCreateDB, canCreateRole bool
		inherit, replication, bypassRLS                   bool
		connectionLimit                                   int
		comment                                           sql.NullString
	)
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolinherit,
		       rolreplication, rolbypassrls, rolconnlimit,
		       shobj_description(oid, 'pg_authid')
		FROM pg_roles
		WHERE rolname = $1
	`, roleName).Scan(
		&canLogin, &isSuperuser, &canCreateDB, &canCreateRole, &inherit,
		&replication, &bypassRLS, &connectionLimit, &comment,
	))
	assert.False(t, canLogin)
	assert.False(t, isSuperuser)
	assert.False(t, canCreateDB)
	assert.False(t, canCreateRole)
	assert.False(t, inherit)
	assert.False(t, replication)
	assert.False(t, bypassRLS)
	assert.Equal(t, 2, connectionLimit)
	expectedMarker := opsMetricsReaderOwnerMarker + migration000088DatabaseName(t, db) + ":public-temp="
	if publicTempBefore {
		expectedMarker += "1"
	} else {
		expectedMarker += "0"
	}
	assert.Equal(t, expectedMarker, comment.String)
	assert.Zero(t, migration000088MembershipCount(t, db, roleName))
	assert.False(t, migration000088HasUnexpectedTablePrivilege(t, db, roleName))
	assert.False(t, migration000088HasCreatePrivilege(t, db, roleName))
	assert.False(t, migration000088PublicHasTemp(t, db))

	_, err = db.ExecContext(ctx, down)
	require.NoError(t, err)
	assert.False(t, migration000088RoleExists(t, db, roleName))
	assert.Equal(t, publicTempBefore, migration000088PublicHasTemp(t, db))
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err, "migration must re-apply after rollback")
	assert.True(t, migration000088RoleExists(t, db, roleName))
}

func TestMigration000088_RejectsUnownedCollisionAndPrivilegeDrift(t *testing.T) {
	db := migration000086OpenIsolatedDB(t)
	ctx := context.Background()
	up := migration000088SQL(t, "up")
	down := migration000088SQL(t, "down")
	migration000088PrepareMetricsTables(t, db)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `
			DO $cleanup$
			DECLARE
				reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
				reader_oid oid;
				parent_oid oid;
			BEGIN
				SELECT oid INTO reader_oid FROM pg_roles WHERE rolname = reader_role;
				SELECT oid INTO parent_oid FROM pg_roles WHERE rolname = 'concord_ops_metrics_reader_parent_test';
				IF reader_oid IS NOT NULL AND parent_oid IS NOT NULL AND EXISTS (
					SELECT 1 FROM pg_auth_members
					WHERE roleid = parent_oid AND member = reader_oid
				) THEN
					EXECUTE format('REVOKE concord_ops_metrics_reader_parent_test FROM %I', reader_role);
				END IF;
				DROP ROLE IF EXISTS concord_ops_metrics_reader_parent_test;
			END
			$cleanup$;
		`)
		_, _ = db.ExecContext(context.Background(), `REVOKE CREATE ON SCHEMA public FROM PUBLIC`)
		_, _ = db.ExecContext(context.Background(), `ALTER DEFAULT PRIVILEGES REVOKE SELECT ON TABLES FROM PUBLIC`)
		_, _ = db.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS ops_metrics_reader_definer_test()`)
		_, _ = db.ExecContext(context.Background(), `DROP SEQUENCE IF EXISTS ops_metrics_reader_sequence_test`)
		_, downErr := db.ExecContext(context.Background(), down)
		assert.NoError(t, downErr)
		_, _ = db.ExecContext(context.Background(), migration000086SQL(t, "down"))
		assert.NoError(t, db.Close())
	})

	_, _ = db.ExecContext(ctx, down)
	_, err := db.ExecContext(ctx, `
		DO $collision$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('CREATE ROLE %I NOLOGIN', reader_role);
		END
		$collision$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "ownership marker")

	_, err = db.ExecContext(ctx, `
		DO $replace$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('DROP ROLE %I', reader_role);
		END
		$replace$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `CREATE ROLE concord_ops_metrics_reader_parent_test NOLOGIN`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		DO $membership$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('GRANT concord_ops_metrics_reader_parent_test TO %I', reader_role);
		END
		$membership$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "role memberships")
	_, err = db.ExecContext(ctx, `
		DO $membership$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('REVOKE concord_ops_metrics_reader_parent_test FROM %I', reader_role);
		END
		$membership$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP ROLE concord_ops_metrics_reader_parent_test`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		DO $attributes$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('ALTER ROLE %I CONNECTION LIMIT 3', reader_role);
		END
		$attributes$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "role attributes")
	_, err = db.ExecContext(ctx, `
		DO $attributes$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('ALTER ROLE %I CONNECTION LIMIT 2', reader_role);
		END
		$attributes$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO PUBLIC`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "unsafe default privileges")
	_, err = db.ExecContext(ctx, `ALTER DEFAULT PRIVILEGES REVOKE SELECT ON TABLES FROM PUBLIC`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		CREATE FUNCTION ops_metrics_reader_definer_test()
		RETURNS integer
		LANGUAGE sql
		SECURITY DEFINER
		AS 'SELECT 1'
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "security definer")
	_, err = db.ExecContext(ctx, `DROP FUNCTION ops_metrics_reader_definer_test()`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `GRANT CREATE ON SCHEMA public TO PUBLIC`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "effective CREATE")
	_, err = db.ExecContext(ctx, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		DO $column_privilege$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('GRANT UPDATE(value) ON ops_metric_samples TO %I', reader_role);
		END
		$column_privilege$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "column privileges")
	_, err = db.ExecContext(ctx, `
		DO $column_privilege$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('REVOKE UPDATE(value) ON ops_metric_samples FROM %I', reader_role);
		END
		$column_privilege$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `CREATE SEQUENCE ops_metrics_reader_sequence_test`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		DO $sequence_privilege$
		DECLARE reader_role text := 'concord_ops_metrics_reader_' || md5(current_database());
		BEGIN
			EXECUTE format('GRANT SELECT ON SEQUENCE ops_metrics_reader_sequence_test TO %I', reader_role);
		END
		$sequence_privilege$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.ErrorContains(t, err, "sequence privileges")
	_, err = db.ExecContext(ctx, `DROP SEQUENCE ops_metrics_reader_sequence_test`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)
}

func TestMigration000088_DatabaseScopedRoleAndCrossDatabaseDependency(t *testing.T) {
	primaryDB := migration000086OpenIsolatedDB(t)
	ctx := context.Background()
	up := migration000088SQL(t, "up")
	down := migration000088SQL(t, "down")
	migration000088PrepareMetricsTables(t, primaryDB)

	databaseURL := os.Getenv(opsMetricsTestDatabaseURL)
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	_, err = primaryDB.ExecContext(ctx, `DROP DATABASE IF EXISTS concord_opsmetrics_000088_peer WITH (FORCE)`)
	require.NoError(t, err)
	_, err = primaryDB.ExecContext(ctx, `CREATE DATABASE concord_opsmetrics_000088_peer`)
	require.NoError(t, err)
	parsed.Path = "/concord_opsmetrics_000088_peer"
	peerDB, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	require.NoError(t, peerDB.PingContext(ctx))
	t.Cleanup(func() {
		_, _ = peerDB.ExecContext(context.Background(), down)
		_, _ = peerDB.ExecContext(context.Background(), migration000086SQL(t, "down"))
		_ = peerDB.Close()
		_, _ = primaryDB.ExecContext(context.Background(), down)
		_, _ = primaryDB.ExecContext(context.Background(), migration000086SQL(t, "down"))
		_, _ = primaryDB.ExecContext(context.Background(), `DROP DATABASE IF EXISTS concord_opsmetrics_000088_peer WITH (FORCE)`)
		assert.NoError(t, primaryDB.Close())
	})

	migration000088PrepareMetricsTables(t, peerDB)
	_, _ = primaryDB.ExecContext(ctx, down)
	_, _ = peerDB.ExecContext(ctx, down)
	_, err = primaryDB.ExecContext(ctx, up)
	require.NoError(t, err)
	_, err = peerDB.ExecContext(ctx, up)
	require.NoError(t, err)
	primaryRole := migration000088RoleName(t, primaryDB)
	peerRole := migration000088RoleName(t, peerDB)
	require.NotEqual(t, primaryRole, peerRole)

	tx, err := peerDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT set_config('concord.test.reader_role', $1, true)`, primaryRole)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		DO $dependency$
		BEGIN
			EXECUTE format(
				'GRANT SELECT ON ops_metric_samples TO %I',
				current_setting('concord.test.reader_role')
			);
		END
		$dependency$;
	`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	_, err = primaryDB.ExecContext(ctx, down)
	require.Error(t, err, "cross-database dependency must block role deletion")
	assert.True(t, migration000088RoleExists(t, primaryDB, primaryRole))

	tx, err = peerDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT set_config('concord.test.reader_role', $1, true)`, primaryRole)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		DO $dependency$
		BEGIN
			EXECUTE format(
				'REVOKE SELECT ON ops_metric_samples FROM %I',
				current_setting('concord.test.reader_role')
			);
		END
		$dependency$;
	`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	_, err = primaryDB.ExecContext(ctx, down)
	require.NoError(t, err)
	assert.False(t, migration000088RoleExists(t, primaryDB, primaryRole))
	assert.True(t, migration000088RoleExists(t, peerDB, peerRole))
	_, err = primaryDB.ExecContext(ctx, up)
	require.NoError(t, err, "primary role must re-apply after safe rollback")
}

func migration000088PrepareMetricsTables(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx, migration000086SQL(t, "down"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, migration000086SQL(t, "up"))
	require.NoError(t, err)
}

func migration000088RoleName(t *testing.T, db *sql.DB) string {
	t.Helper()
	var roleName string
	require.NoError(t, db.QueryRow(`
		SELECT 'concord_ops_metrics_reader_' || md5(current_database())
	`).Scan(&roleName))
	return roleName
}

func migration000088DatabaseName(t *testing.T, db *sql.DB) string {
	t.Helper()
	var databaseName string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&databaseName))
	return databaseName
}

func migration000088RoleExists(t *testing.T, db *sql.DB, roleName string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, roleName).Scan(&exists))
	return exists
}

func migration000088MembershipCount(t *testing.T, db *sql.DB, roleName string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT count(*)
		FROM pg_auth_members
		WHERE member = (SELECT oid FROM pg_roles WHERE rolname = $1)
		   OR roleid = (SELECT oid FROM pg_roles WHERE rolname = $1)
	`, roleName).Scan(&count))
	return count
}

func migration000088HasUnexpectedTablePrivilege(t *testing.T, db *sql.DB, roleName string) bool {
	t.Helper()
	var unexpected bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.table_privileges
			WHERE grantee = $1
			  AND NOT (
				table_schema = 'public'
				AND table_name IN ('ops_metric_samples', 'ops_metric_rollups')
				AND privilege_type = 'SELECT'
			  )
		)
	`, roleName).Scan(&unexpected))
	return unexpected
}

func migration000088HasCreatePrivilege(t *testing.T, db *sql.DB, roleName string) bool {
	t.Helper()
	var canCreate bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_namespace
			WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\'
			  AND nspname <> 'information_schema'
			  AND has_schema_privilege($1, oid, 'CREATE')
		)
	`, roleName).Scan(&canCreate))
	return canCreate
}

func migration000088PublicHasTemp(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var hasTemp bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_database AS database
			CROSS JOIN LATERAL aclexplode(
				COALESCE(database.datacl, acldefault('d', database.datdba))
			) AS privilege
			WHERE database.datname = current_database()
			  AND privilege.grantee = 0
			  AND privilege.privilege_type = 'TEMPORARY'
		)
	`).Scan(&hasTemp))
	return hasTemp
}

func migration000088SQL(t *testing.T, direction string) string {
	t.Helper()
	return migration000088ReadFile(t, filepath.Join("..", "..", "migrations", "000088_ops_metrics_reader."+direction+".sql"))
}

func migration000088ReadFile(t *testing.T, relativePath string) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(filename), relativePath)
	// #nosec G304 -- path is based on runtime.Caller and fixed test-owned filenames.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
