package database_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000194 (#2272): logical login families and voice-auth evidence.
// 000190, 000191 and 000193 belong to other PRs; this file stands at 000194.
//
// Every behavioural test runs on migration000194Fresh, a transaction that runs
// the down and then the up FILE TEXT and is always rolled back, so the schema
// under test is the file, never whatever the shared database was migrated with.
//
// Ordering rule for every caller (as in migration_000189_test.go): call
// testhelpers.SetupTestDB(t) BEFORE migration000194Fresh, so the rollback runs
// before SetupTestDB's TRUNCATE cleanup.

const migration000194Restrict = "23001" // restrict_violation

// migration000194Tables lists every table the migration creates.
var migration000194Tables = []string{
	"login_family_rollout", "login_family_revocations", "login_families",
	"login_family_sids", "login_family_revocation_targets",
	"voice_auth_boot_fences", "voice_auth_connections", "voice_auth_room_bindings",
	"voice_auth_connection_closures", "voice_auth_room_closures",
}

func migration000194UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000194_add_login_family_authority.up.sql")
}

func migration000194DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000194_add_login_family_authority.down.sql")
}

func migration000194Fresh(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	down := migration000194DownSQL(t)
	up := migration000194UpSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })

	_, err = tx.Exec(down)
	require.NoError(t, err, "the down must run on the migrated schema")
	_, err = tx.Exec(up)
	require.NoError(t, err, "the up must run after the down")
	return tx
}

// migration000194Expect runs query in a savepoint and requires a PostgreSQL
// error with code, then rolls the savepoint back so tx stays usable.
func migration000194Expect(t *testing.T, tx *sql.Tx, code, contains, query string, args ...any) {
	t.Helper()
	_, err := tx.Exec(`SAVEPOINT m194`)
	require.NoError(t, err)
	_, err = tx.Exec(query, args...)
	migrationRequirePQError(t, err, code, "")
	if contains != "" {
		assert.Contains(t, err.Error(), contains)
	}
	_, err = tx.Exec(`ROLLBACK TO SAVEPOINT m194`)
	require.NoError(t, err)
}

func migration000194SeedBootAndFamily(t *testing.T, tx *sql.Tx) (boot, family, user uuid.UUID) {
	t.Helper()
	boot, family, user = uuid.New(), uuid.New(), uuid.New()
	_, err := tx.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES ($1)`, boot)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO login_families(family_id, user_id, credential_epoch) VALUES ($1, $2, '')`, family, user)
	require.NoError(t, err)
	return boot, family, user
}

func migration000194InsertConnection(tx *sql.Tx, boot, family, user uuid.UUID, socketID string) error {
	_, err := tx.Exec(`
		INSERT INTO voice_auth_connections(connection_generation, family_id, user_id, credential_epoch,
			node_boot_id, socket_id, registration_token_sha256, registration_body_sha256)
		VALUES ($1, $2, $3, '', $4, $5, repeat('a', 64), repeat('b', 64))`,
		uuid.New(), family, user, boot, socketID)
	return err
}

func migration000194Snapshot(t *testing.T, tx *sql.Tx) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range migration000194Tables {
		out[table] = migrationSchemaSnapshot(t, tx, table)
	}
	rows, err := tx.Query(`
		SELECT 'trigger ' || c.relname || ' ' || pg_get_triggerdef(t.oid)
		  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE NOT t.tgisinternal AND c.relname = ANY($1)
		UNION ALL
		SELECT 'function ' || p.proname || ' ' || md5(pg_get_functiondef(p.oid))
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = current_schema() AND p.proname LIKE 'login\_family\_%'
		 ORDER BY 1`, "{"+strings.Join(migration000194Tables, ",")+"}")
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		out["_objects"] = append(out["_objects"], line)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestMigration000194_UpDownUpRoundTrip(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000194Fresh(t, db)
	first := migration000194Snapshot(t, tx)
	for _, table := range migration000194Tables {
		assert.NotEmpty(t, first[table], table)
	}
	assert.NotEmpty(t, first["_objects"])

	_, err := tx.Exec(migration000194DownSQL(t))
	require.NoError(t, err)
	for _, table := range migration000194Tables {
		var present bool
		require.NoError(t, tx.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present))
		assert.False(t, present, "%s must be gone after the down", table)
	}
	var functions int
	require.NoError(t, tx.QueryRow(`
		SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = current_schema() AND p.proname LIKE 'login\_family\_%'`).Scan(&functions))
	assert.Zero(t, functions, "the down must drop every login_family_* function")

	_, err = tx.Exec(migration000194UpSQL(t))
	require.NoError(t, err)
	assert.Equal(t, first, migration000194Snapshot(t, tx), "down then up must restore the identical schema")
}

