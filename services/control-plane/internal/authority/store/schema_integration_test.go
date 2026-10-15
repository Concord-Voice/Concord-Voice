package store

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"database/sql"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

const testRealm = "11111111-1111-4111-8111-111111111111"
const testAcct = "22222222-2222-4222-8222-222222222222"

func d48(b byte) []byte { return bytes.Repeat([]byte{b}, 48) }

// p97 is b·G on P-384, uncompressed. A1's codec admits only on-curve points
// at every b97 position, so a filler point would not encode.
func p97(b byte) []byte {
	k := make([]byte, 48)
	k[47] = b
	priv, err := ecdh.P384().NewPrivateKey(k)
	if err != nil {
		panic(err)
	}
	return priv.PublicKey().Bytes()
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func mustFail(t *testing.T, db *sql.DB, wantSubstr, q string, args ...any) {
	t.Helper()
	_, err := db.Exec(q, args...)
	if err == nil || !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("%s = %v, want an error containing %q", q, err, wantSubstr)
	}
}

// seedOneEvent writes a delegation and one account event as the superuser.
func seedOneEvent(t *testing.T, super *sql.DB) {
	t.Helper()
	mustExec(t, super, `INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b, digest)
		VALUES (0, 1, '\x00', $1, $1, $2)`, make([]byte, 96), d48(1))
	mustExec(t, super, `INSERT INTO authority.account_events (account_id, seq, restore_epoch, request_id, intent_digest,
		op, bundle, cp_evidence, entry_digest, prev_entry_digest, delegation_gen, delegation_serial, issued_at_ms,
		asserted_at_ms, imported) VALUES ($1, 1, 0, gen_random_uuid(), $2, 'enroll_new', '\x00', '\x00', $3, NULL,
		0, 1, 1, 1, false)`, testAcct, d48(2), d48(3))
}

func insertAbsentHead(t *testing.T, db *sql.DB, acct string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO authority.account_heads (account_id, state, seq, recovery_gen, susp_operator, susp_user,
		root_revoked, legacy_pin_revoked) VALUES ($1, 'absent', 0, 0, false, false, false, false)`, acct)
}

// lockTimeoutConn pins one connection with a 200 ms lock_timeout, so a statement
// that waits on a row lock fails with "canceling statement due to lock timeout".
func lockTimeoutConn(t *testing.T, db *sql.DB) *sql.Conn {
	t.Helper()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.ExecContext(context.Background(), `SET lock_timeout = '200ms'`); err != nil {
		t.Fatal(err)
	}
	return c
}

// beginHolding opens a transaction on db and runs q in it, so the test holds q's locks.
func beginHolding(t *testing.T, db *sql.DB, q string, args ...any) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return tx
}

func TestMetaGuards(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	mustFail(t, f.Super, "realm_id is immutable", `UPDATE authority.meta SET realm_id = gen_random_uuid()`)
	mustExec(t, f.Owner, `UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`, d48(4))
	mustFail(t, f.Super, "set once", `UPDATE authority.meta SET legacy_sealed_at = clock_timestamp()`)
	mustFail(t, f.Super, "set once", `UPDATE authority.meta SET population_digest = $1`, d48(5))
	mustExec(t, f.App, `UPDATE authority.meta SET cancel_unavailable_ms = 10`)
	mustFail(t, f.App, "never decreases", `UPDATE authority.meta SET cancel_unavailable_ms = 9`)
	mustFail(t, f.Super, "check", `INSERT INTO authority.meta (id, realm_id, last_tick_at) VALUES (2, gen_random_uuid(), now())`)
}

// PR review: row triggers never fire on TRUNCATE, so each never-deleted table
// refuses it with a statement trigger, for the owner too.
func TestOwnerCannotTruncateTheNeverDeletedTables(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	mustFail(t, f.Owner, "never deleted", `DELETE FROM authority.meta`)
	for q, want := range map[string]string{
		`TRUNCATE authority.meta`: "never deleted",
		// Two event tables reference delegations, so only CASCADE gets past the FK check.
		`TRUNCATE authority.delegations CASCADE`: "never deleted",
		`TRUNCATE authority.successions`:         "insert only",
		`TRUNCATE authority.revoked_keys`:        "insert only",
	} {
		mustFail(t, f.Owner, want, q)
	}
}

// §3.2.5 Grants (F1, AC15): every forbidden write fails as authority_app.
func TestAppRoleGrants(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	mustFail(t, f.App, "permission denied", `UPDATE authority.meta SET reconciled = false`)
	mustFail(t, f.App, "permission denied", `UPDATE authority.meta SET legacy_sealed_at = now()`)
	mustFail(t, f.App, "permission denied", `UPDATE authority.meta SET replication_required = false`)
	seedOneEvent(t, f.Super)
	mustFail(t, f.App, "permission denied", `DELETE FROM authority.account_events`)
	mustFail(t, f.App, "permission denied", `DELETE FROM authority.request_ledger`)
	mustFail(t, f.App, "permission denied", `DELETE FROM authority.key_bindings`)
	mustFail(t, f.App, "permission denied", `UPDATE authority.account_events SET op = 'erase'`)
	mustFail(t, f.App, "permission denied", `UPDATE authority.delegations SET drain_grace_ms = 0`)
	mustFail(t, f.App, "permission denied", `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cak')`, d48(6))
	var can bool
	if err := f.App.QueryRow(`SELECT has_column_privilege('authority.meta', 'reconciled', 'UPDATE')`).Scan(&can); err != nil || can {
		t.Fatalf("has_column_privilege(meta.reconciled, UPDATE) = %v, %v; want false", can, err)
	}
	if err := f.App.QueryRow(`SELECT has_column_privilege('authority.account_heads', 'account_id', 'UPDATE')`).Scan(&can); err != nil || can {
		t.Fatal("authority_app may UPDATE account_heads.account_id")
	}
	if err := f.App.QueryRow(`SELECT has_table_privilege('authority.account_events', 'TRUNCATE')`).Scan(&can); err != nil || can {
		t.Fatal("authority_app holds TRUNCATE on account_events")
	}
}

