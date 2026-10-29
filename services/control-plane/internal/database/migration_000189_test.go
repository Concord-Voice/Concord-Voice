package database_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000189 (#3680 C4 PR1, spec §6.1): the account-guard maturity schema.
// Maturity, version and enrolment-provenance columns on users and the four
// factor tables, the accountguard_maturity_stamp() default, one material
// trigger per table, and the accountguard_state activation marker. Nothing
// reads any of it before PR3.
//
// Every behavioural test runs on migration000189Fresh, a transaction that runs
// the down and then the up FILE TEXT and is always rolled back, so the schema
// under test is the file, never whatever the shared database was migrated with.
// That is what lets the AC6 mutations in the plan bite.
//
// Ordering rule for every caller: call testhelpers.SetupTestDB(t) BEFORE
// migration000189Fresh. SetupTestDB registers a t.Cleanup that TRUNCATEs, and
// t.Cleanup runs last-in first-out, so the rollback registered afterwards runs
// first and releases the ACCESS EXCLUSIVE locks the down took. Registered the
// other way round, or as a defer, the TRUNCATE waits on the open transaction's
// locks and deadlocks.

const agDeltaCP = 72 * time.Hour // PR3 rebinds this to decide.DeltaCP.

var (
	agLegacyMaturity = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC) // PR3 rebinds this to decide's legacy sentinel.
	agAwaitingNotice = time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC) // PR3 rebinds this to decide's awaiting-notice sentinel.
)

const (
	agRewrapGUC   = "concord.credential_rewrap"
	agRewrapValue = "mfa-rekey"
)

func migration000189UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000189_accountguard_maturity.up.sql")
}

func migration000189DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000189_accountguard_maturity.down.sql")
}

// migration000189Fresh opens a transaction, runs the 000189 down then up file
// text in it, and returns it. The rollback is registered with t.Cleanup (see
// the ordering rule above); the caller must already have called SetupTestDB.
func migration000189Fresh(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	down := migration000189DownSQL(t)
	up := migration000189UpSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })

	_, err = tx.Exec(down)
	require.NoError(t, err, "the down must run on the migrated schema")
	_, err = tx.Exec(up)
	require.NoError(t, err, "the up must run after the down")
	return tx
}

// migration000189InsertUser inserts a bare user (no dependent rows) with the
// given id. username is varchar(50), so 'ag' plus 32 hex characters.
func migration000189InsertUser(q migrationExecer, id uuid.UUID) error {
	_, err := q.Exec(
		`INSERT INTO users (id, email, username, password_hash) VALUES ($1, $2, $3, 'x')`,
		id, "ag-"+id.String()+"@example.test", "ag"+strings.ReplaceAll(id.String(), "-", ""))
	return err
}

// migration000189SeedUser inserts a bare user and returns its id.
func migration000189SeedUser(t *testing.T, tx *sql.Tx) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, migration000189InsertUser(tx, id))
	return id
}

// Seed byte values, kept as named constants because the identical-restore test
// replays them through the recovery-key upsert.
const (
	agTOTPEnc            = "enc-seed"
	agTOTPNonce          = "nonce-seed"
	agRecoveryPrivateKey = "rk-wrapped" // pragma: allowlist secret
	agRecoverySalt       = "rk-salt"
	agRecoveryPrefsKey   = "rk-prefs"
	agRecoveryPrefsSalt  = "rk-prefs-salt"
)