func TestMigration000194_DownRefusesWhenOwnEvidenceExists(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	down := migration000194DownSQL(t)

	t.Run("activated floor", func(t *testing.T) {
		tx := migration000194Fresh(t, db)
		_, err := tx.Exec(`UPDATE login_family_rollout SET activated_at = clock_timestamp(), receipt = '{}'`)
		require.NoError(t, err)
		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, migration000194Restrict, "")
		assert.Contains(t, err.Error(), "activated login-family floor")
	})
	t.Run("boot fence", func(t *testing.T) {
		tx := migration000194Fresh(t, db)
		_, err := tx.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES ($1)`, uuid.New())
		require.NoError(t, err)
		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, migration000194Restrict, "")
		assert.Contains(t, err.Error(), "voice_auth_boot_fences")
	})
	t.Run("login family", func(t *testing.T) {
		tx := migration000194Fresh(t, db)
		_, err := tx.Exec(`INSERT INTO login_families(family_id, user_id, credential_epoch) VALUES ($1, $2, '')`,
			uuid.New(), uuid.New())
		require.NoError(t, err)
		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, migration000194Restrict, "")
		assert.Contains(t, err.Error(), "login_families")
	})
	t.Run("connection closure", func(t *testing.T) {
		tx := migration000194Fresh(t, db)
		boot := uuid.New()
		_, err := tx.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES ($1)`, boot)
		require.NoError(t, err)
		_, err = tx.Exec(`
			INSERT INTO voice_auth_connection_closures(connection_generation, node_boot_id, socket_id,
				credential_epoch, variant, registration_token_sha256, registration_body_sha256, closed_at)
			VALUES ($1, $2, 's', '', 'unregistered', repeat('a', 64), repeat('b', 64), clock_timestamp())`,
			uuid.New(), boot)
		require.NoError(t, err)
		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, migration000194Restrict, "")
	})
}

// The down must not refuse because of pre-existing tables that hold rows on every
// running server: live calls write voice_enforcement_sessions (000154) and
// credential-epoch ejections (000151). Neither is created by this migration.
func TestMigration000194_DownSucceedsWithLiveVoiceEnforcementRows(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000194Fresh(t, db)

	_, err := tx.Exec(`
		INSERT INTO voice_enforcement_sessions(session_generation, node_boot_id, room_id, room_kind, user_id,
			credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'channel', $4, '', 'live-call-socket')`,
		uuid.New(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	_, err = tx.Exec(`
		INSERT INTO credential_epoch_voice_ejections(user_id, credential_epoch, superseded_credential_epoch)
		VALUES ($1, repeat('a', 32), '')`, uuid.New())
	require.NoError(t, err)

	_, err = tx.Exec(migration000194DownSQL(t))
	require.NoError(t, err, "live voice_enforcement_sessions / ejection rows must not refuse the down")

	var sessions, ejections int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions`).Scan(&sessions))
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections`).Scan(&ejections))
	assert.Equal(t, 1, sessions)
	assert.Equal(t, 1, ejections)
}

