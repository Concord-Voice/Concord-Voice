package database_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000189, red-team F1: a factor-table material_version is never reused
// for the same slot. user_mfa_totp and user_recovery_keys are keyed by user_id,
// so remove-then-re-enrol reuses the row identity; a per-row counter would hand
// the new credential a (kind, id, version) its predecessor held. Every factor
// version is therefore drawn from accountguard_material_version_seq (START 2) on
// EVERY insert and every material update. Versions are equality-only.

// agidFactorTables are the factor tables, in agFactorTables order.
var agidFactorTables = agFactorTables

// agidInsert inserts one factor row for userID with a WRITER-SUPPLIED
// material_version and returns the version the row ends up with.
func agidInsert(t *testing.T, q agRowQuerier, table string, userID uuid.UUID, writerVersion int64) int64 {
	t.Helper()
	var got int64
	var row *sql.Row
	switch table {
	case "user_mfa_totp":
		row = q.QueryRow(`
			INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, material_version)
			VALUES ($1, $2, $3, $4) RETURNING material_version`,
			userID, []byte(agTOTPEnc), []byte(agTOTPNonce), writerVersion)
	case "user_mfa_webauthn":
		row = q.QueryRow(`
			INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key, material_version)
			VALUES ($1, $2, $3, $4) RETURNING material_version`,
			userID, []byte("cred-"+uuid.NewString()), []byte("pk-seed"), writerVersion)
	case "user_sso_identities":
		row = q.QueryRow(`
			INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email, material_version)
			VALUES ($1, 'google', $2, 'sso@example.test', $3) RETURNING material_version`,
			userID, "sub-"+uuid.NewString(), writerVersion)
	case "user_recovery_keys":
		row = q.QueryRow(`
			INSERT INTO user_recovery_keys
			    (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt, material_version)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING material_version`,
			userID, []byte(agRecoveryPrivateKey), []byte(agRecoverySalt), []byte(agRecoveryPrefsKey), []byte(agRecoveryPrefsSalt), writerVersion)
	default:
		require.Failf(t, "unknown factor table", "%s", table)
	}
	require.NoError(t, row.Scan(&got), "insert into %s", table)
	return got
}

// agidSlot returns the two DIFFERENT material changes of a factor table.
func agidSlot(t *testing.T, table string) (first, second string) {
	t.Helper()
	for _, s := range agReenrolSlots {
		if s.table == table {
			return s.first, s.second
		}
	}
	require.Failf(t, "no re-enrolment slot", "%s", table)
	return "", ""
}

// The keyed-by-user_id factors: insert, material update, delete, then insert
// again, with the predecessor's own version as the writer's value on the
// re-insert (the value a per-row counter would have kept). No version may repeat
// anywhere in the slot's history.
func TestMigration000189_RemoveThenReenrolNeverRepeatsAVersion(t *testing.T) {
	f := migration000189Seeded(t)

	for _, table := range []string{"user_mfa_totp", "user_recovery_keys"} {
		t.Run(table, func(t *testing.T) {
			c := agCredFor(t, table)
			first, second := agidSlot(t, table)
			history := []int64{f.Base[c.Name]}
			record := func(what string, version int64) {
				t.Helper()
				assert.GreaterOrEqual(t, version, int64(2), "%s: drawn from the sequence", what)
				assert.NotContains(t, history, version, "%s: a version the slot already held", what)
				history = append(history, version)
			}

			agSavepoint(t, f.Tx, func() {
				record("first change", agUpdate(t, f.Tx, c, first, f.User).Version)
				record("second change", agUpdate(t, f.Tx, c, second, f.User).Version)

				_, err := f.Tx.Exec(agSQL(`DELETE FROM %s WHERE %s = $1`, c.Table, c.Key), f.User)
				require.NoError(t, err)

				record("re-enrolment insert", agidInsert(t, f.Tx, table, f.User, history[len(history)-1]))
				record("re-enrolled first change", agUpdate(t, f.Tx, c, first, f.User).Version)
				record("re-enrolled second change", agUpdate(t, f.Tx, c, second, f.User).Version)
				assert.Len(t, history, 6)
			})
		})
	}
}

// A writer cannot choose a factor row's version on INSERT: not the legacy 1, not
// the predecessor's value, not a value far above any draw.
func TestMigration000189_WriterSuppliedInsertVersionIsDiscarded(t *testing.T) {
	f := migration000189Seeded(t)

	for _, table := range agidFactorTables {
		t.Run(table, func(t *testing.T) {
			c := agCredFor(t, table)
			predecessor := f.Base[c.Name]
			for _, supplied := range []int64{1, predecessor, agWriterVersion} {
				agSavepoint(t, f.Tx, func() {
					got := agidInsert(t, f.Tx, table, f.Other, supplied)
					assert.GreaterOrEqual(t, got, int64(2), "supplied %d: version must come from the sequence", supplied)
					assert.NotEqual(t, supplied, got, "supplied %d: the writer's value must be discarded", supplied)
				})
			}
		})
	}
}

