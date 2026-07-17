package database

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var opsMetricsReaderPasswordPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

const (
	opsMetricsReaderTestDatabaseURL = "OPS_METRICS_TEST_DATABASE_URL"
	opsMetricsReaderFixtureRole     = "concord_ops_metrics_reader_0123456789abcdef0123456789abcdef"
	opsMetricsReaderTestPassword    = "temporary_reader-password_123" // pragma: allowlist secret -- isolated test-only credential
)

func TestGenerateOpsMetricsReaderPassword(t *testing.T) {
	first, err := generateOpsMetricsReaderPassword()
	require.NoError(t, err)
	second, err := generateOpsMetricsReaderPassword()
	require.NoError(t, err)

	assert.Regexp(t, opsMetricsReaderPasswordPattern, first)
	assert.Regexp(t, opsMetricsReaderPasswordPattern, second)
	assert.NotEqual(t, first, second)
}

func TestBuildOpsMetricsReaderURL(t *testing.T) {
	//nolint:gosec // Fixed non-secret fixture.
	source := "postgres://concord:old-password@postgres:5432/concord?sslmode=require&sslrootcert=%2Fcerts%2Froot.pem" // pragma: allowlist secret
	derived, err := buildOpsMetricsReaderURL(source, opsMetricsReaderTestPassword, opsMetricsReaderFixtureRole)
	require.NoError(t, err)

	parsed, err := url.Parse(derived)
	require.NoError(t, err)
	assert.Equal(t, "postgres", parsed.Scheme)
	assert.Equal(t, "postgres:5432", parsed.Host)
	assert.Equal(t, "/concord", parsed.Path)
	assert.Equal(t, "require", parsed.Query().Get("sslmode"))
	assert.Equal(t, "/certs/root.pem", parsed.Query().Get("sslrootcert"))
	assert.Equal(t, "3", parsed.Query().Get("connect_timeout"))
	assert.Len(t, parsed.Query(), 3)
	assert.Equal(t, opsMetricsReaderFixtureRole, parsed.User.Username())
	password, present := parsed.User.Password()
	assert.True(t, present)
	assert.Equal(t, opsMetricsReaderTestPassword, password)
	assert.Contains(t, source, "old-password", "source URL must remain unchanged")
}

func TestBuildOpsMetricsReaderURLRejectsInvalidInputs(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		databaseURL string
		password    string
		roleName    string
	}{
		{name: "empty URL", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "wrong scheme", databaseURL: "mysql://db/concord", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "missing host", databaseURL: "postgres:///concord", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "missing database", databaseURL: "postgres://db", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "empty password", databaseURL: "postgres://db/concord", roleName: opsMetricsReaderFixtureRole},
		{name: "empty role", databaseURL: "postgres://db/concord", password: "password"},
		{name: "invalid role", databaseURL: "postgres://db/concord", password: "password", roleName: "concord_ops_metrics_reader"},
		{name: "libpq options", databaseURL: "postgres://db/concord?options=-c%20role%3Dpostgres", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "statement timeout override", databaseURL: "postgres://db/concord?statement_timeout=0", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "read only override", databaseURL: "postgres://db/concord?default_transaction_read_only=off", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "duplicate parameter", databaseURL: "postgres://db/concord?sslmode=require&sslmode=disable", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "invalid connect timeout", databaseURL: "postgres://db/concord?connect_timeout=60", password: "password", roleName: opsMetricsReaderFixtureRole},
		{name: "malformed query", databaseURL: "postgres://db/concord?sslmode=%zz", password: "password", roleName: opsMetricsReaderFixtureRole},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := buildOpsMetricsReaderURL(testCase.databaseURL, testCase.password, testCase.roleName)
			assert.Error(t, err)
		})
	}
}

func TestFinishOpsMetricsReaderActivationCleansUpAmbiguousCommit(t *testing.T) {
	commitErr := errors.New("commit acknowledgement lost")
	cleanupCalled := false
	err := finishOpsMetricsReaderActivation(
		func() error { return commitErr },
		func(operationErr error) error {
			cleanupCalled = true
			assert.ErrorIs(t, operationErr, commitErr)
			return errors.Join(operationErr, errors.New("cleanup attempted"))
		},
	)

	assert.True(t, cleanupCalled)
	assert.ErrorIs(t, err, commitErr)
	assert.ErrorContains(t, err, "cleanup attempted")
}

func TestOpsMetricsReaderConnectionIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping restricted-login integration test", opsMetricsReaderTestDatabaseURL)
	}
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	require.Contains(t, strings.TrimPrefix(parsed.Path, "/"), "opsmetrics")

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)
	require.True(t, roleState.exists)

	connection, err := OpenOpsMetricsReaderConnection(ctx, adminDB, databaseURL)
	require.NoError(t, err)
	require.NotNil(t, connection)
	require.NotNil(t, connection.DB)
	assert.Equal(t, 2, connection.DB.Stats().MaxOpenConnections)

	var currentUser, sessionUser string
	require.NoError(t, connection.DB.QueryRowContext(ctx,
		`SELECT current_user, session_user`,
	).Scan(&currentUser, &sessionUser))
	assert.Equal(t, roleState.name, currentUser)
	assert.Equal(t, roleState.name, sessionUser)

	readerSession, err := connection.DB.Conn(ctx)
	require.NoError(t, err)
	// Prove the grants themselves deny writes. The role default remains read-only
	// in production, but this test removes that belt before testing the suspenders.
	_, err = readerSession.ExecContext(ctx, `SET default_transaction_read_only = off`)
	require.NoError(t, err)

	for _, query := range []string{
		`SELECT count(*) FROM ops_metric_samples`,
		`SELECT count(*) FROM ops_metric_rollups`,
	} {
		var count int
		require.NoError(t, readerSession.QueryRowContext(ctx, query).Scan(&count))
	}

	for _, query := range []string{
		`SELECT count(*) FROM users`,
		`SELECT count(*) FROM pending_registrations`,
		`SELECT count(*) FROM refresh_tokens`,
		`SELECT count(*) FROM messages`,
		`SELECT count(*) FROM user_keys`,
		`SELECT count(*) FROM admin_users`,
	} {
		_, queryErr := readerSession.ExecContext(ctx, query)
		assertPostgresPermissionDenied(t, queryErr)
	}

	for _, query := range []string{
		`INSERT INTO ops_metric_samples (node_id, metric_key, ts, value) VALUES ('cvn_aaaaaaaaaaaaaaaa', 'host_cpu_percent', now(), 1)`,
		`UPDATE ops_metric_samples SET value = 1`,
		`DELETE FROM ops_metric_samples`,
	} {
		_, queryErr := readerSession.ExecContext(ctx, query)
		assertPostgresPermissionDenied(t, queryErr)
	}
	require.NoError(t, readerSession.Close())

	require.NoError(t, connection.Close(ctx))
	require.NoError(t, connection.Close(ctx), "close must be idempotent")

	var canLogin bool
	var passwordIsNull bool
	require.NoError(t, adminDB.QueryRowContext(ctx, `
		SELECT rolcanlogin, rolpassword IS NULL
		FROM pg_authid
		WHERE rolname = $1
	`, roleState.name).Scan(&canLogin, &passwordIsNull))
	assert.False(t, canLogin)
	assert.True(t, passwordIsNull)
}

func TestOpenOpsMetricsReaderConnectionRejectsRoleMembershipIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping restricted-login membership test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)
	require.True(t, roleState.exists)

	const parentRole = "concord_ops_metrics_reader_parent_runtime_test"
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test role is quoted with pq.QuoteIdentifier
	_, err = adminDB.ExecContext(ctx, `DROP ROLE IF EXISTS `+pq.QuoteIdentifier(parentRole))
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test role is quoted with pq.QuoteIdentifier
	_, err = adminDB.ExecContext(ctx, `CREATE ROLE `+pq.QuoteIdentifier(parentRole)+` NOLOGIN`)
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- both fixed and migration-owned role identifiers are safely quoted
	_, err = adminDB.ExecContext(ctx,
		`GRANT `+pq.QuoteIdentifier(parentRole)+` TO `+pq.QuoteIdentifier(roleState.name),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- both fixed and migration-owned role identifiers are safely quoted
		_, _ = adminDB.ExecContext(context.Background(),
			`REVOKE `+pq.QuoteIdentifier(parentRole)+` FROM `+pq.QuoteIdentifier(roleState.name),
		)
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test role is quoted with pq.QuoteIdentifier
		_, cleanupErr := adminDB.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+pq.QuoteIdentifier(parentRole))
		assert.NoError(t, cleanupErr)
	})

	connection, err := OpenOpsMetricsReaderConnection(ctx, adminDB, databaseURL)
	assert.Nil(t, connection)
	require.ErrorContains(t, err, "boundary")
}

