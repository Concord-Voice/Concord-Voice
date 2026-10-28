package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runMain calls config.Load, and an unset ENVIRONMENT now means production,
// whose guards refuse the dev secrets these tests run with.
func TestMain(m *testing.M) {
	if os.Getenv("ENVIRONMENT") == "" {
		_ = os.Setenv("ENVIRONMENT", "development")
	}
	os.Exit(m.Run())
}

func TestParseFlags_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts, err := parseFlags(fs, nil)
	require.NoError(t, err)
	assert.False(t, opts.DryRun)
	assert.Equal(t, 100, opts.BatchSize)
}

func TestParseFlags_Explicit(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts, err := parseFlags(fs, []string{"--dry-run", "--batch-size", "7"})
	require.NoError(t, err)
	assert.True(t, opts.DryRun)
	assert.Equal(t, 7, opts.BatchSize)
}

// TestParseFlags_RejectsBadInput covers the correctness guard: an unparseable
// flag value, an unknown flag, or a stray positional argument must return an
// error so runMain refuses to connect and mutate production data.
func TestParseFlags_RejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"non-integer batch size": {"--batch-size", "nope"},
		"unknown flag":           {"--wat"},
		"positional argument":    {"extra-arg"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			_, err := parseFlags(fs, args)
			require.Error(t, err)
		})
	}
}

func TestReport_DryRun(t *testing.T) {
	var out, errOut bytes.Buffer
	code := report(mfa.RekeyResult{Scanned: 3}, true, 2, &out, &errOut)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "3 row(s) pending re-encryption to version 2")
	assert.Empty(t, errOut.String())
}

func TestReport_LiveCleanRun(t *testing.T) {
	var out, errOut bytes.Buffer
	code := report(mfa.RekeyResult{Rekeyed: 5, Skipped: 1}, false, 2, &out, &errOut)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "rekeyed 5")
	assert.Contains(t, out.String(), "skipped 1")
}

func TestReport_FailuresExitNonZero(t *testing.T) {
	var out, errOut bytes.Buffer
	res := mfa.RekeyResult{
		Rekeyed: 1,
		Failed: []mfa.RekeyFailure{
			{UserID: "11111111-1111-1111-1111-111111111111", SealedVersion: 3, Err: errors.New("no key for version 3 (active version 2)")},
		},
	}
	code := report(res, false, 2, &out, &errOut)
	assert.Equal(t, 1, code)
	assert.True(t, strings.Contains(errOut.String(), "sealed_version=3"))
	assert.True(t, strings.Contains(errOut.String(), "11111111-1111-1111-1111-111111111111"))
}

// TestRunMain_BadArgsExitsTwo covers the correctness guard end-to-end: a stray
// positional argument makes runMain return exit 2 before any DB connection.
func TestRunMain_BadArgsExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runMain(context.Background(), []string{"unexpected-positional"}, &out, &errOut)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "unexpected argument")
}

// TestRunMain_InvalidKeyringExitsTwo covers the setup-error path: a zero keyring
// version makes ParseKeyring fail closed before any DB connection, so runMain
// returns exit code 2. No database required.
func TestRunMain_InvalidKeyringExitsTwo(t *testing.T) {
	t.Setenv("MFA_ENCRYPTION_KEY_VERSION", "0") // ParseKeyring rejects version < 1

	var out, errOut bytes.Buffer
	code := runMain(context.Background(), []string{"--dry-run"}, &out, &errOut)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "invalid MFA encryption keyring")
}

// TestRunMain_UnreachableDatabaseExitsTwo covers the connect/ping-failure path:
// a valid keyring but an unreachable database makes runMain return exit code 2.
func TestRunMain_UnreachableDatabaseExitsTwo(t *testing.T) {
	// Port 1 is never a Postgres; connect_timeout bounds the ping.
	t.Setenv("DATABASE_URL", "postgres://x:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")

	var out, errOut bytes.Buffer
	code := runMain(context.Background(), nil, &out, &errOut)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "database")
}

// TestRunMain_DryRunAgainstDatabase drives the full orchestration (config load →
// keyring → connect → Rekey → report) against the migrated test DB. --dry-run
// mutates nothing, so it is safe against the shared DB. SetupTestDB guarantees
// the migration 000092 key_version column exists before runMain opens its own
// connection to the same DATABASE_URL.
func TestRunMain_DryRunAgainstDatabase(t *testing.T) {
	testdb.SetupTestDB(t)

	var out, errOut bytes.Buffer
	code := runMain(context.Background(), []string{"--dry-run"}, &out, &errOut)
	require.Equal(t, 0, code, "stderr: %s", errOut.String())
	assert.Contains(t, out.String(), "pending re-encryption to version")
}

