package mfa_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reuses rotationKeyV1/rotationKeyV2 from handlers_test.go (same package).

// seedSealedRow inserts an enabled+confirmed enrollment sealed under the given
// ring's active key and returns the user ID.
func seedSealedRow(t *testing.T, ts *testhelpers.TestServer, ring *mfa.Keyring, name, seed string) string {
	t.Helper()
	user := ts.CreateTestUser(t, name)
	ct, nonce, ver, err := ring.Seal([]byte(seed))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, TRUE, TRUE)`,
		user.ID, ct, nonce, ver)
	require.NoError(t, err)
	return user.ID
}

func rotatedRing(t *testing.T) *mfa.Keyring {
	t.Helper()
	ring, err := mfa.ParseKeyring(rotationKeyV2, 2, "1:"+rotationKeyV1)
	require.NoError(t, err)
	return ring
}

func v1OnlyRing(t *testing.T) *mfa.Keyring {
	t.Helper()
	ring, err := mfa.ParseKeyring(rotationKeyV1, 1, "")
	require.NoError(t, err)
	return ring
}

func TestRekey_ConvergesAllRowsToActive(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	u1 := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyuser1", "JBSWY3DPEHPK3PXP")
	u2 := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyuser2", "KRSXG5CTMVRXEZLU")

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 1 /* batchSize=1 exercises pagination */, false)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Rekeyed)
	assert.Empty(t, res.Failed)

	for _, uid := range []string{u1, u2} {
		var enc, nonce []byte
		var ver int
		require.NoError(t, ts.DB.QueryRow(
			`SELECT totp_secret_enc, totp_secret_nonce, key_version FROM user_mfa_totp WHERE user_id = $1`, uid,
		).Scan(&enc, &nonce, &ver))
		assert.Equal(t, 2, ver)
		_, err := ring.Open(enc, nonce, ver)
		assert.NoError(t, err, "re-sealed row must decrypt under the active key")
	}
}

func TestRekey_IdempotentSecondRun(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	seedSealedRow(t, ts, v1OnlyRing(t), "rekeyidem", "JBSWY3DPEHPK3PXP")

	_, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.NoError(t, err)
	res2, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.NoError(t, err)
	assert.Equal(t, 0, res2.Rekeyed)
	assert.Empty(t, res2.Failed)
}

func TestRekey_DryRunCountsWithoutMutating(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeydry", "JBSWY3DPEHPK3PXP")

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, true)
	require.NoError(t, err)
	// The shared test DB may carry pending rows from sibling tests; this
	// user's row must be among the pending count and stay unmutated.
	assert.GreaterOrEqual(t, res.Scanned, 1)

	var ver int
	require.NoError(t, ts.DB.QueryRow(`SELECT key_version FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&ver))
	assert.Equal(t, 1, ver, "dry-run must not mutate")
}