func TestMigration000194_DownFailsWithinLockTimeoutBehindAHeldLock(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	down := migration000194DownSQL(t)

	blocker, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, blocker) })
	_, err = blocker.Exec(`LOCK TABLE login_family_rollout IN ACCESS SHARE MODE`)
	require.NoError(t, err)

	start := time.Now()
	_, err = db.Exec(down)
	requireBoundedLockTimeout(t, start, err)
	require.NoError(t, blocker.Rollback())

	var present bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('login_family_rollout') IS NOT NULL`).Scan(&present))
	assert.True(t, present, "a timed-out down must change nothing")
}

func TestMigration000194_DownSetsBoundedTimeouts(t *testing.T) {
	down := migration000194DownSQL(t)
	assert.Contains(t, down, "SET LOCAL lock_timeout = '3s'")
	assert.Contains(t, down, "SET LOCAL statement_timeout =")
	assert.Less(t, strings.Index(down, "SET LOCAL statement_timeout"), strings.Index(down, "LOCK TABLE"),
		"the timeouts must be set before the first ACCESS EXCLUSIVE lock")
}

func TestMigration000194_RolloutFloorIsImmutableIncludingTruncate(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000194Fresh(t, db)

	_, err := tx.Exec(`UPDATE login_family_rollout SET activated_at = clock_timestamp(), receipt = '{"image":"a"}'`)
	require.NoError(t, err)

	migration000194Expect(t, tx, migration000194Restrict, "activation floor is immutable",
		`UPDATE login_family_rollout SET receipt = '{"image":"b"}'`)
	migration000194Expect(t, tx, migration000194Restrict, "activation floor is immutable",
		`UPDATE login_family_rollout SET activated_at = NULL, receipt = NULL`)
	migration000194Expect(t, tx, migration000194Restrict, "rollout singleton cannot be deleted",
		`DELETE FROM login_family_rollout`)
	migration000194Expect(t, tx, migration000194Restrict, "immutable", `TRUNCATE login_family_rollout`)
	migration000194Expect(t, tx, migration000194Restrict, "immutable", `TRUNCATE login_family_rollout, login_families CASCADE`)

	var activated bool
	require.NoError(t, tx.QueryRow(`SELECT activated_at IS NOT NULL AND receipt->>'image' = 'a' FROM login_family_rollout`).Scan(&activated))
	assert.True(t, activated, "the activated floor survives every rejected statement")

	// A disposable database resets through replica mode, which skips ordinary
	// triggers. This is the documented test-reset path (testdb.ExecReplica).
	_, err = tx.Exec(`SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = tx.Exec(`TRUNCATE login_family_rollout`)
	require.NoError(t, err)
}

func TestMigration000194_EveryEvidenceTableRefusesTruncate(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000194Fresh(t, db)
	for _, table := range migration000194Tables {
		migration000194Expect(t, tx, migration000194Restrict, "immutable", `TRUNCATE `+table+` CASCADE`)
	}
	// A cascading truncate from an unguarded referenced table reaches the guarded child.
	migration000194Expect(t, tx, migration000194Restrict, "immutable", `TRUNCATE voice_enforcement_sessions CASCADE`)
}

func TestMigration000194_SocketIDBoundMatchesVoiceEnforcementSessions(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000194Fresh(t, db)
	boot, family, user := migration000194SeedBootAndFamily(t, tx)

	require.NoError(t, migration000194InsertConnection(tx, boot, family, user, strings.Repeat("s", 255)),
		"a 255-octet socket id is valid for voice_enforcement_sessions and must be valid here")

	for name, socket := range map[string]string{"empty": "", "256 octets": strings.Repeat("s", 256)} {
		_, err := tx.Exec(`SAVEPOINT sock`)
		require.NoError(t, err)
		err = migration000194InsertConnection(tx, boot, family, user, socket)
		migrationRequirePQError(t, err, "23514", "")
		assert.Contains(t, err.Error(), "socket_id", name)
		_, err = tx.Exec(`ROLLBACK TO SAVEPOINT sock`)
		require.NoError(t, err)
	}

	// The closure tables copy the socket id, so they accept the same maximum.
	_, err := tx.Exec(`
		INSERT INTO voice_auth_connection_closures(connection_generation, node_boot_id, socket_id,
			credential_epoch, variant, registration_token_sha256, registration_body_sha256, closed_at)
		VALUES ($1, $2, $3, '', 'unregistered', repeat('a', 64), repeat('b', 64), clock_timestamp())`,
		uuid.New(), boot, strings.Repeat("s", 255))
	require.NoError(t, err)
}

// The closure insert trigger must serialise with boot retirement on the same
// advisory lock login_family_boot_guard takes, before it reads retired_at.
func TestMigration000194_ClosureInsertTakesBootGenerationLock(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000194Fresh(t, db)

	// A boot this transaction never fenced or locked: only the closure trigger
	// can have taken its advisory lock.
	boot := uuid.New()
	_, err := tx.Exec(`
		INSERT INTO voice_auth_connection_closures(connection_generation, node_boot_id, socket_id,
			credential_epoch, variant, registration_token_sha256, registration_body_sha256, closed_at)
		VALUES ($1, $2, 's', '', 'unregistered', repeat('a', 64), repeat('b', 64), clock_timestamp())`,
		uuid.New(), boot)
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(`
		SELECT count(*) FROM pg_locks
		 WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
		   AND ((classid::bigint << 32) | objid::bigint) =
		       hashtextextended('concord.login-family.v1/boot/' || $1::text, 0)`, boot).Scan(&n))
	assert.Equal(t, 1, n, "closure insert must hold the boot generation lock")
}