func TestOpenOpsMetricsReaderConnectionRejectsColumnPrivilegeIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping restricted-login column privilege test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)

	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- migration-owned role identifier is quoted with pq.QuoteIdentifier
	_, err = adminDB.ExecContext(ctx,
		`GRANT UPDATE(value) ON ops_metric_samples TO `+pq.QuoteIdentifier(roleState.name),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- migration-owned role identifier is quoted with pq.QuoteIdentifier
		_, cleanupErr := adminDB.ExecContext(context.Background(),
			// #nosec G202 -- pq.QuoteIdentifier safely quotes the database-derived role identifier.
			`REVOKE UPDATE(value) ON ops_metric_samples FROM `+pq.QuoteIdentifier(roleState.name),
		)
		assert.NoError(t, cleanupErr)
	})

	connection, err := OpenOpsMetricsReaderConnection(ctx, adminDB, databaseURL)
	assert.Nil(t, connection)
	require.ErrorContains(t, err, "boundary")
}

func TestOpenOpsMetricsReaderConnectionRejectsRoleAttributeDriftIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping restricted-login attribute test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- migration-owned role identifier is quoted with pq.QuoteIdentifier
	_, err = adminDB.ExecContext(ctx,
		`ALTER ROLE `+pq.QuoteIdentifier(roleState.name)+` CONNECTION LIMIT 3`,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- migration-owned role identifier is quoted with pq.QuoteIdentifier
		_, cleanupErr := adminDB.ExecContext(context.Background(),
			`ALTER ROLE `+pq.QuoteIdentifier(roleState.name)+` CONNECTION LIMIT 2`,
		)
		assert.NoError(t, cleanupErr)
	})

	connection, err := OpenOpsMetricsReaderConnection(ctx, adminDB, databaseURL)
	assert.Nil(t, connection)
	require.ErrorContains(t, err, "role attributes")
}

func TestOpenOpsMetricsReaderConnectionRejectsDefaultACLDriftIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping restricted-login default ACL test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	_, err = adminDB.ExecContext(ctx, `ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO PUBLIC`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := adminDB.ExecContext(context.Background(),
			`ALTER DEFAULT PRIVILEGES REVOKE SELECT ON TABLES FROM PUBLIC`,
		)
		assert.NoError(t, cleanupErr)
	})

	connection, err := OpenOpsMetricsReaderConnection(ctx, adminDB, databaseURL)
	assert.Nil(t, connection)
	require.ErrorContains(t, err, "provisioning boundary")
}

func TestOpsMetricsReaderLoginIsRejectedByPeerDatabaseIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping database-isolation test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)

	peerURL := createOpsMetricsReaderPeerDatabase(ctx, t, adminDB, databaseURL)
	require.NoError(t, activateOpsMetricsReaderLogin(ctx, adminDB, roleState.name, opsMetricsReaderTestPassword))

	primaryReaderURL, err := buildOpsMetricsReaderURL(databaseURL, opsMetricsReaderTestPassword, roleState.name)
	require.NoError(t, err)
	peerReaderURL, err := buildOpsMetricsReaderURL(peerURL, opsMetricsReaderTestPassword, roleState.name)
	require.NoError(t, err)
	primaryReader := openPinnedOpsMetricsReaderSession(ctx, t, primaryReaderURL)
	peerReader, err := sql.Open("postgres", peerReaderURL)
	require.NoError(t, err)
	peerErr := peerReader.PingContext(ctx)
	assertPostgresHBARejected(t, peerErr)
	require.NoError(t, peerReader.Close())

	require.NoError(t, disableOpsMetricsReaderLogin(ctx, adminDB, roleState.name))
	assertOpsMetricsReaderHasNoSessions(t, adminDB, roleState.name)
	assert.Error(t, primaryReader.PingContext(ctx))
}

func TestDisableOpsMetricsReaderLoginClosesConcurrentReconnectWindowIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping concurrent reader reconnect test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)

	require.NoError(t, activateOpsMetricsReaderLogin(ctx, adminDB, roleState.name, opsMetricsReaderTestPassword))
	readerURL, err := buildOpsMetricsReaderURL(databaseURL, opsMetricsReaderTestPassword, roleState.name)
	require.NoError(t, err)

	lockTx, err := adminDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	var roleOID uint32
	require.NoError(t, lockTx.QueryRowContext(ctx,
		`SELECT oid FROM pg_authid WHERE rolname = $1 FOR UPDATE`,
		roleState.name,
	).Scan(&roleOID))
	t.Cleanup(func() { _ = lockTx.Rollback() })

	disableURL, err := url.Parse(databaseURL)
	require.NoError(t, err)
	disableQuery := disableURL.Query()
	disableQuery.Set("application_name", "opsmetrics_reader_disable_race_test")
	disableURL.RawQuery = disableQuery.Encode()
	disableDB, err := sql.Open("postgres", disableURL.String())
	require.NoError(t, err)
	require.NoError(t, disableDB.PingContext(ctx))
	t.Cleanup(func() { assert.NoError(t, disableDB.Close()) })

	disableResult := make(chan error, 1)
	go func() {
		disableResult <- disableOpsMetricsReaderLogin(ctx, disableDB, roleState.name)
	}()
	waitForOpsMetricsReaderDisableLock(ctx, t, adminDB)

	reconnected := openPinnedOpsMetricsReaderSession(ctx, t, readerURL)
	require.NoError(t, lockTx.Commit())
	require.NoError(t, <-disableResult)

	assertOpsMetricsReaderHasNoSessions(t, adminDB, roleState.name)
	assert.Error(t, reconnected.PingContext(ctx))
}