func TestRekey_UndecryptableRowLeftUntouchedAndReported(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	// Sealed under a key that is in NO ring (version 3 stamp, unknown key).
	orphanRing, err := mfa.ParseKeyring("0303030303030303030303030303030303030303030303030303030303030303", 3, "")
	require.NoError(t, err)
	uid := seedSealedRow(t, ts, orphanRing, "rekeyorphan", "JBSWY3DPEHPK3PXP")
	seedSealedRow(t, ts, v1OnlyRing(t), "rekeygood", "KRSXG5CTMVRXEZLU")

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Rekeyed, 1, "the decryptable row still converges")
	require.NotEmpty(t, res.Failed)
	var found *mfa.RekeyFailure
	for i := range res.Failed {
		if res.Failed[i].UserID == uid {
			found = &res.Failed[i]
		}
	}
	require.NotNil(t, found, "orphan row must be reported")
	assert.Equal(t, 3, found.SealedVersion)

	var ver int
	require.NoError(t, ts.DB.QueryRow(`SELECT key_version FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&ver))
	assert.Equal(t, 3, ver, "failed row must be left untouched")
}

// A row whose stored nonce is the wrong length used to panic inside gcm.Open
// and end the whole backfill. It must be reported, left byte-for-byte
// untouched, and the rows after it must still converge.
func TestRekey_WrongLengthNonceRowReportedAndRunContinues(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	u1 := seedSealedRow(t, ts, v1OnlyRing(t), "rekeynonce1", "JBSWY3DPEHPK3PXP")
	u2 := seedSealedRow(t, ts, v1OnlyRing(t), "rekeynonce2", "KRSXG5CTMVRXEZLU")

	// Rekey walks rows in user_id order, so damaging the lower ID puts a good
	// row after it: the run has to get past the damaged row to converge it.
	bad, good := u1, u2
	if good < bad {
		bad, good = good, bad
	}
	_, err := ts.DB.Exec(`UPDATE user_mfa_totp SET totp_secret_nonce = substring(totp_secret_nonce from 1 for 11) WHERE user_id = $1`, bad)
	require.NoError(t, err)
	var beforeEnc, beforeNonce []byte
	require.NoError(t, ts.DB.QueryRow(`SELECT totp_secret_enc, totp_secret_nonce FROM user_mfa_totp WHERE user_id = $1`, bad).Scan(&beforeEnc, &beforeNonce))

	var res mfa.RekeyResult
	require.NotPanics(t, func() {
		res, err = mfa.Rekey(context.Background(), ts.DB, ring, 1, false)
	})
	require.NoError(t, err)

	var found *mfa.RekeyFailure
	for i := range res.Failed {
		if res.Failed[i].UserID == bad {
			found = &res.Failed[i]
		}
	}
	require.NotNil(t, found, "damaged row must be reported")
	assert.Equal(t, 1, found.SealedVersion)
	assert.ErrorContains(t, found.Err, "invalid nonce length")

	var enc, nonce []byte
	var ver int
	require.NoError(t, ts.DB.QueryRow(`SELECT totp_secret_enc, totp_secret_nonce, key_version FROM user_mfa_totp WHERE user_id = $1`, bad).Scan(&enc, &nonce, &ver))
	assert.Equal(t, beforeEnc, enc, "damaged row's ciphertext changed")
	assert.Equal(t, beforeNonce, nonce, "damaged row's nonce changed")
	assert.Equal(t, 1, ver, "damaged row's key_version changed")

	require.NoError(t, ts.DB.QueryRow(`SELECT key_version FROM user_mfa_totp WHERE user_id = $1`, good).Scan(&ver))
	assert.Equal(t, 2, ver, "the row after the damaged one did not converge")
}

// TestRekey_CASGuardSkipsConcurrentlyRewrittenRow exercises the UPDATE's
// compare-and-swap tail directly: if the stored ciphertext no longer matches
// what the backfill read, the write must affect 0 rows.
func TestRekey_CASGuardSkipsConcurrentlyRewrittenRow(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeycas", "JBSWY3DPEHPK3PXP")

	var origEnc []byte
	require.NoError(t, ts.DB.QueryRow(`SELECT totp_secret_enc FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&origEnc))

	// Simulate a concurrent re-enrollment: the server rewrote the row under the
	// active version between the backfill's SELECT and UPDATE.
	newCT, newNonce, newVer, err := ring.Seal([]byte("NEWSEEDNEWSEEDNE"))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE user_mfa_totp SET totp_secret_enc=$1, totp_secret_nonce=$2, key_version=$3 WHERE user_id=$4`,
		newCT, newNonce, newVer, uid)
	require.NoError(t, err)

	// Replay the backfill's CAS UPDATE with the STALE prior image.
	staleCT, staleNonce, _, err := v1OnlyRing(t).Seal([]byte("JBSWY3DPEHPK3PXP"))
	require.NoError(t, err)
	tag, err := ts.DB.Exec(`
		UPDATE user_mfa_totp
		   SET totp_secret_enc = $1, totp_secret_nonce = $2, key_version = $3, updated_at = NOW()
		 WHERE user_id = $4 AND key_version = $5 AND totp_secret_enc = $6`,
		staleCT, staleNonce, ring.ActiveVersion(), uid, 1, origEnc)
	require.NoError(t, err)
	n, err := tag.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "CAS tail must skip a concurrently rewritten row")

	var enc []byte
	require.NoError(t, ts.DB.QueryRow(`SELECT totp_secret_enc FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&enc))
	assert.Equal(t, newCT, enc, "concurrent rewrite must win")
}

