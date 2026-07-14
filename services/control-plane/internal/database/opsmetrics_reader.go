package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	opsMetricsReaderRolePrefix        = "concord_ops_metrics_reader_"
	opsMetricsReaderOwnerMarkerPrefix = "concord-voice:ops-metrics-reader:v2:"
	opsMetricsReaderPoolSize          = 2
	opsMetricsReaderDrainPollInterval = 10 * time.Millisecond
	opsMetricsReaderCleanupTimeout    = 5 * time.Second
	opsMetricsReaderPeerProbeTimeout  = 3 * time.Second
)

var (
	opsMetricsReaderRoleNamePattern = regexp.MustCompile(`^concord_ops_metrics_reader_[0-9a-f]{32}$`)
	opsMetricsDatabaseNamePattern   = regexp.MustCompile(`^[a-z0-9_]+$`)
)

var opsMetricsReaderExpectedSettings = map[string]struct{}{
	"default_transaction_read_only=on":       {},
	"idle_in_transaction_session_timeout=5s": {},
	"lock_timeout=1s":                        {},
	"statement_timeout=3s":                   {},
}

type opsMetricsReaderRoleState struct {
	databaseName    string
	name            string
	oid             int64
	exists          bool
	canLogin        bool
	isSuperuser     bool
	inherits        bool
	canCreateRole   bool
	canCreateDB     bool
	canReplicate    bool
	bypassesRLS     bool
	connectionLimit int
	settings        []string
}

// OpsMetricsReaderConnection owns the isolated database pool used by the
// read-only admin metrics API.
type OpsMetricsReaderConnection struct {
	DB       *sql.DB
	adminDB  *sql.DB
	roleName string
}

// OpenOpsMetricsReaderConnection rotates a process-ephemeral role password and
// opens a pool that is physically limited to the aggregate metrics tables.
func OpenOpsMetricsReaderConnection(ctx context.Context, adminDB *sql.DB, databaseURL string) (*OpsMetricsReaderConnection, error) {
	if adminDB == nil {
		return nil, errors.New("operations metrics reader administrator database is required")
	}
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	if err != nil {
		return nil, err
	}
	if !roleState.exists {
		return nil, errors.New("operations metrics reader role is not provisioned")
	}
	if err := verifyOpsMetricsReaderProvisioning(ctx, adminDB, roleState); err != nil {
		return nil, err
	}

	password, err := generateOpsMetricsReaderPassword()
	if err != nil {
		return nil, errors.New("generate operations metrics reader credential")
	}
	readerURL, err := buildOpsMetricsReaderURL(databaseURL, password, roleState.name)
	if err != nil {
		return nil, errors.New("build operations metrics reader connection")
	}
	if err := activateOpsMetricsReaderLogin(ctx, adminDB, roleState.name, password); err != nil {
		return nil, err
	}
	if err := verifyOpsMetricsReaderDatabaseIsolation(ctx, adminDB, readerURL, roleState.databaseName); err != nil {
		return nil, reconcileOpsMetricsReaderLoginAfterFailure(adminDB, roleState.name, err)
	}

	readerDB, err := sql.Open("postgres", readerURL)
	if err != nil {
		return nil, reconcileOpsMetricsReaderLoginAfterFailure(
			adminDB,
			roleState.name,
			errors.New("open operations metrics reader database"),
		)
	}
	readerDB.SetMaxOpenConns(opsMetricsReaderPoolSize)
	readerDB.SetMaxIdleConns(1)
	readerDB.SetConnMaxLifetime(15 * time.Minute)
	readerDB.SetConnMaxIdleTime(time.Minute)

	cleanupFailure := func(operationErr error) (*OpsMetricsReaderConnection, error) {
		closeErr := readerDB.Close()
		return nil, reconcileOpsMetricsReaderLoginAfterFailure(
			adminDB,
			roleState.name,
			errors.Join(operationErr, closeErr),
		)
	}
	if err := readerDB.PingContext(ctx); err != nil {
		return cleanupFailure(fmt.Errorf("ping operations metrics reader database: %w", err))
	}
	if err := verifyOpsMetricsReaderPrivileges(ctx, readerDB, roleState); err != nil {
		return cleanupFailure(err)
	}

	return &OpsMetricsReaderConnection{DB: readerDB, adminDB: adminDB, roleName: roleState.name}, nil
}