// migration000189SeedFactors inserts one row in each factor table for userID.
// The TOTP row starts disabled and unconfirmed.
func migration000189SeedFactors(t *testing.T, tx *sql.Tx, userID uuid.UUID) {
	t.Helper()
	_, err := tx.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed)
		VALUES ($1, $2, $3, FALSE, FALSE)`, userID, []byte(agTOTPEnc), []byte(agTOTPNonce))
	require.NoError(t, err)
	_, err = tx.Exec(`
		INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key)
		VALUES ($1, $2, $3)`, userID, []byte("cred-"+uuid.NewString()), []byte("pk-seed"))
	require.NoError(t, err)
	_, err = tx.Exec(`
		INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		VALUES ($1, 'google', $2, 'sso@example.test')`, userID, "sub-"+uuid.NewString())
	require.NoError(t, err)
	_, err = tx.Exec(`
		INSERT INTO user_recovery_keys
		    (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
		VALUES ($1, $2, $3, $4, $5)`, userID,
		[]byte(agRecoveryPrivateKey), []byte(agRecoverySalt), []byte(agRecoveryPrefsKey), []byte(agRecoveryPrefsSalt))
	require.NoError(t, err)
}

// agCred describes one credential whose maturity, version and provenance move
// together: the two users credentials and the four factor tables.
type agCred struct {
	Name, Table, Key                             string
	Version, Matures, Enrolled, Class, Contested string
	// Material is a SET clause that makes exactly one material change.
	Material string
}

var agCreds = []agCred{
	{
		Name: "users password", Table: "users", Key: "id",
		Version: "password_version", Matures: "password_matures_at",
		Enrolled: "password_enrolled_at", Class: "password_enrol_class",
		Material: "password_hash = 'y'",
	},
	{
		Name: "users email", Table: "users", Key: "id",
		Version: "email_version", Matures: "email_matures_at",
		Enrolled: "email_enrolled_at", Class: "email_enrol_class", Contested: "email_contested_at",
		Material: "email = 'ag-new-' || gen_random_uuid()::text || '@example.test'",
	},
	{
		Name: "user_mfa_totp", Table: "user_mfa_totp", Key: "user_id",
		Version: "material_version", Matures: "matures_at", Enrolled: "enrolled_at",
		Class: "enrol_class", Contested: "maturity_contested_at",
		Material: "totp_secret_enc = decode('aabbcc', 'hex')",
	},
	{
		Name: "user_mfa_webauthn", Table: "user_mfa_webauthn", Key: "user_id",
		Version: "material_version", Matures: "matures_at", Enrolled: "enrolled_at",
		Class: "enrol_class", Contested: "maturity_contested_at",
		Material: "public_key = decode('aabbcc', 'hex')",
	},
	{
		Name: "user_sso_identities", Table: "user_sso_identities", Key: "user_id",
		Version: "material_version", Matures: "matures_at", Enrolled: "enrolled_at",
		Class: "enrol_class", Contested: "maturity_contested_at",
		Material: "provider_user_id = 'sub-new-' || gen_random_uuid()::text",
	},
	{
		Name: "user_recovery_keys", Table: "user_recovery_keys", Key: "user_id",
		Version: "material_version", Matures: "matures_at", Enrolled: "enrolled_at",
		Class: "enrol_class", Contested: "maturity_contested_at",
		Material: "recovery_key_salt = decode('aabbcc', 'hex')",
	},
}

// agFactorTables are the four factor tables in the order the migration alters
// them (spec §6.1, system-engineer O5); users comes first.
var agFactorTables = []string{"user_mfa_totp", "user_mfa_webauthn", "user_sso_identities", "user_recovery_keys"}

func agCredFor(t *testing.T, table string) agCred {
	t.Helper()
	for _, c := range agCreds {
		if c.Table == table {
			return c
		}
	}
	require.Failf(t, "unknown credential table", "%s", table)
	return agCred{}
}

// agSQL builds a statement from test-owned identifiers. Every argument is a
// constant from this file, never input.
func agSQL(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// agState is the maturity, version and provenance of one credential.
type agState struct {
	Version  int64
	Matures  time.Time
	Enrolled time.Time
	Class    sql.NullInt64
}

func agStateCols(c agCred) string {
	return strings.Join([]string{c.Version, c.Matures, c.Enrolled, c.Class}, ", ")
}

func (s *agState) scanTargets() []any {
	return []any{&s.Version, &s.Matures, &s.Enrolled, &s.Class}
}

// agRowQuerier is the QueryRow surface agRead needs; every fixture is a *sql.Tx.
type agRowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// agRead returns the state of credential c for the row keyed by key.
func agRead(t *testing.T, q agRowQuerier, c agCred, key uuid.UUID) agState {
	t.Helper()
	var s agState
	err := q.QueryRow(agSQL(`SELECT %s FROM %s WHERE %s = $1`, agStateCols(c), c.Table, c.Key), key).Scan(s.scanTargets()...)
	require.NoError(t, err, "read %s state", c.Name)
	return s
}

// agUpdate runs UPDATE <table> SET <set> WHERE <key> = $1 and returns the state
// the trigger left in the new row (RETURNING sees BEFORE-trigger changes). A set
// clause may use $2 and later parameters, passed in args.
func agUpdate(t *testing.T, tx *sql.Tx, c agCred, set string, key uuid.UUID, args ...any) agState {
	t.Helper()
	var s agState
	q := agSQL(`UPDATE %s SET %s WHERE %s = $1 RETURNING %s`, c.Table, set, c.Key, agStateCols(c))
	err := tx.QueryRow(q, append([]any{key}, args...)...).Scan(s.scanTargets()...)
	require.NoError(t, err, "update %s SET %s", c.Name, set)
	return s
}

// agSavepoint runs fn between SAVEPOINT and ROLLBACK TO, so each case starts
// from the same fixture and a failed statement does not poison the transaction.
func agSavepoint(t *testing.T, tx *sql.Tx, fn func()) {
	t.Helper()
	_, err := tx.Exec(`SAVEPOINT ag_case`)
	require.NoError(t, err)
	fn()
	_, err = tx.Exec(`ROLLBACK TO SAVEPOINT ag_case`)
	require.NoError(t, err)
	_, err = tx.Exec(`RELEASE SAVEPOINT ag_case`)
	require.NoError(t, err)
}

// agNow and agStamp read the transaction's now() and now() + 72 hours from the
// database, so the comparison never depends on the Go clock.
func agNow(t *testing.T, tx *sql.Tx) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, tx.QueryRow(`SELECT now()`).Scan(&now))
	return now
}

func agStamp(t *testing.T, tx *sql.Tx) time.Time {
	t.Helper()
	var stamp time.Time
	require.NoError(t, tx.QueryRow(`SELECT now() + interval '72 hours'`).Scan(&stamp))
	return stamp
}

func agRequireTime(t *testing.T, want, got time.Time, what string) {
	t.Helper()
	assert.Truef(t, want.Equal(got), "%s: want %s, got %s", what, want.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano))
}

// agFixture is a Fresh transaction with three seeded users.
type agFixture struct {
	Tx *sql.Tx
	// User owns one row in each factor table.
	User uuid.UUID
	// Bare has no dependent rows. Updating users.id is only possible here:
	// the foreign keys from the factor tables are not ON UPDATE CASCADE.
	Bare uuid.UUID
	// Other is a second bare user, the target of any user_id move.
	Other uuid.UUID
	// Base is the version each credential (by agCred.Name) holds right after the
	// fixture is seeded. Every row's version came from
	// accountguard_material_version_seq at its INSERT (users included), so it is
	// some value >= 2 and no test may assume which.
	Base map[string]int64
}

// agWriterVersion is a writer-supplied version no sequence draw in these tests
// can reach, so "the writer's value was discarded" cannot pass by coincidence.
const agWriterVersion int64 = 987654321

// agMaterialVersion asserts that got is a legal version after one material
// change of c, given every version the slot has held before (oldest first). Every
// slot draws from accountguard_material_version_seq, whose first value is 2 (it
// keeps every draw apart from the legacy 1), and versions are equality-only: got
// must be at least 2 and must differ from every version the slot held before,
// never merely from the last. It reports whether the version was legal.
func agMaterialVersion(t *testing.T, c agCred, got int64, prior ...int64) bool {
	t.Helper()
	atLeastTwo := assert.GreaterOrEqual(t, got, int64(2), "%s version must come from the sequence", c.Name)
	fresh := assert.NotContains(t, prior, got, "%s version must differ from every version the slot held before", c.Name)
	return atLeastTwo && fresh
}

// migration000189Seeded builds the Fresh transaction and seeds the users, with
// maturity and enrolled_at set to the legacy sentinel and enrol_class to 2 on
// every credential, each at the version its INSERT drew. The provenance writes are non-material UPDATEs
// and fire no trigger.
func migration000189Seeded(t *testing.T) agFixture {
	t.Helper()
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)
	f := agFixture{
		Tx:    tx,
		User:  migration000189SeedUser(t, tx),
		Bare:  migration000189SeedUser(t, tx),
		Other: migration000189SeedUser(t, tx),
	}
	migration000189SeedFactors(t, tx, f.User)
	for _, id := range []uuid.UUID{f.User, f.Bare, f.Other} {
		migration000189SetLegacy(t, tx, id)
	}
	f.Base = map[string]int64{}
	for _, c := range agCreds {
		key := f.User
		if c.Table == "users" {
			key = f.Bare
		}
		f.Base[c.Name] = agRead(t, tx, c, key).Version
	}
	return f
}

func migration000189SetLegacy(t *testing.T, tx *sql.Tx, userID uuid.UUID) {
	t.Helper()
	for _, c := range agCreds {
		set := agSQL(`%s = $2, %s = $2, %s = 2`, c.Matures, c.Enrolled, c.Class)
		_, err := tx.Exec(agSQL(`UPDATE %s SET %s WHERE %s = $1`, c.Table, set, c.Key), userID, agLegacyMaturity)
		require.NoError(t, err, "legacy provenance for %s", c.Name)
	}
}

// agRequireLegacy asserts a credential still reads the seeded legacy state.
func agRequireLegacy(t *testing.T, s agState, version int64, what string) {
	t.Helper()
	assert.Equal(t, version, s.Version, "%s version", what)
	agRequireTime(t, agLegacyMaturity, s.Matures, what+" matures_at")
	agRequireTime(t, agLegacyMaturity, s.Enrolled, what+" enrolled_at")
	assert.Equal(t, sql.NullInt64{Int64: 2, Valid: true}, s.Class, "%s enrol_class", what)
}

// ---------------------------------------------------------------------------
// Task 1: the schema shell.
// ---------------------------------------------------------------------------

// agSchemaState is everything the round trip compares.
type agSchemaState struct {
	Tables    map[string][]string
	HasMarker bool
	Functions []string
}

var agSnapshotTables = append([]string{"users"}, agFactorTables...)

func agSnapshot(t *testing.T, tx *sql.Tx) agSchemaState {
	t.Helper()
	s := agSchemaState{Tables: map[string][]string{}}
	for _, table := range agSnapshotTables {
		s.Tables[table] = migrationSchemaSnapshot(t, tx, table)
	}
	require.NoError(t, tx.QueryRow(`SELECT to_regclass(quote_ident(current_schema()) || '.accountguard_state') IS NOT NULL`).Scan(&s.HasMarker))
	rows, err := tx.Query(`
		SELECT proname FROM pg_proc
		 WHERE pronamespace = to_regnamespace(current_schema()) AND proname LIKE 'accountguard\_%'
		 ORDER BY 1`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		s.Functions = append(s.Functions, name)
	}
	require.NoError(t, rows.Err())
	return s
}

func TestMigration000189_UpDownUpRoundTrip(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	up, down := migration000189UpSQL(t), migration000189DownSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })
	run := func(sqlText string) {
		t.Helper()
		_, execErr := tx.Exec(sqlText)
		require.NoError(t, execErr)
	}

	run(down)
	s0 := agSnapshot(t, tx)
	run(up)
	s1 := agSnapshot(t, tx)
	run(down)
	s0Again := agSnapshot(t, tx)
	run(up)
	s1Again := agSnapshot(t, tx)

	assert.Equal(t, s0, s0Again, "down must restore the pre-000189 shape exactly")
	assert.Equal(t, s1, s1Again, "up after down must rebuild the same shape")
	for _, down := range []agSchemaState{s0, s0Again} {
		assert.False(t, down.HasMarker, "accountguard_state must be gone after the down")
		assert.Empty(t, down.Functions, "no accountguard_* function may survive the down")
	}
	assert.True(t, s1.HasMarker)

	stamped := `timestamp with time zone - NO accountguard_maturity_stamp()`
	users := s1.Tables["users"]
	for _, line := range []string{
		"column password_matures_at " + stamped,
		"column email_matures_at " + stamped,
		"column email_contested_at timestamp with time zone - YES -",
		"column password_version bigint - NO 1",
		"column email_version bigint - NO 1",
		"column password_enrolled_at timestamp with time zone - NO now()",
		"column email_enrolled_at timestamp with time zone - NO now()",
		"column password_enrol_class smallint - YES -",
		"column email_enrol_class smallint - YES -",
	} {
		assert.Contains(t, users, line)
	}
	for _, table := range agFactorTables {
		for _, line := range []string{
			"column matures_at " + stamped,
			"column maturity_contested_at timestamp with time zone - YES -",
			"column material_version bigint - NO 1",
			"column enrolled_at timestamp with time zone - NO now()",
			"column enrol_class smallint - YES -",
		} {
			assert.Contains(t, s1.Tables[table], line, table)
		}
	}

	assert.Contains(t, users,
		"constraint users_matures_at_finite CHECK ((isfinite(password_matures_at) AND isfinite(email_matures_at))) NOT VALID")
	for _, table := range agSnapshotTables {
		for _, suffix := range []string{"matures_at_finite", "enrolled_at_finite", "enrol_class_range"} {
			name := table + "_" + suffix
			line := migrationSnapshotLine(s1.Tables[table], "constraint "+name+" ")
			require.NotEmpty(t, line, "missing constraint %s", name)
			assert.Contains(t, line, "NOT VALID", "%s must stay NOT VALID permanently", name)
		}
	}
}

func TestMigration000189_LegacyRowsReadMature(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	up, down := migration000189UpSQL(t), migration000189DownSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })
	_, err = tx.Exec(down)
	require.NoError(t, err)

	userID := migration000189SeedUser(t, tx)
	migration000189SeedFactors(t, tx, userID)
	_, err = tx.Exec(up)
	require.NoError(t, err)

	for _, c := range agCreds {
		s := agRead(t, tx, c, userID)
		assert.Equal(t, int64(1), s.Version, "%s version", c.Name)
		agRequireTime(t, agLegacyMaturity, s.Matures, c.Name+" matures_at")
		agRequireTime(t, agLegacyMaturity, s.Enrolled, c.Name+" enrolled_at")
		assert.False(t, s.Class.Valid, "%s enrol_class must be NULL for a legacy row", c.Name)
		if c.Contested != "" {
			var contested sql.NullTime
			require.NoError(t, tx.QueryRow(agSQL(`SELECT %s FROM %s WHERE %s = $1`, c.Contested, c.Table, c.Key), userID).Scan(&contested))
			assert.False(t, contested.Valid, "%s contested must be NULL", c.Name)
		}
	}
}

// Review Focus 1: the up is catalog-only on users and the factor tables.
func TestMigration000189_UpDoesNotRewriteUsers(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	up, down := migration000189UpSQL(t), migration000189DownSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })
	_, err = tx.Exec(down)
	require.NoError(t, err)

	filenodes := func() map[string]int64 {
		out := map[string]int64{}
		for _, table := range agSnapshotTables {
			var node int64
			require.NoError(t, tx.QueryRow(`SELECT pg_relation_filenode($1::text::regclass)::bigint`, table).Scan(&node))
			out[table] = node
		}
		return out
	}
	before := filenodes()
	_, err = tx.Exec(up)
	require.NoError(t, err)
	assert.Equal(t, before, filenodes(), "an ADD COLUMN with a volatile default or a validating CHECK rewrites the table under ACCESS EXCLUSIVE")
}

func TestMigration000189_NewRowsStampDeltaCP(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)
	userID := migration000189SeedUser(t, tx)
	migration000189SeedFactors(t, tx, userID)

	stamp, now := agStamp(t, tx), agNow(t, tx)
	for _, c := range agCreds {
		s := agRead(t, tx, c, userID)
		agRequireTime(t, stamp, s.Matures, c.Name+" matures_at")
		agRequireTime(t, now, s.Enrolled, c.Name+" enrolled_at")
		assert.False(t, s.Class.Valid, "%s enrol_class", c.Name)
		assert.GreaterOrEqual(t, s.Version, int64(2), "%s version comes from the sequence on INSERT", c.Name)
	}
}

func TestMigration000189_StampFunctionMatchesDeltaCP(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	var seconds int64
	require.NoError(t, tx.QueryRow(`SELECT EXTRACT(EPOCH FROM accountguard_maturity_stamp() - now())::bigint`).Scan(&seconds))
	assert.Equal(t, int64(agDeltaCP/time.Second), seconds)

	// Every maturity default must be the function, so PR7b's body swap reaches all six.
	for _, c := range agCreds {
		var def string
		err := tx.QueryRow(`
			SELECT pg_get_expr(d.adbin, d.adrelid)
			  FROM pg_attrdef d
			  JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
			 WHERE d.adrelid = $1::text::regclass AND a.attname = $2`, c.Table, c.Matures).Scan(&def)
		require.NoError(t, err, "%s.%s default", c.Table, c.Matures)
		assert.Equal(t, "accountguard_maturity_stamp()", def, "%s.%s", c.Table, c.Matures)
	}
}

func TestMigration000189_MaturesAtMustBeFinite(t *testing.T) {
	f := migration000189Seeded(t)
	for _, kind := range []struct {
		name       string
		column     func(agCred) string
		constraint string
	}{
		{"matures_at", func(c agCred) string { return c.Matures }, "matures_at_finite"},
		{"enrolled_at", func(c agCred) string { return c.Enrolled }, "enrolled_at_finite"},
	} {
		for _, c := range agCreds {
			t.Run(kind.name+"/"+c.Name, func(t *testing.T) {
				key := f.User
				if c.Table == "users" {
					key = f.Bare
				}
				col := kind.column(c)
				agSavepoint(t, f.Tx, func() {
					_, err := f.Tx.Exec(agSQL(`UPDATE %s SET %s = 'infinity' WHERE %s = $1`, c.Table, col, c.Key), key)
					migrationRequirePQError(t, err, "23514", c.Table+"_"+kind.constraint)
				})
				agSavepoint(t, f.Tx, func() {
					_, err := f.Tx.Exec(agSQL(`UPDATE %s SET %s = $2 WHERE %s = $1`, c.Table, col, c.Key), key, agAwaitingNotice)
					assert.NoError(t, err, "the awaiting-notice sentinel is finite and must be accepted")
				})
			})
		}
	}
}

func TestMigration000189_EnrolClassRange(t *testing.T) {
	f := migration000189Seeded(t)
	for _, c := range agCreds {
		t.Run(c.Name, func(t *testing.T) {
			key := f.User
			if c.Table == "users" {
				key = f.Bare
			}
			for _, bad := range []int{0, 4} {
				agSavepoint(t, f.Tx, func() {
					_, err := f.Tx.Exec(agSQL(`UPDATE %s SET %s = $2 WHERE %s = $1`, c.Table, c.Class, c.Key), key, bad)
					migrationRequirePQError(t, err, "23514", c.Table+"_enrol_class_range")
				})
			}
			for _, good := range []any{1, 2, 3, nil} {
				agSavepoint(t, f.Tx, func() {
					_, err := f.Tx.Exec(agSQL(`UPDATE %s SET %s = $2 WHERE %s = $1`, c.Table, c.Class, c.Key), key, good)
					assert.NoError(t, err, "enrol_class %v must be accepted", good)
				})
			}
		})
	}
}

func TestMigration000189_ActivationMarkerIsSingleton(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	var count int
	var allNull bool
	require.NoError(t, tx.QueryRow(`SELECT count(*), COALESCE(bool_and(activated_at IS NULL), FALSE) FROM accountguard_state`).Scan(&count, &allNull))
	assert.Equal(t, 1, count)
	assert.True(t, allNull, "the marker must start unset")

	agSavepoint(t, tx, func() {
		_, err := tx.Exec(`INSERT INTO accountguard_state DEFAULT VALUES`)
		migrationRequirePQError(t, err, "23505", "")
	})
	agSavepoint(t, tx, func() {
		_, err := tx.Exec(`INSERT INTO accountguard_state VALUES (false, NULL)`)
		migrationRequirePQError(t, err, "23514", "accountguard_state_singleton")
	})
}

func TestMigration000189_DownRefusesWhileActivated(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	down := migration000189DownSQL(t)
	tx := migration000189Fresh(t, db)

	_, err := tx.Exec(`UPDATE accountguard_state SET activated_at = now()`)
	require.NoError(t, err)

	_, err = tx.Exec(down)
	migrationRequirePQError(t, err, "23001", "")
	assert.Contains(t, err.Error(), "cannot roll back 000189 while the account guard is activated")
}

func TestMigration000189_DownRefusesWhileContested(t *testing.T) {
	f := migration000189Seeded(t)
	down := migration000189DownSQL(t)

	for _, c := range agCreds {
		if c.Contested == "" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			agSavepoint(t, f.Tx, func() {
				key := f.User
				if c.Table == "users" {
					key = f.Bare
				}
				_, err := f.Tx.Exec(agSQL(`UPDATE %s SET %s = now() WHERE %s = $1`, c.Table, c.Contested, c.Key), key)
				require.NoError(t, err)

				_, err = f.Tx.Exec(down)
				migrationRequirePQError(t, err, "23001", "")
				assert.Contains(t, err.Error(), "cannot roll back 000189 while a "+c.Table+" credential is contested")
			})
		})
	}
}

func TestMigration000189_DownGuardRunsUnderLock(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	down := migration000189DownSQL(t)

	userID := uuid.New()
	require.NoError(t, migration000189InsertUser(db, userID))
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID)
		assert.NoError(t, err)
	})
	_, err := db.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce) VALUES ($1, $2, $3)`,
		userID, []byte(agTOTPEnc), []byte(agTOTPNonce))
	require.NoError(t, err)

	migrationRequireGuardRunsUnderLock(t, db, down, "user_mfa_totp", "user_mfa_totp credential is contested",
		func(tx *sql.Tx) error {
			_, execErr := tx.Exec(`UPDATE user_mfa_totp SET maturity_contested_at = now() WHERE user_id = $1`, userID)
			return execErr
		})
}