func TestOpenOpsMetricsReaderConnectionRejectsNilDatabase(t *testing.T) {
	connection, err := OpenOpsMetricsReaderConnection(context.Background(), nil, "postgres://db/concord")
	assert.Nil(t, connection)
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "postgres://")
}

func TestEnsureOpsMetricsReaderLoginDisabledAcceptsMissingRoleIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping disabled-reader reconciliation test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	_, err = adminDB.ExecContext(ctx, opsMetricsReaderMigrationSQL(t, "000088_ops_metrics_reader.down.sql"))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := adminDB.ExecContext(context.Background(), opsMetricsReaderMigrationSQL(t, "000088_ops_metrics_reader.up.sql"))
		assert.NoError(t, cleanupErr)
	})

	require.NoError(t, EnsureOpsMetricsReaderLoginDisabled(ctx, adminDB))
}

func TestEnsureOpsMetricsReaderLoginDisabledDoesNotAlterSafeRoleIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping disabled-reader privilege test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)
	require.True(t, roleState.exists)
	_, err = adminDB.ExecContext(ctx, `DROP ROLE IF EXISTS concord_ops_metrics_reconcile_test`)
	require.NoError(t, err)
	_, err = adminDB.ExecContext(ctx, `CREATE ROLE concord_ops_metrics_reconcile_test NOLOGIN`)
	require.NoError(t, err)

	limitedURL, err := url.Parse(databaseURL)
	require.NoError(t, err)
	query := limitedURL.Query()
	query.Set("options", "-c role=concord_ops_metrics_reconcile_test")
	limitedURL.RawQuery = query.Encode()
	limitedDB, err := sql.Open("postgres", limitedURL.String())
	require.NoError(t, err)
	require.NoError(t, limitedDB.Ping())
	t.Cleanup(func() {
		assert.NoError(t, limitedDB.Close())
		_, cleanupErr := adminDB.ExecContext(context.Background(), `DROP ROLE IF EXISTS concord_ops_metrics_reconcile_test`)
		assert.NoError(t, cleanupErr)
	})

	var currentUser string
	require.NoError(t, limitedDB.QueryRowContext(ctx, `SELECT current_user`).Scan(&currentUser))
	require.Equal(t, "concord_ops_metrics_reconcile_test", currentUser)
	require.NoError(t, EnsureOpsMetricsReaderLoginDisabled(ctx, limitedDB))

	require.NoError(t, activateOpsMetricsReaderLogin(ctx, adminDB, roleState.name, opsMetricsReaderTestPassword))
	assertPostgresPermissionDenied(t, EnsureOpsMetricsReaderLoginDisabled(ctx, limitedDB))
	require.NoError(t, EnsureOpsMetricsReaderLoginDisabled(ctx, adminDB))

	var canLogin bool
	require.NoError(t, adminDB.QueryRowContext(ctx, `
		SELECT rolcanlogin
		FROM pg_roles
		WHERE rolname = $1
	`, roleState.name).Scan(&canLogin))
	require.False(t, canLogin)
}

func TestEnsureOpsMetricsReaderLoginDisabledDrainsAlreadyDisabledRoleIntegration(t *testing.T) {
	databaseURL := os.Getenv(opsMetricsReaderTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping disabled-reader session drain test", opsMetricsReaderTestDatabaseURL)
	}

	adminDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, adminDB.Ping())
	t.Cleanup(func() { assert.NoError(t, adminDB.Close()) })

	ctx := context.Background()
	prepareOpsMetricsReaderIntegrationSchema(ctx, t, adminDB)
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	require.NoError(t, err)

	require.NoError(t, activateOpsMetricsReaderLogin(ctx, adminDB, roleState.name, opsMetricsReaderTestPassword))
	readerURL, err := buildOpsMetricsReaderURL(databaseURL, opsMetricsReaderTestPassword, roleState.name)
	require.NoError(t, err)
	readerSession := openPinnedOpsMetricsReaderSession(ctx, t, readerURL)

	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- migration-owned role identifier is quoted with pq.QuoteIdentifier
	_, err = adminDB.ExecContext(ctx,
		`ALTER ROLE `+pq.QuoteIdentifier(roleState.name)+` NOLOGIN PASSWORD NULL`,
	)
	require.NoError(t, err)
	require.NoError(t, EnsureOpsMetricsReaderLoginDisabled(ctx, adminDB))

	assertOpsMetricsReaderHasNoSessions(t, adminDB, roleState.name)
	assert.Error(t, readerSession.PingContext(ctx))
}