// Close closes every reader session before disabling login and clearing the
// process-ephemeral password. Both operations are idempotent.
func (connection *OpsMetricsReaderConnection) Close(ctx context.Context) error {
	if connection == nil {
		return nil
	}
	var closeErr error
	if connection.DB != nil {
		closeErr = connection.DB.Close()
	}
	disableErr := disableOpsMetricsReaderLogin(ctx, connection.adminDB, connection.roleName)
	return errors.Join(closeErr, disableErr)
}

// EnsureOpsMetricsReaderLoginDisabled avoids privileged role-management SQL
// when the role is absent or already NOLOGIN. A login-enabled role is unsafe,
// so revocation remains strict and any failure is returned to the caller.
func EnsureOpsMetricsReaderLoginDisabled(ctx context.Context, adminDB *sql.DB) error {
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	if err != nil {
		return err
	}
	if !roleState.exists {
		return nil
	}
	if !roleState.canLogin {
		if err := terminateOpsMetricsReaderSessions(ctx, adminDB, roleState.name); err != nil {
			return fmt.Errorf("drain disabled operations metrics reader sessions: %w", err)
		}
		return nil
	}
	if err := disableOpsMetricsReaderLogin(ctx, adminDB, roleState.name); err != nil {
		return fmt.Errorf("revoke active operations metrics reader login: %w", err)
	}
	return nil
}

// DisableOpsMetricsReaderLogin revokes login capability and removes any
// previous password. It also closes stale sessions cluster-wide because roles
// and active sessions are PostgreSQL-cluster resources.
func DisableOpsMetricsReaderLogin(ctx context.Context, adminDB *sql.DB) error {
	roleState, err := inspectOpsMetricsReaderRole(ctx, adminDB)
	if err != nil {
		return err
	}
	if !roleState.exists {
		return nil
	}
	return disableOpsMetricsReaderLogin(ctx, adminDB, roleState.name)
}

func inspectOpsMetricsReaderRole(ctx context.Context, adminDB *sql.DB) (opsMetricsReaderRoleState, error) {
	if adminDB == nil {
		return opsMetricsReaderRoleState{}, errors.New("operations metrics reader administrator database is required")
	}

	var (
		state    opsMetricsReaderRoleState
		comment  sql.NullString
		settings pq.StringArray
	)
	err := adminDB.QueryRowContext(ctx, `
		WITH identity AS (
			SELECT
				current_database() AS database_name,
				'concord_ops_metrics_reader_' || md5(current_database()) AS role_name
		)
		SELECT
			identity.database_name,
			identity.role_name,
			COALESCE(role.oid, 0)::bigint,
			role.oid IS NOT NULL,
			COALESCE(role.rolcanlogin, false),
			COALESCE(role.rolsuper, false),
			COALESCE(role.rolinherit, false),
			COALESCE(role.rolcreaterole, false),
			COALESCE(role.rolcreatedb, false),
			COALESCE(role.rolreplication, false),
			COALESCE(role.rolbypassrls, false),
			COALESCE(role.rolconnlimit, 0),
			role.rolconfig,
			shobj_description(role.oid, 'pg_authid')
		FROM identity
		LEFT JOIN pg_roles AS role ON role.rolname = identity.role_name
	`).Scan(
		&state.databaseName,
		&state.name,
		&state.oid,
		&state.exists,
		&state.canLogin,
		&state.isSuperuser,
		&state.inherits,
		&state.canCreateRole,
		&state.canCreateDB,
		&state.canReplicate,
		&state.bypassesRLS,
		&state.connectionLimit,
		&settings,
		&comment,
	)
	if err != nil {
		return opsMetricsReaderRoleState{}, fmt.Errorf("inspect operations metrics reader role: %w", err)
	}
	state.settings = []string(settings)
	if !opsMetricsDatabaseNamePattern.MatchString(state.databaseName) ||
		!opsMetricsReaderRoleNamePattern.MatchString(state.name) {
		return opsMetricsReaderRoleState{}, errors.New("operations metrics reader role identity is invalid")
	}
	expectedMarkerPrefix := opsMetricsReaderOwnerMarkerPrefix + state.databaseName + ":public-temp="
	if state.exists && (!comment.Valid ||
		(comment.String != expectedMarkerPrefix+"0" && comment.String != expectedMarkerPrefix+"1")) {
		return opsMetricsReaderRoleState{}, errors.New("operations metrics reader role ownership marker is invalid")
	}
	return state, nil
}