func TestCommittedAtIsForcedOnBothEventTables(t *testing.T) {
	f := authoritydb.Setup(t)
	mustExec(t, f.Super, `INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b, digest)
		VALUES (0, 1, '\x00', $1, $1, $2)`, make([]byte, 96), d48(1))
	mustExec(t, f.App, `INSERT INTO authority.account_events (account_id, seq, restore_epoch, request_id, intent_digest,
		op, bundle, cp_evidence, entry_digest, delegation_gen, delegation_serial, issued_at_ms, asserted_at_ms, imported,
		committed_at) VALUES ($1, 1, 0, gen_random_uuid(), $2, 'enroll_new', '\x00', '\x00', $3, 0, 1, 1, 1, false,
		'2000-01-01')`, testAcct, d48(2), d48(3))
	mustExec(t, f.App, `INSERT INTO authority.device_events (account_id, dseq, request_id, device_intent_digest, op,
		chain_seq, chain_entry_digest, bundle, entry_digest, issued_at_ms, delegation_gen, delegation_serial, imported,
		committed_at) VALUES ($1, 1, gen_random_uuid(), $2, 'enroll_new', 1, $3, '\x00', $4, 1, 0, 1, false, '2000-01-01')`,
		testAcct, d48(4), d48(3), d48(5))
	for _, tbl := range []string{"account_events", "device_events"} {
		var old bool
		if err := f.Super.QueryRow(`SELECT committed_at < '2001-01-01' FROM authority.` + tbl).Scan(&old); err != nil || old {
			t.Fatalf("%s.committed_at was not forced to clock_timestamp()", tbl)
		}
	}
}