// --- #3680 PR1: Rekey under migration 000189's material trigger ---

// rekeyLegacyProvenance marks the seeded row as a legacy credential with known
// provenance, so the "unchanged" assertions are not vacuous.
func rekeyLegacyProvenance(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`
		UPDATE user_mfa_totp
		   SET matures_at = '1970-01-01Z', enrolled_at = '2000-01-01Z', enrol_class = 2
		 WHERE user_id = $1`, userID)
	require.NoError(t, err)
}

// A rewrap is not a re-enrolment: it must not bump material_version, re-stamp
// maturity or reset provenance.
func TestRekey_PreservesMaturityAndMaterialVersion(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	const seed = "JBSWY3DPEHPK3PXP"
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeymature", seed)
	rekeyLegacyProvenance(t, ts.DB, uid)
	var versionBefore int64
	require.NoError(t, ts.DB.QueryRow(`SELECT material_version FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&versionBefore))

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Rekeyed)

	var (
		enc, nonce        []byte
		keyVersion        int
		materialVersion   int64
		matures, enrolled time.Time
		enrolClass        sql.NullInt64
	)
	require.NoError(t, ts.DB.QueryRow(`
		SELECT totp_secret_enc, totp_secret_nonce, key_version, material_version, matures_at, enrolled_at, enrol_class
		  FROM user_mfa_totp WHERE user_id = $1`, uid,
	).Scan(&enc, &nonce, &keyVersion, &materialVersion, &matures, &enrolled, &enrolClass))

	assert.Equal(t, 2, keyVersion)
	assert.Equal(t, versionBefore, materialVersion, "a rewrap must not move material_version")
	legacy := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC) // PR3 rebinds this to decide's legacy sentinel.
	assert.True(t, legacy.Equal(matures), "a rewrap must not re-stamp maturity, got %s", matures)
	assert.True(t, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Equal(enrolled), "enrolled_at changed: %s", enrolled)
	assert.Equal(t, sql.NullInt64{Int64: 2, Valid: true}, enrolClass, "enrol_class changed")

	plain, err := ring.Open(enc, nonce, keyVersion)
	require.NoError(t, err)
	assert.Equal(t, seed, string(plain), "the row must open under the rotated ring to the original seed")
}

// Review Focus 2: the flag is transaction-local. On a single-connection pool
// the next statement runs on the same session, so a flag that outlived its row
// would exempt a real re-enrolment.
func TestRekey_RewrapFlagDoesNotLeakPastRow(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyleak", "JBSWY3DPEHPK3PXP")
	rekeyLegacyProvenance(t, ts.DB, uid)
	var versionBefore int64
	require.NoError(t, ts.DB.QueryRow(`SELECT material_version FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&versionBefore))

	pool, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { assert.NoError(t, pool.Close()) })

	res, err := mfa.Rekey(context.Background(), pool, ring, 100, false)
	require.NoError(t, err)
	require.Equal(t, 1, res.Rekeyed)

	var unset bool
	require.NoError(t, pool.QueryRow(`SELECT NULLIF(current_setting('concord.credential_rewrap', true), '') IS NULL`).Scan(&unset))
	assert.True(t, unset, "the rewrap flag must not survive the row's transaction")

	var version int64
	require.NoError(t, pool.QueryRow(`
		UPDATE user_mfa_totp SET totp_secret_enc = $2 WHERE user_id = $1 RETURNING material_version`,
		uid, []byte("a-real-reenrolment")).Scan(&version))
	assert.NotEqual(t, versionBefore, version, "an ordinary secret change on the same session must be material")
	assert.GreaterOrEqual(t, version, int64(2), "the new version is drawn from the sequence")
}