func disableOpsMetricsReaderLogin(ctx context.Context, adminDB *sql.DB, roleName string) error {
	if adminDB == nil || !opsMetricsReaderRoleNamePattern.MatchString(roleName) {
		return errors.New("operations metrics reader disable configuration is invalid")
	}

	// Commit NOLOGIN before taking a session snapshot. Once this returns, a
	// holder of the previous process credential cannot establish a new session;
	// the drain below can then converge instead of racing reconnects.
	tx, err := adminDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operations metrics reader disable transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// PostgreSQL utility statements do not accept identifier parameters. The role
	// name is database-derived, pattern-validated above, and identifier-quoted.
	//nolint:gosec // The only dynamic value is a validated, quoted role identifier.
	disableSQL := `ALTER ROLE ` + pq.QuoteIdentifier(roleName) + ` NOLOGIN PASSWORD NULL`
	if _, err := tx.ExecContext(ctx, disableSQL); err != nil {
		return rollbackOpsMetricsReaderTransaction(tx, fmt.Errorf("disable operations metrics reader login: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operations metrics reader disable transaction: %w", err)
	}
	if err := terminateOpsMetricsReaderSessions(ctx, adminDB, roleName); err != nil {
		return err
	}
	return nil
}

func generateOpsMetricsReaderPassword() (string, error) {
	credential := make([]byte, 32)
	if _, err := rand.Read(credential); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(credential), nil
}

func buildOpsMetricsReaderURL(databaseURL, password, roleName string) (string, error) {
	if strings.TrimSpace(databaseURL) == "" || password == "" || !opsMetricsReaderRoleNamePattern.MatchString(roleName) {
		return "", errors.New("database URL, reader password, and role are required")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return "", errors.New("invalid database URL")
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", errors.New("database URL must use PostgreSQL")
	}
	if parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errors.New("database URL must include a host and database")
	}
	query, err := parseOpsMetricsReaderURLQuery(parsed.RawQuery)
	if err != nil {
		return "", err
	}
	parsed.RawQuery = query.Encode()
	parsed.User = url.UserPassword(roleName, password)
	return parsed.String(), nil
}

func parseOpsMetricsReaderURLQuery(rawQuery string) (url.Values, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, errors.New("database URL query is invalid")
	}
	for key, values := range query {
		if err := validateOpsMetricsReaderURLParameter(key, values); err != nil {
			return nil, err
		}
	}
	if query.Get("connect_timeout") == "" {
		query.Set("connect_timeout", "3")
	}
	return query, nil
}

func validateOpsMetricsReaderURLParameter(key string, values []string) error {
	if len(values) != 1 || values[0] == "" || strings.ContainsRune(values[0], '\x00') {
		return fmt.Errorf("database URL parameter %q is invalid", key)
	}
	value := values[0]
	switch key {
	case "sslmode":
		if !isAllowedOpsMetricsReaderSSLMode(value) {
			return errors.New("database URL sslmode is invalid")
		}
	case "sslcert", "sslkey", "sslrootcert", "sslcrl":
		// TLS material paths are trusted deployment configuration.
	case "channel_binding":
		if !isAllowedOpsMetricsReaderChannelBinding(value) {
			return errors.New("database URL channel_binding is invalid")
		}
	case "connect_timeout":
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 1 || seconds > 10 {
			return errors.New("database URL connect_timeout must be between 1 and 10 seconds")
		}
	default:
		return fmt.Errorf("database URL parameter %q is not allowed for the metrics reader", key)
	}
	return nil
}

func isAllowedOpsMetricsReaderSSLMode(value string) bool {
	switch value {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		return true
	default:
		return false
	}
}

func isAllowedOpsMetricsReaderChannelBinding(value string) bool {
	switch value {
	case "disable", "prefer", "require":
		return true
	default:
		return false
	}
}