func prepareOpsMetricsReaderIntegrationSchema(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.ExecContext(ctx, opsMetricsReaderMigrationSQL(t, "000086_ops_metrics.down.sql"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, opsMetricsReaderMigrationSQL(t, "000086_ops_metrics.up.sql"))
	require.NoError(t, err)

	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS users (test_id INTEGER)`,
		`CREATE TABLE IF NOT EXISTS pending_registrations (test_id INTEGER)`,
		`CREATE TABLE IF NOT EXISTS refresh_tokens (test_id INTEGER)`,
		`CREATE TABLE IF NOT EXISTS messages (test_id INTEGER)`,
		`CREATE TABLE IF NOT EXISTS user_keys (test_id INTEGER)`,
		`CREATE TABLE IF NOT EXISTS admin_users (test_id INTEGER)`,
	} {
		_, err = db.ExecContext(ctx, query)
		require.NoError(t, err)
	}

	_, err = db.ExecContext(ctx, opsMetricsReaderMigrationSQL(t, "000088_ops_metrics_reader.up.sql"))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, DisableOpsMetricsReaderLogin(context.Background(), db))
		_, cleanupErr := db.ExecContext(context.Background(), opsMetricsReaderMigrationSQL(t, "000088_ops_metrics_reader.down.sql"))
		assert.NoError(t, cleanupErr)
		_, cleanupErr = db.ExecContext(context.Background(), opsMetricsReaderMigrationSQL(t, "000086_ops_metrics.down.sql"))
		assert.NoError(t, cleanupErr)
	})
}

func createOpsMetricsReaderPeerDatabase(
	ctx context.Context,
	t *testing.T,
	adminDB *sql.DB,
	databaseURL string,
) string {
	t.Helper()
	const peerDatabase = "concord_opsmetrics_reader_peer_test"
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test database identifier is quoted with pq.QuoteIdentifier
	_, err := adminDB.ExecContext(ctx, `DROP DATABASE IF EXISTS `+pq.QuoteIdentifier(peerDatabase)+` WITH (FORCE)`)
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test database identifier is quoted with pq.QuoteIdentifier
	_, err = adminDB.ExecContext(ctx, `CREATE DATABASE `+pq.QuoteIdentifier(peerDatabase))
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test database identifier is quoted with pq.QuoteIdentifier
		_, cleanupErr := adminDB.ExecContext(context.Background(),
			`DROP DATABASE IF EXISTS `+pq.QuoteIdentifier(peerDatabase)+` WITH (FORCE)`,
		)
		assert.NoError(t, cleanupErr)
	})

	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	parsed.Path = "/" + peerDatabase
	return parsed.String()
}

func openPinnedOpsMetricsReaderSession(ctx context.Context, t *testing.T, databaseURL string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	require.NoError(t, db.PingContext(ctx))
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

func waitForOpsMetricsReaderDisableLock(ctx context.Context, t *testing.T, adminDB *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := adminDB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE application_name = 'opsmetrics_reader_disable_race_test'
				  AND wait_event_type = 'Lock'
			)
		`).Scan(&waiting)
		require.NoError(t, err)
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operations metrics reader disable did not reach the role lock")
}

func assertOpsMetricsReaderHasNoSessions(t *testing.T, adminDB *sql.DB, roleName string) {
	t.Helper()
	var sessionCount int
	require.NoError(t, adminDB.QueryRow(`
		SELECT count(*)
		FROM pg_stat_activity
		WHERE usename = $1
	`, roleName).Scan(&sessionCount))
	assert.Zero(t, sessionCount)
}

func opsMetricsReaderMigrationSQL(t *testing.T, name string) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "migrations", name)
	// #nosec G304 -- path is based on runtime.Caller and a fixed test-owned name.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}

func assertPostgresPermissionDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "expected PostgreSQL error, got %T", err)
	assert.Equal(t, pqerror.Code("42501"), pqErr.Code)
}

func assertPostgresHBARejected(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "expected PostgreSQL error, got %T", err)
	assert.Equal(t, pqerror.Code("28000"), pqErr.Code)
}