func TestHeadTriggersAndChecks(t *testing.T) {
	f := authoritydb.Setup(t)
	insertAbsentHead(t, f.App, testAcct)
	mustFail(t, f.App, "ah_seq_head", `UPDATE authority.account_heads SET seq = 1`)
	mustFail(t, f.App, "ah_pending", `UPDATE authority.account_heads SET state = 'pending', prior_state = 'absent'`)
	mustExec(t, f.Owner, `UPDATE authority.account_heads SET held = true`)
	mustFail(t, f.App, "held", `UPDATE authority.account_heads SET held = false`) // the held-clear trigger RAISEs
	mustExec(t, f.Owner, `UPDATE authority.account_heads SET held = false`)
	mustFail(t, f.App, "force_unverified", `UPDATE authority.account_heads SET force_unverified = true`)
	mustExec(t, f.Owner, `UPDATE authority.account_heads SET force_unverified = true`)
	mustFail(t, f.App, "force_unverified", `UPDATE authority.account_heads SET force_unverified = false`)
	mustExec(t, f.App, `UPDATE authority.account_heads SET force_unverified = false, active_root = $1, state = 'active',
		seq = 1, head_entry_digest = $2`, p97(1), d48(9))
	mustFail(t, f.App, "ah_dseq", `UPDATE authority.account_heads SET dseq = 1`)
	mustFail(t, f.App, "ah_device_set", `UPDATE authority.account_heads SET device_set = '\x00'`)
	mustFail(t, f.App, "ah_slot", `UPDATE authority.account_heads SET replacement_g_ms = 1`)
}

func TestDelegationDrainGraceOnlyByOwnerAndOnlyToZero(t *testing.T) {
	f := authoritydb.Setup(t)
	mustExec(t, f.App, `INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b, digest)
		VALUES (0, 1, '\x00', $1, $1, $2)`, make([]byte, 96), d48(1))
	mustFail(t, f.Super, "drain_grace_ms", `UPDATE authority.delegations SET drain_grace_ms = 0`) // superuser is not authority_owner
	mustExec(t, f.Owner, `UPDATE authority.delegations SET drain_grace_ms = 0`)
	mustFail(t, f.Owner, "drain_grace_ms", `UPDATE authority.delegations SET drain_grace_ms = 660000`)
	mustFail(t, f.Owner, "never deleted", `DELETE FROM authority.delegations`)
	// Review round 3: no row is born drained, whoever inserts it (DoR:1376).
	for _, db := range []*sql.DB{f.App, f.Owner} {
		mustFail(t, db, "drain_grace_ms", `INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b,
			digest, drain_grace_ms) VALUES (0, 3, '\x00', $1, $1, $2, 0)`, make([]byte, 96), d48(3))
	}
	mustFail(t, f.App, "check", `INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b, digest)
		VALUES (0, 2, '\x00', $1, $1, $2)`, make([]byte, 95), d48(2))
}

func TestLegacyPopulationRefusesInsertsAfterTheSeal(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	mustExec(t, f.Owner, `TRUNCATE authority.legacy_population`) // allowed before the seal, as DELETE is
	mustExec(t, f.App, `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`, testAcct, d48(1))
	mustExec(t, f.Owner, `UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`, d48(4))
	mustFail(t, f.Owner, "sealed", `TRUNCATE authority.legacy_population`)
	mustFail(t, f.App, "sealed", `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest)
		VALUES (gen_random_uuid(), $1)`, d48(2))
	// B1-D32 (review round 3): the sealed rows are frozen, for the owner too.
	mustFail(t, f.Owner, "sealed", `UPDATE authority.legacy_population SET legacy_ek_digest = $1`, d48(3))
	mustFail(t, f.Owner, "sealed", `DELETE FROM authority.legacy_population`)
}

// B1-D30: an insert waits for a seal that has locked meta, then sees it sealed.
// The seal's first statement (B3's SealLegacy) is the FOR UPDATE below.
func TestLegacyPopulationInsertSerializesWithTheSeal(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	seal := beginHolding(t, f.Owner, `SELECT 1 FROM authority.meta WHERE id = 1 FOR UPDATE`)
	ins := lockTimeoutConn(t, f.App)
	const q = `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`
	if _, err := ins.ExecContext(context.Background(), q, testAcct, d48(1)); err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("insert during an open seal = %v, want a lock timeout", err)
	}
	if _, err := seal.Exec(`UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`, d48(4)); err != nil {
		t.Fatal(err)
	}
	if err := seal.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := ins.ExecContext(context.Background(), q, testAcct, d48(1)); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("insert after the seal = %v, want the sealed refusal", err)
	}
}