// PostgreSQL-behaviour pin: passes by design, it needs no 000189 object. It
// pins the claim the migration's leading SET LOCAL lock_timeout rests on, that a
// SET LOCAL inside a multi-statement simple query (how golang-migrate sends a
// file, an implicit transaction block) governs the statements after it.
func TestMigration000189_SetLocalLockTimeoutAppliesInImplicitBlock(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := conn.ExecContext(ctx, `DROP TABLE IF EXISTS ag_lt_probe`)
		assert.NoError(t, dropErr)
		assert.NoError(t, conn.Close())
	})

	// No arguments: lib/pq then sends one simple query, as a migration file is sent.
	_, err = conn.ExecContext(ctx, `SET LOCAL lock_timeout = '3s'; CREATE TEMP TABLE ag_lt_probe AS SELECT current_setting('lock_timeout') AS v`)
	require.NoError(t, err)

	var v string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT v FROM ag_lt_probe`).Scan(&v))
	assert.Equal(t, "3s", v)
}

// ---------------------------------------------------------------------------
// Task 2: material triggers and the rewrap predicate.
// ---------------------------------------------------------------------------

func TestMigration000189_MaterialChangeStampsProvenance(t *testing.T) {
	f := migration000189Seeded(t)
	now, stamp := agNow(t, f.Tx), agStamp(t, f.Tx)

	for _, c := range agCreds {
		t.Run(c.Name, func(t *testing.T) {
			key := f.User
			if c.Table == "users" {
				key = f.Bare
			}
			agSavepoint(t, f.Tx, func() {
				s := agUpdate(t, f.Tx, c, c.Material, key)
				require.True(t, agMaterialVersion(t, c, s.Version, f.Base[c.Name]), "the change must be material")
				agRequireTime(t, stamp, s.Matures, "matures_at")
				agRequireTime(t, now, s.Enrolled, "enrolled_at")
				assert.False(t, s.Class.Valid, "enrol_class must be reset to unknown")

				if c.Table == "users" {
					for _, other := range agCreds {
						if other.Table == "users" && other.Name != c.Name {
							agRequireLegacy(t, agRead(t, f.Tx, other, key), f.Base[other.Name], other.Name+" must not move")
						}
					}
				}
			})
		})
	}
}

func TestMigration000189_ExplicitProvenanceIsKept(t *testing.T) {
	f := migration000189Seeded(t)
	now := agNow(t, f.Tx)
	explicit := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, c := range agCreds {
		t.Run(c.Name, func(t *testing.T) {
			key := f.User
			if c.Table == "users" {
				key = f.Bare
			}
			agSavepoint(t, f.Tx, func() {
				set := agSQL(`%s, %s = '2000-01-01Z', %s = 3`, c.Material, c.Enrolled, c.Class)
				s := agUpdate(t, f.Tx, c, set, key)
				require.True(t, agMaterialVersion(t, c, s.Version, f.Base[c.Name]))
				agRequireTime(t, explicit, s.Enrolled, "explicit enrolled_at")
				assert.Equal(t, sql.NullInt64{Int64: 3, Valid: true}, s.Class, "explicit enrol_class")
			})
			agSavepoint(t, f.Tx, func() {
				// A class equal to OLD reads as untouched, so it is nulled (the
				// explicit-means-distinct contract in the plan's Task 2 Produces).
				set := agSQL(`%s, %s = 2`, c.Material, c.Class)
				s := agUpdate(t, f.Tx, c, set, key)
				require.True(t, agMaterialVersion(t, c, s.Version, f.Base[c.Name]))
				assert.False(t, s.Class.Valid, "a class equal to OLD is not an explicit write")
				agRequireTime(t, now, s.Enrolled, "enrolled_at")
			})
			agSavepoint(t, f.Tx, func() {
				// The runner's alternative: a follow-up statement fires no trigger.
				moved := agUpdate(t, f.Tx, c, c.Material, key)
				require.True(t, agMaterialVersion(t, c, moved.Version, f.Base[c.Name]))
				s := agUpdate(t, f.Tx, c, c.Class+" = 2", key)
				assert.Equal(t, sql.NullInt64{Int64: 2, Valid: true}, s.Class, "follow-up class write")
				assert.Equal(t, moved.Version, s.Version, "the follow-up must not bump the version")
			})
		})
	}
}

func TestMigration000189_NonMaterialChangeKeepsProvenance(t *testing.T) {
	f := migration000189Seeded(t)

	cases := []struct {
		cred agCred
		sql  string
		key  uuid.UUID
	}{
		// These write a column outside every trigger's OF list, so the trigger
		// does not fire at all.
		{agCredFor(t, "user_mfa_webauthn"), `sign_count = sign_count + 1`, f.User},
		{agCredFor(t, "user_mfa_totp"), `last_used_step = 1`, f.User},
		{agCreds[0], `mfa_methods = '{totp}'`, f.Bare},
		// These write a material column to the value it already holds, so the
		// trigger FIRES and finds nothing distinct. The writer-supplied version
		// that rides along proves it fired: a non-material change keeps OLD's, so
		// it comes back as the base, not as agWriterVersion.
		{agCreds[0], agSQL(`password_hash = password_hash, password_version = %d`, agWriterVersion), f.Bare}, // pragma: allowlist secret
		{agCreds[1], agSQL(`email = email, email_version = %d`, agWriterVersion), f.Bare},
		{agCredFor(t, "user_mfa_totp"), agSQL(`totp_secret_enc = totp_secret_enc, material_version = %d`, agWriterVersion), f.User}, // pragma: allowlist secret
		{agCredFor(t, "user_mfa_webauthn"), agSQL(`public_key = public_key, material_version = %d`, agWriterVersion), f.User},
		{agCredFor(t, "user_sso_identities"), agSQL(`provider_user_id = provider_user_id, material_version = %d`, agWriterVersion), f.User},
		{agCredFor(t, "user_recovery_keys"), agSQL(`recovery_key_salt = recovery_key_salt, material_version = %d`, agWriterVersion), f.User},
	}
	for _, tc := range cases {
		t.Run(tc.cred.Table+"/"+tc.sql, func(t *testing.T) {
			agSavepoint(t, f.Tx, func() {
				s := agUpdate(t, f.Tx, tc.cred, tc.sql, tc.key)
				agRequireLegacy(t, s, f.Base[tc.cred.Name], tc.cred.Name)
			})
		})
	}
}

// PostgreSQL-behaviour pin (E4): passes by design, it needs no 000189 object.
// After a transaction-local set_config commits, the same session reads ” and
// not NULL, which is why every reader wraps the GUC in NULLIF(..., ”).
func TestMigration000189_GUCReadsEmptyAfterLocalSetCommits(t *testing.T) {
	testhelpers.SetupTestDB(t)
	ctx := context.Background()

	// A dedicated one-connection pool: a never-used session, so the first read
	// is the genuinely never-set state and not a recycled pooled connection's.
	pool, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { assert.NoError(t, pool.Close()) })
	conn, err := pool.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })

	read := func() sql.NullString {
		var v sql.NullString
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT current_setting($1, true)`, agRewrapGUC).Scan(&v))
		return v
	}
	nullifiedIsNull := func() bool {
		var isNull bool
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT NULLIF(current_setting($1, true), '') IS NULL`, agRewrapGUC).Scan(&isNull))
		return isNull
	}

	// State 1: never set on this session.
	assert.False(t, read().Valid, "a never-set GUC reads NULL")
	assert.True(t, nullifiedIsNull())

	// State 2: set transaction-locally, committed.
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, agRewrapGUC, agRewrapValue)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	after := read()
	assert.True(t, after.Valid, "after a committed local set the GUC reads empty, not NULL")
	assert.Equal(t, "", after.String)
	assert.True(t, nullifiedIsNull())

	// State 3: an autocommit local set, then the next statement.
	_, err = conn.ExecContext(ctx, `SELECT set_config($1, $2, true)`, agRewrapGUC, agRewrapValue)
	require.NoError(t, err)
	assert.True(t, nullifiedIsNull())
}

func TestMigration000189_IsRewrapRequiresExactValue(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	isRewrap := func() bool {
		var ok bool
		require.NoError(t, tx.QueryRow(`SELECT accountguard_is_rewrap()`).Scan(&ok))
		return ok
	}
	agSavepoint(t, tx, func() {
		assert.False(t, isRewrap(), "unset")
	})
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{agRewrapValue, true},
		{"MFA-REKEY", false},
		{agRewrapValue + " ", false},
		{"other", false},
	} {
		agSavepoint(t, tx, func() {
			_, err := tx.Exec(`SELECT set_config($1, $2, true)`, agRewrapGUC, tc.value)
			require.NoError(t, err)
			assert.Equal(t, tc.want, isRewrap(), "value %q", tc.value)
		})
	}
}

func TestMigration000189_RewrapExemptsOnlyRewrapColumns(t *testing.T) {
	f := migration000189Seeded(t)
	totp := agCredFor(t, "user_mfa_totp")
	stamp := agStamp(t, f.Tx)

	const rewrap = `totp_secret_enc = decode('aabbcc', 'hex'), totp_secret_nonce = decode('ddeeff', 'hex'), ` +
		`key_version = 2, updated_at = now() + interval '1 second'`
	const rewrapWithoutKeyVersion = `totp_secret_enc = decode('aabbcc', 'hex'), totp_secret_nonce = decode('ddeeff', 'hex'), ` +
		`updated_at = now() + interval '1 second'`
	cases := []struct {
		name     string
		flag     string // "" leaves the GUC alone
		set      string
		material bool
	}{
		{"flag set, rewrap columns only", agRewrapValue, rewrap, false},
		{"no flag, same change", "", rewrap, true},
		{"flag set, secret plus enabled", agRewrapValue, rewrap + `, enabled = TRUE`, true},
		{"flag set, secret plus backup_codes_hash", agRewrapValue, rewrap + `, backup_codes_hash = ARRAY['h']`, true},
		{"flag other, secret change", "other", rewrap, true},
		// RT-2b: a rewrap always moves key_version (mfa-rekey selects rows whose
		// key_version differs from the active one). A flagged swap that leaves it
		// alone is a new secret under the same key, so it is material.
		{"flag set, secret swap that omits key_version", agRewrapValue, rewrapWithoutKeyVersion, true},
		{"flag set, secret swap that rewrites key_version to its own value", agRewrapValue, rewrapWithoutKeyVersion + `, key_version = key_version`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agSavepoint(t, f.Tx, func() {
				if tc.flag != "" {
					_, err := f.Tx.Exec(`SELECT set_config($1, $2, true)`, agRewrapGUC, tc.flag)
					require.NoError(t, err)
				}
				s := agUpdate(t, f.Tx, totp, tc.set, f.User)
				if tc.material {
					agMaterialVersion(t, totp, s.Version, f.Base[totp.Name])
					agRequireTime(t, stamp, s.Matures, "a material change stamps maturity")
				} else {
					assert.Equal(t, f.Base[totp.Name], s.Version, "a rewrap keeps the version")
					agRequireTime(t, agLegacyMaturity, s.Matures, "a rewrap must leave maturity alone")
				}
			})
		})
	}
}

func TestMigration000189_TOTPEnableAndConfirmAreMaterialOnlyUpward(t *testing.T) {
	f := migration000189Seeded(t)
	totp := agCredFor(t, "user_mfa_totp")

	steps := []struct {
		set      string
		material bool
	}{
		{"enabled = TRUE", true},
		{"enabled = FALSE", false},
		{"confirmed = TRUE", true},
		{"confirmed = FALSE", false},
	}
	seen := []int64{f.Base[totp.Name]}
	for _, step := range steps {
		s := agUpdate(t, f.Tx, totp, step.set, f.User)
		if step.material {
			agMaterialVersion(t, totp, s.Version, seen...)
			seen = append(seen, s.Version)
		} else {
			assert.Equal(t, seen[len(seen)-1], s.Version, "%s is not material downward", step.set)
		}
	}
}

// Review Focus 5: the setup upsert reaches an enabled TOTP row through
// ON CONFLICT DO UPDATE (mfa/handlers.go TOTPSetup), not a plain UPDATE.
func TestMigration000189_TOTPSetupUpsertIsMaterial(t *testing.T) {
	f := migration000189Seeded(t)
	totp := agCredFor(t, "user_mfa_totp")

	enabledState := agUpdate(t, f.Tx, totp, `enabled = TRUE, confirmed = TRUE`, f.User)
	before := agUpdate(t, f.Tx, totp, `matures_at = '1970-01-01Z'`, f.User)
	agRequireTime(t, agLegacyMaturity, before.Matures, "maturity reset")
	require.Equal(t, enabledState.Version, before.Version, "a maturity-only write keeps the version")

	var version int64
	var enabled bool
	var matures time.Time
	err := f.Tx.QueryRow(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, FALSE, FALSE)
		ON CONFLICT (user_id) DO UPDATE SET
			totp_secret_enc = EXCLUDED.totp_secret_enc, -- pragma: allowlist secret
			totp_secret_nonce = EXCLUDED.totp_secret_nonce, -- pragma: allowlist secret
			key_version = EXCLUDED.key_version,
			enabled = FALSE,
			confirmed = FALSE,
			verified_at = NULL,
			confirmed_at = NULL,
			backup_codes_hash = '{}',
			backup_codes_used = '{}',
			last_used_step = NULL,
			updated_at = NOW()
		RETURNING material_version, enabled, matures_at`,
		f.User, []byte("new-enc"), []byte("new-nonce"), 1).Scan(&version, &enabled, &matures)
	require.NoError(t, err)

	// The version is equality-only. The conflict path spends a sequence value on
	// the BEFORE INSERT and draws another on the BEFORE UPDATE, so the row must
	// carry the sequence's most recent value, read in this same session, and a
	// value its slot never held.
	agMaterialVersion(t, totp, version, f.Base[totp.Name], before.Version)
	var lastValue int64
	require.NoError(t, f.Tx.QueryRow(`SELECT last_value FROM accountguard_material_version_seq`).Scan(&lastValue))
	assert.Equal(t, lastValue, version, "the upsert's version is the sequence's latest draw")
	assert.False(t, enabled)
	agRequireTime(t, agStamp(t, f.Tx), matures, "the upsert must stamp maturity")
}