// TestRunMain_AbortedRunPrintsPartialResultAndExitsOne drives an aborted live run
// end to end: one undecryptable row, met first, lands in Failed; the next row's
// rewrap is made to look material by a test trigger, which aborts the run. The
// operator still gets the partial result (the Failed row is their remediation
// list), then the error, and the exit status is 1.
func TestRunMain_AbortedRunPrintsPartialResultAndExitsOne(t *testing.T) {
	const (
		retiredKey = "0101010101010101010101010101010101010101010101010101010101010101"
		activeKey  = "0202020202020202020202020202020202020202020202020202020202020202"
		// Rows are visited in user_id order, so failedID is met before abortID.
		failedID = "11111111-1111-1111-1111-111111111111"
		abortID  = "22222222-2222-2222-2222-222222222222"
	)
	db, _ := testdb.SetupTestDB(t)

	t.Setenv("MFA_ENCRYPTION_KEY", activeKey)
	t.Setenv("MFA_ENCRYPTION_KEY_VERSION", "2")
	t.Setenv("MFA_ENCRYPTION_KEYS_RETIRED", "1:"+retiredKey)

	oldRing, err := mfa.ParseKeyring(retiredKey, 1, "")
	require.NoError(t, err)
	enc, nonce, ver, err := oldRing.Seal([]byte("JBSWY3DPEHPK3PXP"))
	require.NoError(t, err)

	for _, id := range []string{failedID, abortID} {
		_, err = db.Exec(`INSERT INTO users (id, email, username, password_hash) VALUES ($1, $2, $3, 'x')`,
			id, "rekey-"+id+"@example.test", "rk"+id[:8])
		require.NoError(t, err)
		t.Cleanup(func() {
			_, removeErr := db.Exec(`DELETE FROM users WHERE id = $1`, id)
			assert.NoError(t, removeErr)
		})
	}
	// failedID: sealed under key_version 3, which is in no ring.
	_, err = db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, 3, TRUE, TRUE)`, failedID, []byte("sealed-under-nothing"), []byte("nonce"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, TRUE, TRUE)`, abortID, enc, nonce, ver)
	require.NoError(t, err)

	const (
		dropTrigger = `DROP TRIGGER IF EXISTS zz_rekey_force_version_change ON user_mfa_totp`
		dropFunc    = `DROP FUNCTION IF EXISTS rekey_force_version_change()`
	)
	dropAll := func() {
		_, dropErr := db.Exec(dropTrigger)
		assert.NoError(t, dropErr)
		_, dropErr = db.Exec(dropFunc)
		assert.NoError(t, dropErr)
	}
	dropAll()
	t.Cleanup(dropAll)
	_, err = db.Exec(`CREATE FUNCTION rekey_force_version_change() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN NEW.material_version := NEW.material_version + 1; RETURN NEW; END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER zz_rekey_force_version_change BEFORE UPDATE ON user_mfa_totp
		FOR EACH ROW EXECUTE FUNCTION rekey_force_version_change()`)
	require.NoError(t, err)

	var out, errOut bytes.Buffer
	code := runMain(context.Background(), nil, &out, &errOut)

	assert.Equal(t, 1, code, "an aborted run must exit non-zero; stderr: %s", errOut.String())
	assert.Contains(t, out.String(), "rekey aborted; partial result:")
	assert.Contains(t, out.String(), "rekeyed 0, skipped 0 (concurrent rewrite), failed 1")
	assert.Contains(t, errOut.String(), "FAILED user="+failedID+" sealed_version=3")
	assert.Contains(t, errOut.String(), "rekey: ")
	assert.Contains(t, errOut.String(), "rewrap exemption not applied")
	assert.Contains(t, errOut.String(), "reseal user "+abortID)
	assert.Less(t, strings.Index(errOut.String(), "FAILED user="), strings.Index(errOut.String(), "rekey: "),
		"the partial result is printed before the error")

	var keyVersion int
	require.NoError(t, db.QueryRow(`SELECT key_version FROM user_mfa_totp WHERE user_id = $1`, abortID).Scan(&keyVersion))
	assert.Equal(t, 1, keyVersion, "the aborted row must be rolled back")
}