// D-207: revocations are insert-only, for the owner too.
func TestRevokedKeysAreInsertOnly(t *testing.T) {
	f := authoritydb.Setup(t)
	mustExec(t, f.Owner, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cak')`, d48(6))
	mustFail(t, f.Owner, "insert only", `UPDATE authority.revoked_keys SET kind = 'operator'`)
	mustFail(t, f.Owner, "insert only", `DELETE FROM authority.revoked_keys`)
}

func TestStandbyNamesParsesEveryPatroniForm(t *testing.T) {
	f := authoritydb.Setup(t)
	cases := map[string]string{
		`east-2`:                   `{east-2}`,
		`"east-2"`:                 `{east-2}`,
		`FIRST 1 ("east-2")`:       `{east-2}`,
		`ANY 1 (east-2, "west-1")`: `{east-2,west-1}`,
		`*`:                        `{}`,
		``:                         `{}`,
		`first 1 (east-2, *)`:      `{east-2}`,
	}
	for in, want := range cases {
		var got string
		if err := f.Owner.QueryRow(`SELECT authority.standby_names($1)::text`, in).Scan(&got); err != nil || got != want {
			t.Fatalf("standby_names(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestReplicatedIsFalseWithNoStandby(t *testing.T) {
	f := authoritydb.Setup(t)
	var ok bool
	if err := f.App.QueryRow(`SELECT authority.replicated(pg_current_wal_insert_lsn())`).Scan(&ok); err != nil || ok {
		t.Fatalf("replicated() on a cluster with no standby = %v, %v; want false", ok, err)
	}
}

func TestPurgeRefusesWhileUnreconciled(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	insertAbsentHead(t, f.App, testAcct)
	mustExec(t, f.Owner, `SELECT authority.enter_unreconciled()`)
	var purged bool
	if err := f.App.QueryRow(`SELECT authority.purge_erased($1)`, testAcct).Scan(&purged); err != nil || purged {
		t.Fatalf("purge_erased while unreconciled = %v, %v; want false", purged, err)
	}
	var reconciled bool
	if err := f.App.QueryRow(`SELECT reconciled FROM authority.meta`).Scan(&reconciled); err != nil || reconciled {
		t.Fatal("enter_unreconciled() did not set reconciled = false")
	}
}

// B1-D30: purge_erased locks meta FOR SHARE before L1, so a tick holding the row
// (its cancel_unavailable_ms leg) makes the purge wait rather than decide on the
// value the tick is replacing. enter_unreconciled() takes the same row lock.
func TestPurgeWaitsForAMetaWriter(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	insertAbsentHead(t, f.App, testAcct)
	beginHolding(t, f.App, `UPDATE authority.meta SET cancel_unavailable_ms = cancel_unavailable_ms + 1 WHERE id = 1`)
	var purged bool
	err := lockTimeoutConn(t, f.App).QueryRowContext(context.Background(), `SELECT authority.purge_erased($1)`, testAcct).Scan(&purged)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("purge_erased during an open tick = %v, %v; want a lock timeout", purged, err)
	}
}

// B1-D11: a recorded refusal is kept for N_CANCEL_MS (2 592 000 000 ms, 30 d).
// The two defaults read clock_timestamp() separately, so allow 1 s between them.
func TestLedgerRowIsKeptForNCancel(t *testing.T) {
	f := authoritydb.Setup(t)
	mustExec(t, f.Super, `INSERT INTO authority.request_ledger (account_id, request_id, intent_digest, code, cp_evidence)
		VALUES ($1, gen_random_uuid(), $2, 'stale_seq', '\x00')`, testAcct, d48(1))
	var keepMs int64
	if err := f.Super.QueryRow(`SELECT floor(extract(epoch FROM prune_after - recorded_at) * 1000)::bigint
		FROM authority.request_ledger`).Scan(&keepMs); err != nil {
		t.Fatal(err)
	}
	if keepMs < 2_592_000_000 || keepMs >= 2_592_000_000+1_000 {
		t.Fatalf("prune_after − recorded_at = %d ms, want N_CANCEL_MS (2 592 000 000)", keepMs)
	}
}