func TestMigration000189_WriterSuppliedMaturesAtIsKept(t *testing.T) {
	f := migration000189Seeded(t)
	pw, em := agCreds[0], agCreds[1]

	s := agUpdate(t, f.Tx, pw, `password_hash = 'y', password_matures_at = '2000-01-01Z'`, f.Bare)
	require.True(t, agMaterialVersion(t, pw, s.Version, f.Base[pw.Name]), "the password change must be material")
	agRequireTime(t, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), s.Matures, "a writer-supplied matures_at is kept")

	e := agUpdate(t, f.Tx, em, `email_matures_at = '2001-01-01Z'`, f.Bare)
	assert.Equal(t, f.Base[em.Name], e.Version, "a maturity-only write is not a material change")
}

// E5: on a material change a writer-supplied matures_at survives, on every
// credential (the trigger stamps only when the writer left matures_at alone).
func TestMigration000189_WriterSuppliedMaturesAtKeptOnMaterialChange(t *testing.T) {
	f := migration000189Seeded(t)
	far := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, c := range agCreds {
		t.Run(c.Name, func(t *testing.T) {
			key := f.User
			if c.Table == "users" {
				key = f.Bare
			}
			agSavepoint(t, f.Tx, func() {
				s := agUpdate(t, f.Tx, c, agSQL(`%s, %s = '2999-01-01Z'`, c.Material, c.Matures), key)
				require.True(t, agMaterialVersion(t, c, s.Version, f.Base[c.Name]), "the change must be material")
				agRequireTime(t, far, s.Matures, "a writer-supplied matures_at is kept on a material change")
			})
		})
	}
}