// dropRekeyTestTrigger removes a test-only trigger and its function. DROP ... IF
// EXISTS makes it safe both before the CREATE (a previous run that died before
// its cleanup) and in the cleanup itself; a failure is a failure.
func dropRekeyTestTrigger(t *testing.T, db *sql.DB, trigger, function string) {
	t.Helper()
	_, err := db.Exec(`DROP TRIGGER IF EXISTS ` + trigger + ` ON user_mfa_totp`)
	assert.NoError(t, err, "drop trigger %s", trigger)
	_, err = db.Exec(`DROP FUNCTION IF EXISTS ` + function)
	assert.NoError(t, err, "drop function %s", function)
}

// Constant DDL for a deferred constraint trigger that raises at COMMIT after an
// UPDATE, so every statement succeeds and tx.Commit() itself returns the error.
// The unique names keep it clear of package mfa's i5_fail_commit, which fires on
// INSERT OR DELETE.
const (
	rekeyFailCommitFunc = `CREATE OR REPLACE FUNCTION rekey_fail_commit_update() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rekey forced commit failure'; END $$`
	rekeyFailCommitTrigger = `CREATE CONSTRAINT TRIGGER rekey_fail_commit_update AFTER UPDATE ON user_mfa_totp
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION rekey_fail_commit_update()`
)

func TestRekey_CommitFailureAbortsRun(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	// Seed first: the trigger would otherwise fire on the fixture's own writes.
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeycommit", "JBSWY3DPEHPK3PXP")

	dropRekeyTestTrigger(t, ts.DB, "rekey_fail_commit_update", "rekey_fail_commit_update()")
	t.Cleanup(func() { dropRekeyTestTrigger(t, ts.DB, "rekey_fail_commit_update", "rekey_fail_commit_update()") })
	_, err := ts.DB.Exec(rekeyFailCommitFunc)
	require.NoError(t, err)
	_, err = ts.DB.Exec(rekeyFailCommitTrigger)
	require.NoError(t, err)

	_, err = mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.Error(t, err)
	assert.ErrorContains(t, err, "commit row transaction")

	var version int
	require.NoError(t, ts.DB.QueryRow(`SELECT key_version FROM user_mfa_totp WHERE user_id = $1`, uid).Scan(&version))
	// This trigger raises while the commit is being processed, before the
	// transaction is durable, so the row keeps its old key version. A commit
	// error alone would not prove that: an error from Commit can follow a commit
	// that did apply, which is why the run aborts instead of counting the row.
	assert.Equal(t, 1, version, "the deferred trigger rejected the commit, so the row must stay on its old key version")
}

// Constant DDL for a NON-deferred BEFORE UPDATE row trigger that raises inside
// the CAS UPDATE itself, so the statement fails and tx.Commit() is never reached.
// Its names and message differ from the commit-failure trigger above, which
// makes the two failure arms distinguishable by error text.
const (
	rekeyFailUpdateFunc = `CREATE OR REPLACE FUNCTION rekey_fail_update_stmt() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rekey forced update failure'; END $$`
	rekeyFailUpdateTrigger = `CREATE TRIGGER rekey_fail_update_stmt BEFORE UPDATE ON user_mfa_totp
		FOR EACH ROW EXECUTE FUNCTION rekey_fail_update_stmt()`
)

func TestRekey_UpdateFailureAbortsRun(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	// Seed first: the trigger would otherwise fire on the fixture's own writes.
	uid := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyupdate", "JBSWY3DPEHPK3PXP")

	var beforeEnc []byte
	var beforeVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT totp_secret_enc, key_version FROM user_mfa_totp WHERE user_id = $1`, uid,
	).Scan(&beforeEnc, &beforeVersion))
	require.Equal(t, 1, beforeVersion)

	dropRekeyTestTrigger(t, ts.DB, "rekey_fail_update_stmt", "rekey_fail_update_stmt()")
	t.Cleanup(func() { dropRekeyTestTrigger(t, ts.DB, "rekey_fail_update_stmt", "rekey_fail_update_stmt()") })
	_, err := ts.DB.Exec(rekeyFailUpdateFunc)
	require.NoError(t, err)
	_, err = ts.DB.Exec(rekeyFailUpdateTrigger)
	require.NoError(t, err)

	_, err = mfa.Rekey(context.Background(), ts.DB, ring, 100, false)
	require.Error(t, err)
	assert.ErrorContains(t, err, "update row")
	assert.NotContains(t, err.Error(), "commit row transaction", "the UPDATE must fail before any commit is attempted")

	var enc []byte
	var version int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT totp_secret_enc, key_version FROM user_mfa_totp WHERE user_id = $1`, uid,
	).Scan(&enc, &version))
	assert.Equal(t, beforeVersion, version, "the failed UPDATE must leave the row on its old key version")
	assert.Equal(t, beforeEnc, enc, "the failed UPDATE must leave the ciphertext unchanged")
}