func activateOpsMetricsReaderLogin(ctx context.Context, adminDB *sql.DB, roleName, password string) error {
	if adminDB == nil || !opsMetricsReaderRoleNamePattern.MatchString(roleName) || password == "" {
		return errors.New("operations metrics reader activation configuration is invalid")
	}
	// Rotation is two-phase. First revoke the old credential and drain every
	// cluster-wide session; only then publish the replacement credential. A
	// failed rotation therefore leaves the role NOLOGIN.
	if err := disableOpsMetricsReaderLogin(ctx, adminDB, roleName); err != nil {
		return reconcileOpsMetricsReaderLoginAfterFailure(
			adminDB,
			roleName,
			fmt.Errorf("prepare operations metrics reader activation: %w", err),
		)
	}
	tx, err := adminDB.BeginTx(ctx, nil)
	if err != nil {
		return reconcileOpsMetricsReaderLoginAfterFailure(
			adminDB,
			roleName,
			fmt.Errorf("begin operations metrics reader activation transaction: %w", err),
		)
	}
	defer func() { _ = tx.Rollback() }()
	failActivation := func(operationErr error) error {
		return reconcileOpsMetricsReaderLoginAfterFailure(adminDB, roleName, operationErr)
	}

	for _, statement := range []string{
		`SET LOCAL log_statement = 'none'`,
		`SET LOCAL log_min_error_statement = 'panic'`,
		`SET LOCAL log_min_duration_statement = -1`,
		`SET LOCAL log_parameter_max_length = 0`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return failActivation(rollbackOpsMetricsReaderTransaction(
				tx,
				fmt.Errorf("suppress operations metrics reader credential logging: %w", err),
			))
		}
	}

	// PostgreSQL utility statements do not accept bind parameters. The password
	// is Base64URL generated above and quoted as a SQL literal; the database-
	// derived role name is pattern-validated and identifier-quoted.
	//nolint:gosec // Both dynamic values are constrained and encoded for their SQL positions.
	activationSQL := `ALTER ROLE ` + pq.QuoteIdentifier(roleName) + ` LOGIN PASSWORD ` + pq.QuoteLiteral(password) + ` VALID UNTIL 'infinity'`
	if _, err := tx.ExecContext(ctx, activationSQL); err != nil {
		return failActivation(rollbackOpsMetricsReaderTransaction(
			tx,
			errors.New("activate operations metrics reader login"),
		))
	}
	return finishOpsMetricsReaderActivation(tx.Commit, failActivation)
}

func finishOpsMetricsReaderActivation(commit func() error, cleanup func(error) error) error {
	if err := commit(); err != nil {
		return cleanup(fmt.Errorf("commit operations metrics reader activation transaction: %w", err))
	}
	return nil
}

func reconcileOpsMetricsReaderLoginAfterFailure(
	adminDB *sql.DB,
	roleName string,
	operationErr error,
) error {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), opsMetricsReaderCleanupTimeout)
	defer cleanupCancel()
	cleanupErr := disableOpsMetricsReaderLogin(cleanupCtx, adminDB, roleName)
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("reconcile operations metrics reader after failure: %w", cleanupErr)
	}
	return errors.Join(operationErr, cleanupErr)
}