func TestMigration000189_VersionColumnsMoveOnlyThroughTrigger(t *testing.T) {
	f := migration000189Seeded(t)

	for _, c := range agCreds {
		t.Run(c.Name, func(t *testing.T) {
			key, paired := f.User, `user_id = $2`
			var args []any
			if c.Table == "users" {
				key, paired = f.Bare, c.Material
			} else {
				args = []any{f.Other}
			}
			agSavepoint(t, f.Tx, func() {
				s := agUpdate(t, f.Tx, c, agSQL(`%s = %d`, c.Version, agWriterVersion), key)
				assert.Equal(t, f.Base[c.Name], s.Version, "a direct version write is discarded")
			})
			agSavepoint(t, f.Tx, func() {
				s := agUpdate(t, f.Tx, c, agSQL(`%s = %d, `, c.Version, agWriterVersion)+paired, key, args...)
				agMaterialVersion(t, c, s.Version, f.Base[c.Name])
				assert.NotEqual(t, agWriterVersion, s.Version, "the trigger moves the version itself, not to the writer's value")
			})
		})
	}
}

// Review Focus 3: the recovery-key upsert (mfa/handlers.go) rewrites identical
// bytes when a client re-stores the same key. That is not a re-enrolment.
func TestMigration000189_IdenticalRestoreIsNotMaterial(t *testing.T) {
	f := migration000189Seeded(t)

	const upsert = `
		INSERT INTO user_recovery_keys
		    (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET
			recovery_wrapped_private_key = EXCLUDED.recovery_wrapped_private_key, -- pragma: allowlist secret
			recovery_key_salt = EXCLUDED.recovery_key_salt,
			recovery_wrapped_prefs_key = EXCLUDED.recovery_wrapped_prefs_key,
			recovery_prefs_key_salt = EXCLUDED.recovery_prefs_key_salt,
			updated_at = NOW()
		RETURNING material_version, matures_at`

	run := func(prefsKey string) (version int64, matures time.Time) {
		t.Helper()
		require.NoError(t, f.Tx.QueryRow(upsert, f.User,
			[]byte(agRecoveryPrivateKey), []byte(agRecoverySalt), []byte(prefsKey), []byte(agRecoveryPrefsSalt),
		).Scan(&version, &matures))
		return version, matures
	}

	agSavepoint(t, f.Tx, func() {
		version, matures := run(agRecoveryPrefsKey)
		assert.Equal(t, f.Base["user_recovery_keys"], version, "identical bytes are not a material change")
		agRequireTime(t, agLegacyMaturity, matures, "identical restore must not stamp")
	})
	agSavepoint(t, f.Tx, func() {
		version, matures := run(agRecoveryPrefsKey + "!")
		agMaterialVersion(t, agCredFor(t, "user_recovery_keys"), version, f.Base["user_recovery_keys"])
		agRequireTime(t, agStamp(t, f.Tx), matures, "a changed restore stamps")
	})
}