// Constant DDL for a BEFORE UPDATE trigger that moves material_version after the
// accountguard_material trigger has run (triggers fire in name order, and "zz_"
// sorts last). It stands in for the failure the Go-side check exists for: the
// material trigger treating a rewrap as material and drawing a new version.
const (
	rekeyForceVersionFunc = `CREATE OR REPLACE FUNCTION rekey_force_version_change() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN NEW.material_version := NEW.material_version + 1; RETURN NEW; END $$`
	rekeyForceVersionTrigger = `CREATE TRIGGER zz_rekey_force_version_change BEFORE UPDATE ON user_mfa_totp
		FOR EACH ROW EXECUTE FUNCTION rekey_force_version_change()`
)

// A rewrap whose row comes back with a different material_version means the
// trigger did not apply the rewrap exemption. The run must abort, roll that row
// back, name it, and still return the partial result: the undecryptable row met
// before the abort stays in Failed, and the aborted row is not counted as re-sealed.
func TestRekey_ExemptionNotAppliedAbortsWithPartialResult(t *testing.T) {
	ts := setupTS(t)
	ring := rotatedRing(t)
	a := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyabort1", "JBSWY3DPEHPK3PXP")
	b := seedSealedRow(t, ts, v1OnlyRing(t), "rekeyabort2", "KRSXG5CTMVRXEZLU")
	// Rows are visited in user_id order: the lower one must be the undecryptable
	// one (key_version 3 has no key in the ring) so it is met before the abort.
	failedID, abortID := a, b
	if b < a {
		failedID, abortID = b, a
	}
	_, err := ts.DB.Exec(`UPDATE user_mfa_totp SET key_version = 3 WHERE user_id = $1`, failedID)
	require.NoError(t, err)
	var abortVersionBefore int64
	require.NoError(t, ts.DB.QueryRow(`SELECT material_version FROM user_mfa_totp WHERE user_id = $1`, abortID).Scan(&abortVersionBefore))

	dropRekeyTestTrigger(t, ts.DB, "zz_rekey_force_version_change", "rekey_force_version_change()")
	t.Cleanup(func() {
		dropRekeyTestTrigger(t, ts.DB, "zz_rekey_force_version_change", "rekey_force_version_change()")
	})
	_, err = ts.DB.Exec(rekeyForceVersionFunc)
	require.NoError(t, err)
	_, err = ts.DB.Exec(rekeyForceVersionTrigger)
	require.NoError(t, err)

	res, err := mfa.Rekey(context.Background(), ts.DB, ring, 100, false)

	require.Error(t, err)
	assert.ErrorContains(t, err, "rewrap exemption not applied")
	assert.ErrorContains(t, err, "reseal user "+abortID+" (key_version 1)", "the abort must name the row")
	assert.Zero(t, res.Rekeyed, "an aborted row is not a re-sealed row")
	require.Len(t, res.Failed, 1, "the partial result keeps the failure met before the abort")
	assert.Equal(t, failedID, res.Failed[0].UserID)

	var keyVersion int
	var materialVersion int64
	require.NoError(t, ts.DB.QueryRow(`SELECT key_version, material_version FROM user_mfa_totp WHERE user_id = $1`, abortID).Scan(&keyVersion, &materialVersion))
	assert.Equal(t, 1, keyVersion, "the aborted row must be rolled back, not committed")
	assert.Equal(t, abortVersionBefore, materialVersion)
}