// The upsert's conflict branch fires BEFORE INSERT first (which spends a draw)
// and then BEFORE UPDATE. A material conflict update ends on a fresh version; a
// non-material one keeps the row's own, whatever the INSERT proposed.
func TestMigration000189_UpsertConflictBranchGetsAFreshVersion(t *testing.T) {
	f := migration000189Seeded(t)
	c := agCredFor(t, "user_mfa_totp")
	base := f.Base[c.Name]

	const upsert = `
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, material_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET %s
		RETURNING material_version`

	run := func(set string, enc string, supplied int64) int64 {
		t.Helper()
		var version int64
		require.NoError(t, f.Tx.QueryRow(agSQL(upsert, set), f.User, []byte(enc), []byte(agTOTPNonce), supplied).Scan(&version))
		return version
	}

	t.Run("material conflict update", func(t *testing.T) {
		agSavepoint(t, f.Tx, func() {
			got := run(`totp_secret_enc = EXCLUDED.totp_secret_enc, material_version = EXCLUDED.material_version`, "new-enc", base)
			assert.GreaterOrEqual(t, got, int64(2))
			assert.NotEqual(t, base, got, "the conflict branch must not keep, or take from the writer, the old version")
			var last int64
			require.NoError(t, f.Tx.QueryRow(`SELECT last_value FROM accountguard_material_version_seq`).Scan(&last))
			assert.Equal(t, last, got, "the final version is the sequence's latest draw")
		})
	})
	t.Run("non-material conflict update", func(t *testing.T) {
		agSavepoint(t, f.Tx, func() {
			got := run(`updated_at = NOW(), material_version = EXCLUDED.material_version`, agTOTPEnc, agWriterVersion)
			assert.Equal(t, base, got, "a non-material conflict update keeps the row's version")
		})
	})
}

// Rows that exist before the migration read the legacy version 1 (the constant
// fast default), and every value the sequence hands out afterwards is at least 2,
// so no new credential can collide with a legacy one.
func TestMigration000189_LegacyRowsReadOneAndNewVersionsStartAtTwo(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	up, down := migration000189UpSQL(t), migration000189DownSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })
	_, err = tx.Exec(down)
	require.NoError(t, err)

	legacy := migration000189SeedUser(t, tx)
	migration000189SeedFactors(t, tx, legacy)
	_, err = tx.Exec(up)
	require.NoError(t, err)

	for _, table := range agidFactorTables {
		c := agCredFor(t, table)
		assert.Equal(t, int64(1), agRead(t, tx, c, legacy).Version, "%s: a legacy row reads version 1", table)
	}

	// A material change to each legacy row draws from the sequence.
	for _, table := range agidFactorTables {
		c := agCredFor(t, table)
		s := agUpdate(t, tx, c, c.Material, legacy)
		assert.GreaterOrEqual(t, s.Version, int64(2), "%s: a material change leaves legacy 1", table)
	}

	// Every new row, on every factor table, starts at 2 or more.
	fresh := migration000189SeedUser(t, tx)
	migration000189SeedFactors(t, tx, fresh)
	for _, table := range agidFactorTables {
		c := agCredFor(t, table)
		assert.GreaterOrEqual(t, agRead(t, tx, c, fresh).Version, int64(2), "%s: a new row never takes the legacy 1", table)
	}
}

// The trigger names the sequence through TG_TABLE_SCHEMA. An unqualified name
// would be searched in pg_temp first, so a session's own TEMP SEQUENCE of the
// same name would choose the version.
func TestMigration000189_SessionTempSequenceDoesNotSupplyTheVersion(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	const shadowStart = 5000000
	_, err := tx.Exec(agSQL(`CREATE TEMP SEQUENCE accountguard_material_version_seq START WITH %d`, shadowStart))
	require.NoError(t, err)

	user := migration000189SeedUser(t, tx)
	migration000189SeedFactors(t, tx, user)
	for _, table := range agidFactorTables {
		c := agCredFor(t, table)
		assert.Less(t, agRead(t, tx, c, user).Version, int64(shadowStart), "%s: INSERT drew from the temp sequence", table)
		s := agUpdate(t, tx, c, c.Material, user)
		assert.Less(t, s.Version, int64(shadowStart), "%s: a material update drew from the temp sequence", table)
		assert.GreaterOrEqual(t, s.Version, int64(2), table)
	}

	var called bool
	require.NoError(t, tx.QueryRow(`SELECT is_called FROM pg_temp.accountguard_material_version_seq`).Scan(&called))
	assert.False(t, called, "the temp sequence must be untouched")

	// Positive control: an unqualified name in this session DOES resolve to the
	// temp sequence, so the shadow above was effective and the checks could fail.
	var viaSearchPath int64
	require.NoError(t, tx.QueryRow(`SELECT nextval('accountguard_material_version_seq')`).Scan(&viaSearchPath))
	assert.Equal(t, int64(shadowStart), viaSearchPath)
}