// agReenrolSlots gives each credential two DIFFERENT material changes, because
// a second change that repeats the first is not material and would test nothing.
// Email and SSO draw a fresh random value each time, so one clause serves both.
var agReenrolSlots = []struct {
	name     string
	table    string
	first    string
	second   string
	useBare  bool // users rows are keyed on the bare user, factor rows on the owner
	credName string
}{
	{"users_password", "users", "password_hash = 'y'", "password_hash = 'z'", true, "users password"},
	{"users_email", "users", "", "", true, "users email"},
	{"user_mfa_totp", "user_mfa_totp", "totp_secret_enc = decode('aabbcc', 'hex')", "totp_secret_enc = decode('ddeeff', 'hex')", false, "user_mfa_totp"},
	{"user_mfa_webauthn", "user_mfa_webauthn", "public_key = decode('aabbcc', 'hex')", "public_key = decode('ddeeff', 'hex')", false, "user_mfa_webauthn"},
	{"user_sso_identities", "user_sso_identities", "", "", false, "user_sso_identities"},
	{"user_recovery_keys", "user_recovery_keys", "recovery_key_salt = decode('aabbcc', 'hex')", "recovery_key_salt = decode('ddeeff', 'hex')", false, "user_recovery_keys"},
}

func agReenrolCred(t *testing.T, name string) agCred {
	t.Helper()
	for _, c := range agCreds {
		if c.Name == name {
			return c
		}
	}
	require.Failf(t, "unknown credential", "%s", name)
	return agCred{}
}