func terminateOpsMetricsReaderSessions(ctx context.Context, adminDB *sql.DB, roleName string) error {
	if adminDB == nil || !opsMetricsReaderRoleNamePattern.MatchString(roleName) {
		return errors.New("operations metrics reader session drain configuration is invalid")
	}

	zeroObservations := 0
	for {
		var attempted int
		if err := adminDB.QueryRowContext(ctx, `
			SELECT count(*)
			FROM (
				SELECT pg_terminate_backend(pid)
				FROM pg_stat_activity
				WHERE usename = $1
				  AND pid <> pg_backend_pid()
			) AS termination_attempts
		`, roleName).Scan(&attempted); err != nil {
			return fmt.Errorf("terminate stale operations metrics reader sessions: %w", err)
		}

		var remaining int
		if err := adminDB.QueryRowContext(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE usename = $1
			  AND pid <> pg_backend_pid()
		`, roleName).Scan(&remaining); err != nil {
			return fmt.Errorf("verify operations metrics reader session drain: %w", err)
		}
		if remaining == 0 {
			zeroObservations++
			if zeroObservations == 2 {
				return nil
			}
		} else {
			zeroObservations = 0
		}

		timer := time.NewTimer(opsMetricsReaderDrainPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("drain operations metrics reader sessions: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func verifyOpsMetricsReaderProvisioning(
	ctx context.Context,
	adminDB *sql.DB,
	state opsMetricsReaderRoleState,
) error {
	if state.oid == 0 || state.canLogin || state.isSuperuser || state.inherits ||
		state.canCreateRole || state.canCreateDB || state.canReplicate || state.bypassesRLS ||
		state.connectionLimit != opsMetricsReaderPoolSize ||
		!hasExactOpsMetricsReaderSettings(state.settings) {
		return errors.New("operations metrics reader role attributes are invalid")
	}

	var (
		membershipFree, noDatabaseRoleSettings          bool
		passwordIsNull                                  bool
		canConnect, noUnsafeDatabasePrivileges          bool
		noPublicTemp                                    bool
		publicSchemaUsage, noUnexpectedSchemaUsage      bool
		noSchemaCreate                                  bool
		hasExpectedTableAccess, noUnexpectedTableAccess bool
		noSequenceAccess, noUnsafeDefaultACLs           bool
		noCallableSecurityDefiner                       bool
		noUnexpectedDependencies, exactDependencies     bool
	)
	err := adminDB.QueryRowContext(ctx, `
		SELECT
			NOT EXISTS (
				SELECT 1
				FROM pg_auth_members
				WHERE member = $2::oid OR roleid = $2::oid
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_db_role_setting
				WHERE setrole = $2::oid
				  AND setdatabase <> 0
			),
			EXISTS (
				SELECT 1
				FROM pg_authid
				WHERE oid = $2::oid
				  AND rolpassword IS NULL
			),
			has_database_privilege($1, current_database(), 'CONNECT'),
			NOT has_database_privilege($1, current_database(), 'CREATE')
				AND NOT has_database_privilege($1, current_database(), 'TEMPORARY'),
			NOT EXISTS (
				SELECT 1
				FROM pg_database AS database
				CROSS JOIN LATERAL aclexplode(
					COALESCE(database.datacl, acldefault('d', database.datdba))
				) AS privilege
				WHERE database.datname = current_database()
				  AND privilege.grantee = 0
				  AND privilege.privilege_type = 'TEMPORARY'
			),
			has_schema_privilege($1, 'public', 'USAGE'),
			NOT EXISTS (
				SELECT 1
				FROM pg_namespace
				WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND nspname NOT IN ('information_schema', 'public')
				  AND has_schema_privilege($1, oid, 'USAGE')
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_namespace
				WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND nspname <> 'information_schema'
				  AND has_schema_privilege($1, oid, 'CREATE')
			),
			has_table_privilege($1, 'public.ops_metric_samples', 'SELECT')
				AND has_table_privilege($1, 'public.ops_metric_rollups', 'SELECT'),
			NOT EXISTS (
				SELECT 1
				FROM pg_class AS relation
				JOIN pg_namespace AS schema ON schema.oid = relation.relnamespace
				WHERE schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND schema.nspname <> 'information_schema'
				  AND relation.relkind IN ('r', 'p', 'v', 'm', 'f')
				  AND (
					(
						NOT (
							schema.nspname = 'public'
							AND relation.relname IN ('ops_metric_samples', 'ops_metric_rollups')
						)
						AND (
							has_table_privilege($1, relation.oid, 'SELECT')
							OR has_any_column_privilege($1, relation.oid, 'SELECT')
						)
					)
					OR has_table_privilege($1, relation.oid, 'INSERT')
					OR has_table_privilege($1, relation.oid, 'UPDATE')
					OR has_table_privilege($1, relation.oid, 'DELETE')
					OR has_table_privilege($1, relation.oid, 'TRUNCATE')
					OR has_table_privilege($1, relation.oid, 'REFERENCES')
					OR has_table_privilege($1, relation.oid, 'TRIGGER')
					OR has_any_column_privilege($1, relation.oid, 'INSERT')
					OR has_any_column_privilege($1, relation.oid, 'UPDATE')
					OR has_any_column_privilege($1, relation.oid, 'REFERENCES')
				  )
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_class AS sequence
				JOIN pg_namespace AS schema ON schema.oid = sequence.relnamespace
				WHERE sequence.relkind = 'S'
				  AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND schema.nspname <> 'information_schema'
				  AND (
					has_sequence_privilege($1, sequence.oid, 'USAGE')
					OR has_sequence_privilege($1, sequence.oid, 'SELECT')
					OR has_sequence_privilege($1, sequence.oid, 'UPDATE')
				  )
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_default_acl AS defaults
				CROSS JOIN LATERAL aclexplode(defaults.defaclacl) AS privilege
				WHERE privilege.grantee IN (0, $2::oid)
				  AND defaults.defaclobjtype IN ('r', 'S', 'f', 'T', 'n')
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_proc AS function
				JOIN pg_namespace AS schema ON schema.oid = function.pronamespace
				WHERE function.prosecdef
				  AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND schema.nspname <> 'information_schema'
				  AND has_schema_privilege($1, schema.oid, 'USAGE')
				  AND has_function_privilege($1, function.oid, 'EXECUTE')
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_shdepend AS dependency
				WHERE dependency.refclassid = 'pg_authid'::regclass
				  AND dependency.refobjid = $2::oid
				  AND NOT (
					dependency.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND dependency.deptype = 'a'
					AND dependency.objsubid = 0
					AND (
						(
							dependency.classid = 'pg_namespace'::regclass
							AND dependency.objid = 'public'::regnamespace
						)
						OR (
							dependency.classid = 'pg_class'::regclass
							AND dependency.objid IN (
								'public.ops_metric_samples'::regclass,
								'public.ops_metric_rollups'::regclass
							)
						)
					)
				  )
			),
			(
				SELECT count(*) = 3
				FROM pg_shdepend
				WHERE refclassid = 'pg_authid'::regclass
				  AND refobjid = $2::oid
			)
	`, state.name, state.oid).Scan(
		&membershipFree,
		&noDatabaseRoleSettings,
		&passwordIsNull,
		&canConnect,
		&noUnsafeDatabasePrivileges,
		&noPublicTemp,
		&publicSchemaUsage,
		&noUnexpectedSchemaUsage,
		&noSchemaCreate,
		&hasExpectedTableAccess,
		&noUnexpectedTableAccess,
		&noSequenceAccess,
		&noUnsafeDefaultACLs,
		&noCallableSecurityDefiner,
		&noUnexpectedDependencies,
		&exactDependencies,
	)
	if err != nil {
		return fmt.Errorf("verify operations metrics reader provisioning: %w", err)
	}
	if !membershipFree || !noDatabaseRoleSettings || !passwordIsNull || !canConnect ||
		!noUnsafeDatabasePrivileges || !noPublicTemp || !publicSchemaUsage ||
		!noUnexpectedSchemaUsage || !noSchemaCreate || !hasExpectedTableAccess ||
		!noUnexpectedTableAccess || !noSequenceAccess || !noUnsafeDefaultACLs ||
		!noCallableSecurityDefiner || !noUnexpectedDependencies || !exactDependencies {
		return errors.New("operations metrics reader provisioning boundary is invalid")
	}
	return nil
}

func hasExactOpsMetricsReaderSettings(settings []string) bool {
	if len(settings) != len(opsMetricsReaderExpectedSettings) {
		return false
	}
	for _, setting := range settings {
		if _, expected := opsMetricsReaderExpectedSettings[setting]; !expected {
			return false
		}
	}
	return true
}

func verifyOpsMetricsReaderDatabaseIsolation(
	ctx context.Context,
	adminDB *sql.DB,
	readerURL string,
	targetDatabase string,
) error {
	parsed, err := url.Parse(readerURL)
	if err != nil {
		return errors.New("parse operations metrics reader isolation URL")
	}
	rows, err := adminDB.QueryContext(ctx, `
		SELECT datname
		FROM pg_database
		WHERE datallowconn
		  AND datname <> $1
		ORDER BY datname
	`, targetDatabase)
	if err != nil {
		return fmt.Errorf("list operations metrics reader peer databases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	probed := 0
	for rows.Next() {
		var peerDatabase string
		if err := rows.Scan(&peerDatabase); err != nil {
			return fmt.Errorf("scan operations metrics reader peer database: %w", err)
		}
		if err := probeOpsMetricsReaderPeerDatabase(ctx, parsed, peerDatabase); err != nil {
			return err
		}
		probed++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate operations metrics reader peer databases: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close operations metrics reader peer database rows: %w", err)
	}
	if probed == 0 {
		return errors.New("operations metrics reader isolation could not be proven against a peer database")
	}
	return nil
}

func probeOpsMetricsReaderPeerDatabase(ctx context.Context, readerURL *url.URL, peerDatabase string) error {
	peerURL := *readerURL
	peerURL.Path = "/" + peerDatabase
	peerURL.RawPath = ""

	peerDB, err := sql.Open("postgres", peerURL.String())
	if err != nil {
		return errors.New("open operations metrics reader peer probe")
	}
	peerDB.SetMaxOpenConns(1)
	peerDB.SetMaxIdleConns(0)
	probeCtx, probeCancel := context.WithTimeout(ctx, opsMetricsReaderPeerProbeTimeout)
	pingErr := peerDB.PingContext(probeCtx)
	probeCancel()
	closeErr := peerDB.Close()
	if pingErr == nil {
		return errors.Join(
			fmt.Errorf("operations metrics reader connected to peer database %q", peerDatabase),
			closeErr,
		)
	}
	var postgresErr *pq.Error
	if !errors.As(pingErr, &postgresErr) || postgresErr.Code != "28000" {
		return errors.Join(
			fmt.Errorf("operations metrics reader peer database %q was not rejected by pg_hba.conf: %w", peerDatabase, pingErr),
			closeErr,
		)
	}
	if closeErr != nil {
		return fmt.Errorf("close operations metrics reader peer probe: %w", closeErr)
	}
	return nil
}

func verifyOpsMetricsReaderPrivileges(
	ctx context.Context,
	readerDB *sql.DB,
	state opsMetricsReaderRoleState,
) error {
	var (
		currentDatabase, currentUser, sessionUser  string
		defaultReadOnly, transactionReadOnly       string
		statementTimeout, lockTimeout, idleTimeout string
		roleAttributesValid                        bool
		readSamples, readRollups                   bool
		readUsers, readMessages                    bool
		readUserKeys, readAdminUsers               bool
		writeSamples, updateSamples, deleteSamples bool
		membershipFree, noUnsafeDatabasePrivileges bool
		publicSchemaUsage, noUnexpectedSchemaUsage bool
		noUnexpectedTableAccess, noSequenceAccess  bool
		noObjectOwnership, noSchemaCreate          bool
		noCallableSecurityDefiner                  bool
	)
	err := readerDB.QueryRowContext(ctx, `
		SELECT
			current_database(),
			current_user,
			session_user,
			current_setting('default_transaction_read_only'),
			current_setting('transaction_read_only'),
			current_setting('statement_timeout'),
			current_setting('lock_timeout'),
			current_setting('idle_in_transaction_session_timeout'),
			EXISTS (
				SELECT 1
				FROM pg_roles
				WHERE rolname = current_user
				  AND NOT rolsuper
				  AND NOT rolinherit
				  AND NOT rolcreaterole
				  AND NOT rolcreatedb
				  AND NOT rolreplication
				  AND NOT rolbypassrls
				  AND rolconnlimit = 2
				  AND cardinality(rolconfig) = 4
				  AND 'default_transaction_read_only=on' = ANY (rolconfig)
				  AND 'statement_timeout=3s' = ANY (rolconfig)
				  AND 'lock_timeout=1s' = ANY (rolconfig)
				  AND 'idle_in_transaction_session_timeout=5s' = ANY (rolconfig)
			),
			has_table_privilege(current_user, 'public.ops_metric_samples', 'SELECT'),
			has_table_privilege(current_user, 'public.ops_metric_rollups', 'SELECT'),
			has_table_privilege(current_user, 'public.users', 'SELECT'),
			has_table_privilege(current_user, 'public.messages', 'SELECT'),
			has_table_privilege(current_user, 'public.user_keys', 'SELECT'),
			has_table_privilege(current_user, 'public.admin_users', 'SELECT'),
			has_table_privilege(current_user, 'public.ops_metric_samples', 'INSERT'),
			has_table_privilege(current_user, 'public.ops_metric_samples', 'UPDATE'),
			has_table_privilege(current_user, 'public.ops_metric_samples', 'DELETE'),
			NOT EXISTS (
				SELECT 1
				FROM pg_auth_members
				WHERE member = (SELECT oid FROM pg_roles WHERE rolname = current_user)
				   OR roleid = (SELECT oid FROM pg_roles WHERE rolname = current_user)
			),
			NOT has_database_privilege(current_user, current_database(), 'CREATE')
				AND NOT has_database_privilege(current_user, current_database(), 'TEMPORARY'),
			has_schema_privilege(current_user, 'public', 'USAGE'),
			NOT EXISTS (
				SELECT 1
				FROM pg_namespace
				WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND nspname NOT IN ('information_schema', 'public')
				  AND has_schema_privilege(current_user, oid, 'USAGE')
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_class AS relation
				JOIN pg_namespace AS schema ON schema.oid = relation.relnamespace
				WHERE schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND schema.nspname <> 'information_schema'
				  AND relation.relkind IN ('r', 'p', 'v', 'm', 'f')
				  AND (
					(
						NOT (schema.nspname = 'public' AND relation.relname IN ('ops_metric_samples', 'ops_metric_rollups'))
						AND (
							has_table_privilege(current_user, relation.oid, 'SELECT')
							OR has_any_column_privilege(current_user, relation.oid, 'SELECT')
						)
					)
					OR has_table_privilege(current_user, relation.oid, 'INSERT')
					OR has_table_privilege(current_user, relation.oid, 'UPDATE')
					OR has_table_privilege(current_user, relation.oid, 'DELETE')
					OR has_table_privilege(current_user, relation.oid, 'TRUNCATE')
					OR has_table_privilege(current_user, relation.oid, 'REFERENCES')
					OR has_table_privilege(current_user, relation.oid, 'TRIGGER')
					OR has_any_column_privilege(current_user, relation.oid, 'INSERT')
					OR has_any_column_privilege(current_user, relation.oid, 'UPDATE')
					OR has_any_column_privilege(current_user, relation.oid, 'REFERENCES')
				  )
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_class AS sequence
				JOIN pg_namespace AS schema ON schema.oid = sequence.relnamespace
				WHERE sequence.relkind = 'S'
				  AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND schema.nspname <> 'information_schema'
				  AND (
					has_sequence_privilege(current_user, sequence.oid, 'USAGE')
					OR has_sequence_privilege(current_user, sequence.oid, 'SELECT')
					OR has_sequence_privilege(current_user, sequence.oid, 'UPDATE')
				  )
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_class AS relation
				JOIN pg_namespace AS schema ON schema.oid = relation.relnamespace
				WHERE relation.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
				  AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND schema.nspname <> 'information_schema'
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_namespace
				WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND nspname <> 'information_schema'
				  AND has_schema_privilege(current_user, oid, 'CREATE')
			),
			NOT EXISTS (
				SELECT 1
				FROM pg_proc AS function
				JOIN pg_namespace AS schema ON schema.oid = function.pronamespace
				WHERE function.prosecdef
				  AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				  AND schema.nspname <> 'information_schema'
				  AND has_schema_privilege(current_user, schema.oid, 'USAGE')
				  AND has_function_privilege(current_user, function.oid, 'EXECUTE')
			)
	`).Scan(
		&currentDatabase,
		&currentUser,
		&sessionUser,
		&defaultReadOnly,
		&transactionReadOnly,
		&statementTimeout,
		&lockTimeout,
		&idleTimeout,
		&roleAttributesValid,
		&readSamples,
		&readRollups,
		&readUsers,
		&readMessages,
		&readUserKeys,
		&readAdminUsers,
		&writeSamples,
		&updateSamples,
		&deleteSamples,
		&membershipFree,
		&noUnsafeDatabasePrivileges,
		&publicSchemaUsage,
		&noUnexpectedSchemaUsage,
		&noUnexpectedTableAccess,
		&noSequenceAccess,
		&noObjectOwnership,
		&noSchemaCreate,
		&noCallableSecurityDefiner,
	)
	if err != nil {
		return fmt.Errorf("verify operations metrics reader privileges: %w", err)
	}
	if currentDatabase != state.databaseName || currentUser != state.name || sessionUser != state.name ||
		defaultReadOnly != "on" || transactionReadOnly != "on" ||
		statementTimeout != "3s" || lockTimeout != "1s" || idleTimeout != "5s" ||
		!roleAttributesValid ||
		!readSamples || !readRollups || readUsers || readMessages || readUserKeys ||
		readAdminUsers || writeSamples || updateSamples || deleteSamples ||
		!membershipFree || !noUnsafeDatabasePrivileges || !publicSchemaUsage ||
		!noUnexpectedSchemaUsage || !noUnexpectedTableAccess || !noSequenceAccess ||
		!noObjectOwnership || !noSchemaCreate || !noCallableSecurityDefiner {
		return errors.New("operations metrics reader privilege boundary is invalid")
	}
	return nil
}

func rollbackOpsMetricsReaderTransaction(tx *sql.Tx, operationErr error) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return errors.Join(operationErr, fmt.Errorf("rollback operations metrics reader transaction: %w", err))
	}
	return operationErr
}