// Every user INSERT draws both slots from the sequence and discards whatever the
// writer supplied, as the factor tables do: a kept value could equal a later draw
// for the same row.
func TestMigration000189_UsersInsertDiscardsWriterSuppliedVersions(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	for _, supplied := range []int64{1, 2, agWriterVersion} {
		agSavepoint(t, tx, func() {
			id := uuid.New()
			var pw, em int64
			require.NoError(t, tx.QueryRow(`
				INSERT INTO users (id, email, username, password_hash, password_version, email_version)
				VALUES ($1, $2, $3, 'x', $4, $4) RETURNING password_version, email_version`,
				id, "ag-"+id.String()+"@example.test", "ag"+strings.ReplaceAll(id.String(), "-", ""), supplied).Scan(&pw, &em))
			assert.GreaterOrEqual(t, pw, int64(2), "supplied %d: password_version must come from the sequence", supplied)
			assert.GreaterOrEqual(t, em, int64(2), "supplied %d: email_version must come from the sequence", supplied)
			if supplied != 1 && supplied != 2 {
				assert.NotEqual(t, supplied, pw, "supplied %d: the writer's password_version must be discarded", supplied)
				assert.NotEqual(t, supplied, em, "supplied %d: the writer's email_version must be discarded", supplied)
			}
			assert.NotEqual(t, pw, em, "each slot draws its own value")
		})
	}
}

// Red-team RT-4: a users version was OLD + 1, so a rolled-back material write
// handed its version to the next, different, material. Each change now draws
// from the sequence, whose spent values a rollback does not return.
func TestMigration000189_UsersVersionNotReusedAfterRolledBackWrite(t *testing.T) {
	f := migration000189Seeded(t)

	for _, c := range agCreds {
		if c.Table != "users" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			var rolledBack, committed agState
			agSavepoint(t, f.Tx, func() {
				rolledBack = agUpdate(t, f.Tx, c, c.Material, f.Bare)
			})
			committed = agUpdate(t, f.Tx, c, c.Material, f.Bare)

			assert.True(t, agMaterialVersion(t, c, rolledBack.Version, f.Base[c.Name]))
			assert.True(t, agMaterialVersion(t, c, committed.Version, f.Base[c.Name], rolledBack.Version),
				"a different credential must not carry the version a rolled-back write spent")
		})
	}
}

// Red-team RT-3: an ordinary trigger does not fire under session_replication_role
// = replica, which a superuser (the application role in compose) can set. The
// material triggers are ENABLE ALWAYS, so a credential change still moves its
// version and re-stamps maturity there. Setting the role needs a superuser; the
// test says so and skips on a role that is not one.
func TestMigration000189_MaterialTriggersFireUnderReplicaRole(t *testing.T) {
	f := migration000189Seeded(t)
	var super bool
	require.NoError(t, f.Tx.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super))
	if !super {
		t.Skip("SET session_replication_role needs a superuser, and this database role is not one; " +
			"the RT-3 property is exercised only where the application role is a superuser")
	}
	stamp := agStamp(t, f.Tx)

	for _, c := range agCreds {
		t.Run(c.Name, func(t *testing.T) {
			key := f.User
			if c.Table == "users" {
				key = f.Bare
			}
			agSavepoint(t, f.Tx, func() {
				_, err := f.Tx.Exec(`SET LOCAL session_replication_role = replica`)
				require.NoError(t, err)
				s := agUpdate(t, f.Tx, c, c.Material, key)
				assert.True(t, agMaterialVersion(t, c, s.Version, f.Base[c.Name]),
					"a replica-role material change must still move the version")
				agRequireTime(t, stamp, s.Matures, "a replica-role material change must still stamp maturity")
			})
		})
	}
}

// All five accountguard_material triggers are ENABLE ALWAYS ('A'), not the
// ordinary origin-only 'O'.
func TestMigration000189_MaterialTriggersAreEnableAlways(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000189Fresh(t, db)

	rows, err := tx.Query(`
		SELECT c.relname, t.tgenabled::text
		  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE t.tgname = 'accountguard_material' AND NOT t.tgisinternal
		   AND c.relnamespace = to_regnamespace(current_schema())
		 ORDER BY 1`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	got := map[string]string{}
	for rows.Next() {
		var table, enabled string
		require.NoError(t, rows.Scan(&table, &enabled))
		got[table] = enabled
	}
	require.NoError(t, rows.Err())

	want := map[string]string{}
	for _, table := range agSnapshotTables {
		want[table] = "A"
	}
	assert.Equal(t, want, got, "every accountguard_material trigger must be ENABLE ALWAYS")
}