// Architect ruling F2: a writer that sets enrolled_at has made its provenance
// statement, so the trigger must keep the enrol_class it wrote even when that
// class equals OLD. Re-enrolling at the SAME class (an MFA owner rotating the
// password at C2 twice) must not fall back to NULL, which Decide reads as the
// rank floor and so lets a lone mailbox contest the owner's fresh credential.
//
// The seeded class is 2 on every credential, so the first write below is already
// a same-class write. Both steps are asserted: the second one, after a kept
// class, is the case the ruling names.
func TestMigration000189_WriterProvenanceSurvivesSameClassReenrolment(t *testing.T) {
	f := migration000189Seeded(t)
	stamp := agStamp(t, f.Tx)
	enrolledFirst := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	enrolledSecond := time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, slot := range agReenrolSlots {
		t.Run(slot.name, func(t *testing.T) {
			c := agReenrolCred(t, slot.credName)
			key := f.User
			if slot.useBare {
				key = f.Bare
			}
			first, second := slot.first, slot.second
			if first == "" {
				first, second = c.Material, c.Material // random per evaluation
			}
			agSavepoint(t, f.Tx, func() {
				s1 := agUpdate(t, f.Tx, c, agSQL(`%s, %s = '2001-01-01Z', %s = 2`, first, c.Enrolled, c.Class), key)
				require.True(t, agMaterialVersion(t, c, s1.Version, f.Base[c.Name]), "the first change must be material")
				agRequireTime(t, enrolledFirst, s1.Enrolled, "first enrolled_at is the writer's")
				assert.Equal(t, sql.NullInt64{Int64: 2, Valid: true}, s1.Class,
					"first re-enrolment at the same class: the writer set enrolled_at, so enrol_class must survive")

				s2 := agUpdate(t, f.Tx, c, agSQL(`%s, %s = '2002-01-01Z', %s = 2`, second, c.Enrolled, c.Class), key)
				require.True(t, agMaterialVersion(t, c, s2.Version, f.Base[c.Name], s1.Version), "the second change must be material")
				agRequireTime(t, stamp, s2.Matures, "maturity is still re-stamped")
				agRequireTime(t, enrolledSecond, s2.Enrolled, "second enrolled_at is the writer's")
				assert.Equal(t, sql.NullInt64{Int64: 2, Valid: true}, s2.Class,
					"second re-enrolment at the same class: enrol_class must survive, not read as unknown")
			})
		})
	}
}

// Control for F2: the exemption is keyed on the writer setting enrolled_at. A
// material change that leaves enrolled_at alone is not a provenance statement,
// so provenance resets exactly as before even when the writer repeats the class.
func TestMigration000189_WriterWithoutEnrolledAtStillResetsProvenance(t *testing.T) {
	f := migration000189Seeded(t)
	now := agNow(t, f.Tx)

	for _, slot := range agReenrolSlots {
		t.Run(slot.name, func(t *testing.T) {
			c := agReenrolCred(t, slot.credName)
			key := f.User
			if slot.useBare {
				key = f.Bare
			}
			first := slot.first
			if first == "" {
				first = c.Material
			}
			agSavepoint(t, f.Tx, func() {
				s := agUpdate(t, f.Tx, c, agSQL(`%s, %s = 2`, first, c.Class), key)
				require.True(t, agMaterialVersion(t, c, s.Version, f.Base[c.Name]), "the change must be material")
				assert.False(t, s.Class.Valid, "no enrolled_at from the writer: enrol_class resets to unknown")
				agRequireTime(t, now, s.Enrolled, "enrolled_at is re-stamped by the trigger")
			})
		})
	}
}

// E4: the rewrap exemption holds only when NOTHING but the four rewrap columns
// changed. Pair the rewrap SET list with every other column of user_mfa_totp, one
// at a time, under the flag: each must be a material change. enabled, confirmed
// and user_id are material for their own reasons (user_id needs the second bare
// user bound); the rest prove that the whole-row comparison, not a column list in
// the predicate, is what bounds the exemption.
func TestMigration000189_RewrapExemptionCoversEveryOtherColumn(t *testing.T) {
	f := migration000189Seeded(t)
	totp := agCredFor(t, "user_mfa_totp")
	rewrapCols := map[string]bool{"totp_secret_enc": true, "totp_secret_nonce": true, "key_version": true, "updated_at": true}
	const rewrap = `totp_secret_enc = decode('aabbcc', 'hex'), totp_secret_nonce = decode('ddeeff', 'hex'), ` +
		`key_version = 2, updated_at = now() + interval '1 second'`

	var tried int
	for _, col := range agSortedKeys(agInventory["user_mfa_totp"]) {
		if rewrapCols[col] {
			continue
		}
		tried++
		t.Run(col, func(t *testing.T) {
			agSavepoint(t, f.Tx, func() {
				_, err := f.Tx.Exec(`SELECT set_config($1, $2, true)`, agRewrapGUC, agRewrapValue)
				require.NoError(t, err)
				set := agSQL(`%s, %s = %s`, rewrap, col, agInventory["user_mfa_totp"][col].Change)
				var args []any
				if strings.Contains(set, "$2") {
					args = []any{f.Other}
				}
				s := agUpdate(t, f.Tx, totp, set, f.User, args...)
				assert.True(t, agMaterialVersion(t, totp, s.Version, f.Base[totp.Name]),
					"a flagged rewrap that also changes %s must be material", col)
			})
		})
	}
	// The inventory is the loop's source, so an emptied inventory must not pass.
	require.GreaterOrEqual(t, tried, 10, "the loop must cover the non-rewrap columns")
}

// E3: the down's guard refuses an activated guard or a contested credential, and
// nothing else. A row still inside its 72-hour window is not a reason to refuse.
func TestMigration000189_DownSucceedsWithImmatureUncontestedRows(t *testing.T) {
	f := migration000189Seeded(t)
	down, up := migration000189DownSQL(t), migration000189UpSQL(t)

	for _, c := range agCreds {
		_, err := f.Tx.Exec(agSQL(`UPDATE %s SET %s = now() + interval '72 hours'`, c.Table, c.Matures))
		require.NoError(t, err, "make every %s row immature", c.Name)
		if c.Contested != "" {
			var contested int
			require.NoError(t, f.Tx.QueryRow(agSQL(`SELECT count(*) FROM %s WHERE %s IS NOT NULL`, c.Table, c.Contested)).Scan(&contested))
			require.Zero(t, contested, "%s must be uncontested", c.Name)
		}
	}

	_, err := f.Tx.Exec(down)
	require.NoError(t, err, "the down must not refuse immature, uncontested rows")
	var present bool
	require.NoError(t, f.Tx.QueryRow(`SELECT to_regclass(quote_ident(current_schema()) || '.accountguard_state') IS NOT NULL`).Scan(&present))
	assert.False(t, present, "the down must have run")

	_, err = f.Tx.Exec(up)
	require.NoError(t, err, "the up must run again after the down")
	require.NoError(t, f.Tx.QueryRow(`SELECT to_regclass(quote_ident(current_schema()) || '.accountguard_state') IS NOT NULL`).Scan(&present))
	assert.True(t, present)
}

// lockTimeoutWindow bounds how long a file that begins with SET LOCAL
// lock_timeout = '3s' may wait behind a held lock: the timer must fire (a lower
// bound proves the wait was the timer, not an instant refusal) and must be the
// only thing ending the wait (the upper bound sits well under the test's own 20s
// context, which is the net that keeps a missing timeout from hanging the run).
const (
	lockTimeoutFloor = 2 * time.Second
	lockTimeoutCeil  = 6 * time.Second
	lockTimeoutNet   = 20 * time.Second
)

// holdAccessShare opens a transaction holding ACCESS SHARE on users, the weakest
// lock that still conflicts with the ACCESS EXCLUSIVE both files take.
func holdAccessShare(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	blocker, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, blocker) })
	_, err = blocker.Exec(`LOCK TABLE users IN ACCESS SHARE MODE`)
	require.NoError(t, err)
	return blocker
}

func requireBoundedLockTimeout(t *testing.T, start time.Time, err error) {
	t.Helper()
	elapsed := time.Since(start)
	migrationRequirePQError(t, err, "55P03", "")
	assert.GreaterOrEqual(t, elapsed, lockTimeoutFloor, "the failure must be the 3s lock_timeout, not an instant refusal")
	assert.Less(t, elapsed, lockTimeoutCeil, "the lock_timeout must end the wait")
}

// E1: the up fails with lock_not_available inside its 3s lock_timeout when it
// cannot get ACCESS EXCLUSIVE on users, and rolls back whole. The down is
// committed here (as migration 000145's test does) so the up runs against the
// pre-000189 schema with nothing else of this test holding a lock; the cleanup
// restores the up, then re-applies 000191, which replaces a function 000189
// creates. Without it the shared database would keep 000189's body at version
// 191 for every later test.
func TestMigration000189_UpFailsWithinLockTimeoutBehindAHeldLock(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	up, down := migration000189UpSQL(t), migration000189DownSQL(t)

	_, err := db.Exec(down)
	require.NoError(t, err)
	t.Cleanup(func() {
		var present bool
		require.NoError(t, db.QueryRow(`SELECT to_regclass('accountguard_state') IS NOT NULL`).Scan(&present))
		if !present {
			_, restoreErr := db.Exec(up)
			require.NoError(t, restoreErr, "restore migration 000189 after test")
			_, restoreErr = db.Exec(migration000191UpSQL(t))
			require.NoError(t, restoreErr, "re-apply migration 000191 after restoring 000189")
		}
	})

	blocker := holdAccessShare(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeoutNet)
	defer cancel()
	start := time.Now()
	_, err = db.ExecContext(ctx, up)
	requireBoundedLockTimeout(t, start, err)
	require.NoError(t, blocker.Rollback())

	var present bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('accountguard_state') IS NOT NULL`).Scan(&present))
	assert.False(t, present, "a timed-out up must leave nothing behind")
}

// E2: the same for the down. The blocker holds ACCESS SHARE on users, the down's
// first LOCK TABLE target, so the down must fail within its own 3s lock_timeout,
// and a failed down changes nothing.
func TestMigration000189_DownFailsWithinLockTimeoutBehindAHeldLock(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	down := migration000189DownSQL(t)

	blocker := holdAccessShare(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeoutNet)
	defer cancel()
	start := time.Now()
	_, err := db.ExecContext(ctx, down)
	requireBoundedLockTimeout(t, start, err)
	require.NoError(t, blocker.Rollback())

	var present bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('accountguard_state') IS NOT NULL`).Scan(&present))
	assert.True(t, present, "a timed-out down must change nothing")
}

// E7: the sequence is the only source of every version, so its shape is part of
// the contract: NO CYCLE (a cycling sequence hands a value out twice), START WITH
// 2 (every draw stays apart from the legacy 1), and bigint (a 32-bit sequence
// would wrap into an error long before a version became ambiguous).
func TestMigration000189_VersionSequenceShape(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	var dataType string
	var start int64
	var cycle bool
	require.NoError(t, tx.QueryRow(`
		SELECT data_type::text, start_value, cycle FROM pg_sequences
		 WHERE schemaname = current_schema() AND sequencename = 'accountguard_material_version_seq'`).Scan(&dataType, &start, &cycle))
	assert.Equal(t, "bigint", dataType)
	assert.Equal(t, int64(2), start)
	assert.False(t, cycle, "a cycling sequence would hand a version out twice")
}
