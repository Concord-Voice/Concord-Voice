package database_test

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000196 (#3680 C4 PR3, spec §6.2): the account-guard request tables.
// Five tables hold the pending-request state machine, its origin-credential
// provenance, the retired-credential history, the displaced-address reservations
// and the HMAC key-id registry. Nothing reads any of it until the runner is
// activated.
//
// Every behavioural test runs on migration000196Fresh, a transaction that runs
// the down and then the up FILE TEXT and is always rolled back, so the schema
// under test is the file, never whatever the shared database was migrated with.
// The ordering rule of migration_000189_test.go applies unchanged: call
// testhelpers.SetupTestDB(t) BEFORE migration000196Fresh, so the rollback runs
// before SetupTestDB's TRUNCATE cleanup and releases the DDL locks first.
//
// The helpers agSavepoint, migration000189InsertUser and agSQL come from
// migration_000189_test.go (same package).

const (
	aprRequests     = "account_pending_requests"
	aprOrigins      = "account_pending_request_origins"
	aprHistory      = "user_credential_history"
	aprReservations = "account_address_reservations"
	aprHMACKeys     = "accountguard_hmac_keys"
)

// aprTables lists the five tables in the order the migration creates them.
var aprTables = []string{aprRequests, aprOrigins, aprHistory, aprReservations, aprHMACKeys}

func migration000196UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000196_accountguard_requests.up.sql")
}

func migration000196DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000196_accountguard_requests.down.sql")
}

// migration000196Fresh opens a transaction, runs the 000196 down then up file
// text in it, and returns it. Both files are read before the transaction opens,
// so a missing file fails the test cleanly instead of leaving a transaction
// open. The rollback is registered with t.Cleanup (see the ordering rule above).
func migration000196Fresh(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	down := migration000196DownSQL(t)
	up := migration000196UpSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })

	_, err = tx.Exec(down)
	require.NoError(t, err, "the down must run on the migrated schema")
	_, err = tx.Exec(up)
	require.NoError(t, err, "the up must run after the down")
	return tx
}

// aprExpr is a SQL expression a row builder splices in as written, for values
// that must come from the database clock. Every value is a constant from this
// file, never input. Anything else in a row becomes a bind parameter.
type aprExpr string

const aprNow = aprExpr("now()")

// aprAgo and aprFromNow build a database-clock instant a Go duration before or
// after now(), so a window in a test is the decision core's constant and not a
// restated number. The duration is rendered as whole seconds from a Go value,
// never from input.
func aprAgo(d time.Duration) aprExpr {
	return aprExpr(fmt.Sprintf("now() - make_interval(secs => %d)", int64(d/time.Second)))
}

func aprFromNow(d time.Duration) aprExpr {
	return aprExpr(fmt.Sprintf("now() + make_interval(secs => %d)", int64(d/time.Second)))
}

// aprDay is the margin the down-guard cases keep either side of a window edge.
const aprDay = 24 * time.Hour

// aprFP returns a 32-byte fingerprint filled with b.
func aprFP(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// aprInsert inserts row into table. Column names are test-owned constants; values
// are bind parameters except aprExpr, which is spliced in.
func aprInsert(q migrationExecer, table string, row map[string]any) error {
	cols := make([]string, 0, len(row))
	for c := range row {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	vals := make([]string, 0, len(cols))
	args := make([]any, 0, len(cols))
	for _, c := range cols {
		if expr, ok := row[c].(aprExpr); ok {
			vals = append(vals, string(expr))
			continue
		}
		args = append(args, row[c])
		vals = append(vals, "$"+strconv.Itoa(len(args)))
	}
	_, err := q.Exec(agSQL(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(cols, ", "), strings.Join(vals, ", ")), args...)
	return err
}

// aprRow is the minimal VALID open request (a pending credential_remove) for user
// u, with over applied on top. A nil override value writes NULL.
func aprRow(u uuid.UUID, over map[string]any) map[string]any {
	row := map[string]any{
		"user_id":                 u,
		"kind":                    "credential_remove",
		"target_kind":             "totp",
		"target_id":               "cred-seed",
		"target_material_version": int64(7),
		"status":                  "pending",
		"origin_class":            1,
		"origin_fingerprint":      aprFP(0xA1),
		"opened_at":               aprNow,
		"completes_at":            aprFromNow(decide.DeltaCP),
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

// o is shorthand for an override map.
type o = map[string]any

func aprCompleted(u uuid.UUID, over o) map[string]any {
	row := aprRow(u, o{"status": "completed", "completed_at": aprNow})
	for k, v := range over {
		row[k] = v
	}
	return row
}

func aprCancelled(u uuid.UUID, reason string, over o) map[string]any {
	row := aprRow(u, o{"status": "cancelled", "cancelled_at": aprNow, "cancel_reason": reason})
	for k, v := range over {
		row[k] = v
	}
	return row
}

func aprDisputed(u uuid.UUID, over o) map[string]any {
	row := aprRow(u, o{"status": "disputed", "disputed_at": aprNow, "dispute_group_id": uuid.New()})
	for k, v := range over {
		row[k] = v
	}
	return row
}

func aprBreakGlass(u uuid.UUID, over o) map[string]any {
	row := aprRow(u, o{"kind": "break_glass", "target_kind": nil, "target_id": nil, "target_material_version": nil})
	for k, v := range over {
		row[k] = v
	}
	return row
}

func aprHistoryPassword(u uuid.UUID, over o) map[string]any {
	row := map[string]any{
		"id":            uuid.New(),
		"user_id":       u,
		"kind":          "password",
		"password_hash": "hash-seed", // pragma: allowlist secret
		"retired_at":    aprNow,
		"was_mature":    true,
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

func aprHistoryAddress(u uuid.UUID, over o) map[string]any {
	row := map[string]any{
		"id":           uuid.New(),
		"user_id":      u,
		"kind":         "address",
		"address":      "old-" + uuid.NewString() + "@example.test",
		"retired_at":   aprNow,
		"was_mature":   true,
		"was_primary":  true,
		"was_verified": true,
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

func aprReservation(u, request uuid.UUID, over o) map[string]any {
	row := map[string]any{
		"address":      "res-" + uuid.NewString() + "@example.test",
		"user_id":      u,
		"request_id":   request,
		"reason":       "displaced_primary",
		"displaced_at": aprNow,
		"floor_until":  aprFromNow(decide.NCancelCP),
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

// aprFixture is a Fresh transaction with two users, one HMAC key and one
// completed request (which the down guard must not count as live state).
type aprFixture struct {
	Tx    *sql.Tx
	User  uuid.UUID
	Other uuid.UUID
	// KeyID is a registered accountguard_hmac_keys id, so a fingerprint_key_id
	// that a foreign key or lookup might check is always real.
	KeyID int16
	// Request is a completed request owned by User.
	Request uuid.UUID
}

func migration000196Seeded(t *testing.T) aprFixture {
	t.Helper()
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000196Fresh(t, db)
	f := aprFixture{Tx: tx, User: uuid.New(), Other: uuid.New(), Request: uuid.New()}
	require.NoError(t, migration000189InsertUser(tx, f.User))
	require.NoError(t, migration000189InsertUser(tx, f.Other))
	require.NoError(t, tx.QueryRow(`INSERT INTO accountguard_hmac_keys (key_check) VALUES ($1) RETURNING id`, aprFP(0x11)).Scan(&f.KeyID))
	require.NoError(t, aprInsert(tx, aprRequests, aprCompleted(f.User, o{"id": f.Request})))
	return f
}

// aprPresent reports which of the five tables exist as tx sees them.
func aprPresent(t *testing.T, tx *sql.Tx) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, table := range aprTables {
		var present bool
		require.NoError(t, tx.QueryRow(`SELECT to_regclass(quote_ident(current_schema()) || '.' || quote_ident($1::text)) IS NOT NULL`, table).Scan(&present))
		out[table] = present
	}
	return out
}

func aprRequireAll(t *testing.T, tx *sql.Tx, want bool, msg string) {
	t.Helper()
	for table, present := range aprPresent(t, tx) {
		assert.Equalf(t, want, present, "%s: table %s", msg, table)
	}
}

func aprCount(t *testing.T, tx *sql.Tx, query string, arg any) int {
	t.Helper()
	var n int
	require.NoError(t, tx.QueryRow(query, arg).Scan(&n))
	return n
}

// ---------------------------------------------------------------------------
// Round trip.
// ---------------------------------------------------------------------------

func TestMigration000196_UpDownUpConstraintsIdentical(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	up, down := migration000196UpSQL(t), migration000196DownSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })
	run := func(sqlText string) {
		t.Helper()
		_, execErr := tx.Exec(sqlText)
		require.NoError(t, execErr)
	}
	snapshot := func() map[string][]string {
		t.Helper()
		out := map[string][]string{}
		for _, table := range aprTables {
			out[table] = migrationSchemaSnapshot(t, tx, table)
		}
		return out
	}

	run(down)
	gone := snapshot()
	aprRequireAll(t, tx, false, "after the down")
	run(up)
	s1 := snapshot()
	aprRequireAll(t, tx, true, "after the up")
	run(down)
	goneAgain := snapshot()
	run(up)
	s1Again := snapshot()

	// An empty snapshot equals another empty one, so prove the comparison is not
	// vacuous before trusting it: the down leaves nothing, the up builds a lot.
	for _, table := range aprTables {
		assert.Empty(t, gone[table], "the down must remove %s completely", table)
		assert.Empty(t, goneAgain[table], "the second down must remove %s completely", table)
		assert.NotEmpty(t, migrationSnapshotLine(s1[table], "constraint "), "%s must carry constraints", table)
	}
	assert.Equal(t, s1, s1Again, "up after down must rebuild the same constraints, indexes and columns")

	// The eight named request CHECKs and the history CHECK exist under their names.
	for _, name := range []string{
		"apr_disputed_fields", "apr_completed_fields", "apr_cancelled_fields", "apr_canceller_class",
		"apr_terminal_payload", "apr_fingerprint_retention", "apr_target_shape", "apr_key_id_pairs",
	} {
		assert.NotEmpty(t, migrationSnapshotLine(s1[aprRequests], "constraint "+name+" CHECK"), "missing CHECK %s", name)
	}
	assert.NotEmpty(t, migrationSnapshotLine(s1[aprHistory], "constraint uch_kind_fields CHECK"), "missing CHECK uch_kind_fields")
}

// The indexes spec §6.2 and R7 list, found by the pieces of their definition so
// the index names stay the migration's choice.
func TestMigration000196_IndexesMatchSpec(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000196Fresh(t, db)

	indexLines := func(table string) []string {
		var out []string
		for _, line := range migrationSchemaSnapshot(t, tx, table) {
			if strings.HasPrefix(line, "index ") {
				out = append(out, line)
			}
		}
		return out
	}
	requireIndex := func(table string, fragments ...string) {
		t.Helper()
		for _, line := range indexLines(table) {
			matches := true
			for _, fragment := range fragments {
				matches = matches && strings.Contains(line, fragment)
			}
			if matches {
				return
			}
		}
		assert.Failf(t, "missing index", "%s: no index containing %q", table, fragments)
	}

	requireIndex(aprRequests, "btree (completes_at)", "pending")
	requireIndex(aprRequests, "btree (user_id, status)")
	requireIndex(aprRequests, "btree (user_id, request_fingerprint, cancelled_at)", "proof", "dispute_resolved")
	requireIndex(aprRequests, "UNIQUE", "btree (user_id)", "break_glass", "pending")
	requireIndex(aprRequests, "btree (fingerprint_key_id)")
	requireIndex(aprHistory, "btree (kind, address)", "address")
	requireIndex(aprHistory, "btree (user_id, kind, retired_at)")
	requireIndex(aprReservations, "btree (user_id)")
}

// ---------------------------------------------------------------------------
// CHECK matrix.
// ---------------------------------------------------------------------------

func TestMigration000196_CheckMatrix(t *testing.T) {
	f := migration000196Seeded(t)
	u, key := f.User, f.KeyID

	// reject names the violated constraint; "-" means a column CHECK whose
	// auto-generated name the test does not pin (any 23514 qualifies). An empty
	// reject admits the row. Every rejecting row is valid in every other respect,
	// so the named constraint is the only thing that can refuse it.
	cases := []struct {
		name   string
		table  string
		row    map[string]any
		reject string
	}{
		// --- account_pending_requests: rows that must be admitted.
		{"open pending credential_remove", aprRequests, aprRow(u, nil), ""},
		{"open pending with request fingerprint and key id", aprRequests, aprRow(u, o{"request_fingerprint": aprFP(0xB2), "fingerprint_key_id": key, "coarse_fingerprint": aprFP(0xC3)}), ""},
		{"open pending holding payloads and held_since", aprRequests, aprRow(u, o{"pending_password_hash": "hash-pending", "pending_address": "new@example.test", "held_since": aprNow, "held_ms": int64(5)}), ""}, // pragma: allowlist secret
		{"disputed with disputed_at and group id", aprRequests, aprDisputed(u, nil), ""},
		{"completed with completed_at", aprRequests, aprCompleted(u, nil), ""},
		{"cancelled superseded with reason and cancelled_at", aprRequests, aprCancelled(u, "superseded", nil), ""},
		{"cancelled proof keeps fingerprints and canceller class", aprRequests, aprCancelled(u, "proof", o{"canceller_class": 2, "request_fingerprint": aprFP(0xB2), "fingerprint_key_id": key, "coarse_fingerprint": aprFP(0xC3)}), ""},
		{"cancelled dispute_resolved keeps fingerprints and canceller class", aprRequests, aprCancelled(u, "dispute_resolved", o{"canceller_class": 3, "request_fingerprint": aprFP(0xB2), "fingerprint_key_id": key}), ""},
		{"break_glass with no target", aprRequests, aprBreakGlass(u, nil), ""},

		// --- apr_disputed_fields.
		{"disputed without disputed_at", aprRequests, aprDisputed(u, o{"disputed_at": nil}), "apr_disputed_fields"},
		{"disputed without dispute_group_id", aprRequests, aprDisputed(u, o{"dispute_group_id": nil}), "apr_disputed_fields"},
		{"disputed with neither", aprRequests, aprDisputed(u, o{"disputed_at": nil, "dispute_group_id": nil}), "apr_disputed_fields"},

		// --- apr_completed_fields.
		{"completed without completed_at", aprRequests, aprCompleted(u, o{"completed_at": nil}), "apr_completed_fields"},

		// --- apr_cancelled_fields.
		{"cancelled without cancel_reason", aprRequests, aprCancelled(u, "superseded", o{"cancel_reason": nil}), "apr_cancelled_fields"},
		{"cancelled without cancelled_at", aprRequests, aprCancelled(u, "superseded", o{"cancelled_at": nil}), "apr_cancelled_fields"},
		// The other direction: cancel facts only on a cancelled row.
		{"pending carrying a proof cancel_reason", aprRequests, aprRow(u, o{"cancel_reason": "proof", "canceller_class": 1}), "apr_cancelled_fields"},
		{"pending carrying a superseded cancel_reason", aprRequests, aprRow(u, o{"cancel_reason": "superseded"}), "apr_cancelled_fields"},
		{"completed carrying cancelled_at", aprRequests, aprCompleted(u, o{"cancelled_at": aprNow}), "apr_cancelled_fields"},
		{"pending carrying cancelled_at", aprRequests, aprRow(u, o{"cancelled_at": aprNow}), "apr_cancelled_fields"},

		// --- apr_canceller_class: set exactly when the reason is proof or dispute_resolved.
		{"proof cancel without canceller_class", aprRequests, aprCancelled(u, "proof", nil), "apr_canceller_class"},
		{"dispute_resolved cancel without canceller_class", aprRequests, aprCancelled(u, "dispute_resolved", nil), "apr_canceller_class"},
		{"superseded cancel with canceller_class", aprRequests, aprCancelled(u, "superseded", o{"canceller_class": 1}), "apr_canceller_class"},
		{"hold_cap cancel with canceller_class", aprRequests, aprCancelled(u, "hold_cap", o{"canceller_class": 2}), "apr_canceller_class"},
		{"dispute_expired cancel with canceller_class", aprRequests, aprCancelled(u, "dispute_expired", o{"canceller_class": 3}), "apr_canceller_class"},

		// --- apr_terminal_payload: a terminal row holds no payload and no held_since.
		{"completed holding pending_password_hash", aprRequests, aprCompleted(u, o{"pending_password_hash": "hash-pending"}), "apr_terminal_payload"}, // pragma: allowlist secret
		{"completed holding pending_address", aprRequests, aprCompleted(u, o{"pending_address": "new@example.test"}), "apr_terminal_payload"},
		{"completed holding held_since", aprRequests, aprCompleted(u, o{"held_since": aprNow}), "apr_terminal_payload"},
		{"cancelled holding pending_password_hash", aprRequests, aprCancelled(u, "superseded", o{"pending_password_hash": "hash-pending"}), "apr_terminal_payload"}, // pragma: allowlist secret
		{"cancelled holding pending_address", aprRequests, aprCancelled(u, "superseded", o{"pending_address": "new@example.test"}), "apr_terminal_payload"},
		{"cancelled holding held_since", aprRequests, aprCancelled(u, "superseded", o{"held_since": aprNow}), "apr_terminal_payload"},

		// --- apr_fingerprint_retention: fingerprints survive only an open row or a proof/dispute_resolved cancel.
		{"completed keeping request_fingerprint", aprRequests, aprCompleted(u, o{"request_fingerprint": aprFP(0xB2), "fingerprint_key_id": key}), "apr_fingerprint_retention"},
		{"completed keeping coarse_fingerprint", aprRequests, aprCompleted(u, o{"coarse_fingerprint": aprFP(0xC3)}), "apr_fingerprint_retention"},
		{"superseded cancel keeping request_fingerprint", aprRequests, aprCancelled(u, "superseded", o{"request_fingerprint": aprFP(0xB2), "fingerprint_key_id": key}), "apr_fingerprint_retention"},
		{"hold_cap cancel keeping coarse_fingerprint", aprRequests, aprCancelled(u, "hold_cap", o{"coarse_fingerprint": aprFP(0xC3)}), "apr_fingerprint_retention"},
		{"dispute_expired cancel keeping request_fingerprint", aprRequests, aprCancelled(u, "dispute_expired", o{"request_fingerprint": aprFP(0xB2), "fingerprint_key_id": key}), "apr_fingerprint_retention"},

		// --- apr_target_shape: break_glass has no target, credential_remove has all three.
		{"break_glass with target_kind", aprRequests, aprBreakGlass(u, o{"target_kind": "totp"}), "apr_target_shape"},
		{"break_glass with target_id", aprRequests, aprBreakGlass(u, o{"target_id": "cred-seed"}), "apr_target_shape"},
		{"break_glass with target_material_version", aprRequests, aprBreakGlass(u, o{"target_material_version": int64(7)}), "apr_target_shape"},
		{"credential_remove without target_kind", aprRequests, aprRow(u, o{"target_kind": nil}), "apr_target_shape"},
		{"credential_remove without target_id", aprRequests, aprRow(u, o{"target_id": nil}), "apr_target_shape"},
		{"credential_remove without target_material_version", aprRequests, aprRow(u, o{"target_material_version": nil}), "apr_target_shape"},

		// --- apr_key_id_pairs: request_fingerprint and fingerprint_key_id are set together.
		{"request_fingerprint without fingerprint_key_id", aprRequests, aprRow(u, o{"request_fingerprint": aprFP(0xB2)}), "apr_key_id_pairs"},
		{"fingerprint_key_id without request_fingerprint", aprRequests, aprRow(u, o{"fingerprint_key_id": key}), "apr_key_id_pairs"},

		// --- account_pending_requests column CHECKs (auto-named).
		{"kind outside the enum", aprRequests, aprRow(u, o{"kind": "bogus"}), "-"},
		{"status outside the enum", aprRequests, aprRow(u, o{"status": "bogus"}), "-"},
		{"origin_class 0", aprRequests, aprRow(u, o{"origin_class": 0}), "-"},
		{"origin_class 4", aprRequests, aprRow(u, o{"origin_class": 4}), "-"},
		{"origin_fingerprint of 31 bytes", aprRequests, aprRow(u, o{"origin_fingerprint": aprFP(0xA1)[:31]}), "-"},
		{"negative held_ms", aprRequests, aprRow(u, o{"held_ms": int64(-1)}), "-"},
		{"cancel_reason outside the enum", aprRequests, aprCancelled(u, "bogus", nil), "-"},
		{"canceller_class 4", aprRequests, aprCancelled(u, "proof", o{"canceller_class": 4}), "-"},
		{"canceller_class 0", aprRequests, aprCancelled(u, "proof", o{"canceller_class": 0}), "-"},

		// --- user_credential_history / uch_kind_fields.
		{"history password row", aprHistory, aprHistoryPassword(u, nil), ""},
		{"history address row", aprHistory, aprHistoryAddress(u, nil), ""},
		{"history address row that was not primary or verified", aprHistory, aprHistoryAddress(u, o{"was_primary": false, "was_verified": false}), ""},
		{"history password without password_hash", aprHistory, aprHistoryPassword(u, o{"password_hash": nil}), "uch_kind_fields"},
		{"history password carrying an address", aprHistory, aprHistoryPassword(u, o{"address": "x@example.test"}), "uch_kind_fields"},
		{"history address without address", aprHistory, aprHistoryAddress(u, o{"address": nil}), "uch_kind_fields"},
		{"history address without was_primary", aprHistory, aprHistoryAddress(u, o{"was_primary": nil}), "uch_kind_fields"},
		{"history address without was_verified", aprHistory, aprHistoryAddress(u, o{"was_verified": nil}), "uch_kind_fields"},
		{"history address carrying a password_hash", aprHistory, aprHistoryAddress(u, o{"password_hash": "hash-seed"}), "uch_kind_fields"}, // pragma: allowlist secret
		{"history kind outside the enum", aprHistory, aprHistoryPassword(u, o{"kind": "bogus"}), "-"},

		// --- account_address_reservations.
		{"reservation", aprReservations, aprReservation(u, f.Request, nil), ""},
		{"reservation reason outside the enum", aprReservations, aprReservation(u, f.Request, o{"reason": "bogus"}), "-"},

		// --- address normalization: stored addresses are lower-cased and trimmed.
		{"reservation address lower-trimmed", aprReservations, aprReservation(u, f.Request, o{"address": "norm@example.test"}), ""},
		{"reservation address mixed case", aprReservations, aprReservation(u, f.Request, o{"address": "Norm@Example.test"}), "aar_address_normalized"},
		{"reservation address with leading space", aprReservations, aprReservation(u, f.Request, o{"address": " norm@example.test"}), "aar_address_normalized"},
		{"reservation address with trailing space", aprReservations, aprReservation(u, f.Request, o{"address": "norm@example.test "}), "aar_address_normalized"},
		{"history address lower-trimmed", aprHistory, aprHistoryAddress(u, o{"address": "norm@example.test"}), ""},
		{"history address mixed case", aprHistory, aprHistoryAddress(u, o{"address": "Norm@Example.test"}), "uch_address_normalized"},
		{"history address with leading space", aprHistory, aprHistoryAddress(u, o{"address": " norm@example.test"}), "uch_address_normalized"},
		{"history address with trailing space", aprHistory, aprHistoryAddress(u, o{"address": "norm@example.test "}), "uch_address_normalized"},
		{"pending request pending_address lower-trimmed", aprRequests, aprRow(u, o{"pending_address": "norm@example.test"}), ""},
		{"pending request pending_address mixed case", aprRequests, aprRow(u, o{"pending_address": "Norm@Example.test"}), "apr_pending_address_normalized"},
		{"pending request pending_address with leading space", aprRequests, aprRow(u, o{"pending_address": " norm@example.test"}), "apr_pending_address_normalized"},
		{"pending request pending_address with trailing space", aprRequests, aprRow(u, o{"pending_address": "norm@example.test "}), "apr_pending_address_normalized"},

		// --- accountguard_hmac_keys.
		{"hmac key of 32 bytes", aprHMACKeys, o{"key_check": aprFP(0x22)}, ""},
		{"hmac key_check of 31 bytes", aprHMACKeys, o{"key_check": aprFP(0x22)[:31]}, "-"},
		{"hmac key_check of 33 bytes", aprHMACKeys, o{"key_check": append(aprFP(0x22), 0x22)}, "-"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agSavepoint(t, f.Tx, func() {
				err := aprInsert(f.Tx, tc.table, tc.row)
				switch tc.reject {
				case "":
					assert.NoError(t, err, "the minimal valid row must be admitted")
				case "-":
					migrationRequirePQError(t, err, "23514", "")
				default:
					migrationRequirePQError(t, err, "23514", tc.reject)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// A-20: no unique index over (dispute_group_id, origin_fingerprint).
// ---------------------------------------------------------------------------

func TestMigration000196_NoDisputeOriginUniqueIndex(t *testing.T) {
	f := migration000196Seeded(t)

	rows, err := f.Tx.Query(`
		SELECT i.indexrelid::regclass::text,
		       array_agg(COALESCE(a.attname::text, '<expression>') ORDER BY k.ord)
		  FROM pg_index i
		  CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
		  LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		 WHERE i.indrelid = to_regclass(quote_ident(current_schema()) || '.account_pending_requests')
		   AND k.ord <= i.indnkeyatts
		 GROUP BY i.indexrelid`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	keys := map[string][]string{}
	for rows.Next() {
		var name string
		var cols []string
		require.NoError(t, rows.Scan(&name, pq.Array(&cols)))
		keys[name] = cols
	}
	require.NoError(t, rows.Err())

	// Positive controls: the inspector sees the primary key and a known index, so
	// an empty map or a broken join cannot pass the loop below by finding nothing.
	var sawPrimaryKey, sawCompletesAt bool
	for _, cols := range keys {
		sawPrimaryKey = sawPrimaryKey || (len(cols) == 1 && cols[0] == "id")
		sawCompletesAt = sawCompletesAt || (len(cols) == 1 && cols[0] == "completes_at")
	}
	require.True(t, sawPrimaryKey, "the index inspector must see the primary key on id: %v", keys)
	require.True(t, sawCompletesAt, "the index inspector must see the completes_at index: %v", keys)

	for name, cols := range keys {
		sorted := append([]string(nil), cols...)
		sort.Strings(sorted)
		assert.NotEqual(t, []string{"dispute_group_id", "origin_fingerprint"}, sorted,
			"index %s keys (dispute_group_id, origin_fingerprint): A-20 forbids any index on that pair", name)
	}

	// Behaviour: two disputed requests of one group may carry one origin fingerprint.
	group := uuid.New()
	for _, target := range []string{"cred-a", "cred-b"} {
		require.NoError(t, aprInsert(f.Tx, aprRequests,
			aprDisputed(f.User, o{"dispute_group_id": group, "origin_fingerprint": aprFP(0xD4), "target_id": target})),
			"disputed row for %s", target)
	}
}

// ---------------------------------------------------------------------------
// One pending break_glass per user.
// ---------------------------------------------------------------------------

func TestMigration000196_OnePendingBreakGlassPerUser(t *testing.T) {
	f := migration000196Seeded(t)

	require.NoError(t, aprInsert(f.Tx, aprRequests, aprBreakGlass(f.User, nil)))

	agSavepoint(t, f.Tx, func() {
		err := aprInsert(f.Tx, aprRequests, aprBreakGlass(f.User, nil))
		migrationRequirePQError(t, err, "23505", "")
	})
	agSavepoint(t, f.Tx, func() {
		err := aprInsert(f.Tx, aprRequests, aprBreakGlass(f.Other, nil))
		assert.NoError(t, err, "the limit is per user, not global")
	})
	agSavepoint(t, f.Tx, func() {
		err := aprInsert(f.Tx, aprRequests, aprDisputed(f.User, o{"kind": "break_glass", "target_kind": nil, "target_id": nil, "target_material_version": nil}))
		assert.NoError(t, err, "a disputed break_glass is not pending")
	})
	agSavepoint(t, f.Tx, func() {
		err := aprInsert(f.Tx, aprRequests, aprRow(f.User, nil))
		assert.NoError(t, err, "a pending credential_remove is not a break_glass")
	})
	agSavepoint(t, f.Tx, func() {
		err := aprInsert(f.Tx, aprRequests, aprCompleted(f.User, o{"kind": "break_glass", "target_kind": nil, "target_id": nil, "target_material_version": nil}))
		assert.NoError(t, err, "a completed break_glass is not pending")
	})
}

// ---------------------------------------------------------------------------
// Keys other than CHECKs: primary keys, uniqueness, the identity column.
// ---------------------------------------------------------------------------

func TestMigration000196_KeysAndIdentity(t *testing.T) {
	f := migration000196Seeded(t)

	t.Run("hmac key_check is unique", func(t *testing.T) {
		agSavepoint(t, f.Tx, func() {
			err := aprInsert(f.Tx, aprHMACKeys, o{"key_check": aprFP(0x11)})
			migrationRequirePQError(t, err, "23505", "")
		})
	})
	t.Run("hmac id is generated always", func(t *testing.T) {
		agSavepoint(t, f.Tx, func() {
			err := aprInsert(f.Tx, aprHMACKeys, o{"id": 9999, "key_check": aprFP(0x33)})
			migrationRequirePQError(t, err, "428C9", "")
		})
	})
	t.Run("hmac ids are distinct and increasing", func(t *testing.T) {
		agSavepoint(t, f.Tx, func() {
			var second int16
			require.NoError(t, f.Tx.QueryRow(`INSERT INTO accountguard_hmac_keys (key_check) VALUES ($1) RETURNING id`, aprFP(0x44)).Scan(&second))
			assert.Greater(t, second, f.KeyID)
		})
	})
	t.Run("hmac created_at defaults to now", func(t *testing.T) {
		var set bool
		require.NoError(t, f.Tx.QueryRow(`SELECT created_at IS NOT NULL FROM accountguard_hmac_keys WHERE id = $1`, f.KeyID).Scan(&set))
		assert.True(t, set)
	})
	t.Run("reservation address is the primary key", func(t *testing.T) {
		row := aprReservation(f.User, f.Request, nil)
		require.NoError(t, aprInsert(f.Tx, aprReservations, row))
		agSavepoint(t, f.Tx, func() {
			err := aprInsert(f.Tx, aprReservations, aprReservation(f.Other, f.Request, o{"address": row["address"]}))
			migrationRequirePQError(t, err, "23505", "")
		})
	})
	t.Run("origin rows are keyed on all four columns", func(t *testing.T) {
		origin := o{"request_id": f.Request, "credential_kind": "totp", "credential_id": "cred-1", "material_version": int64(3)}
		require.NoError(t, aprInsert(f.Tx, aprOrigins, origin))
		agSavepoint(t, f.Tx, func() {
			migrationRequirePQError(t, aprInsert(f.Tx, aprOrigins, origin), "23505", "")
		})
		agSavepoint(t, f.Tx, func() {
			other := o{"request_id": f.Request, "credential_kind": "totp", "credential_id": "cred-1", "material_version": int64(4)}
			assert.NoError(t, aprInsert(f.Tx, aprOrigins, other), "a different material_version is a different origin")
		})
	})
	t.Run("origin rows need an existing request", func(t *testing.T) {
		agSavepoint(t, f.Tx, func() {
			err := aprInsert(f.Tx, aprOrigins, o{"request_id": uuid.New(), "credential_kind": "totp", "credential_id": "c", "material_version": int64(1)})
			migrationRequirePQError(t, err, "23503", "")
		})
	})
}

// ---------------------------------------------------------------------------
// The guarded down.
// ---------------------------------------------------------------------------

func TestMigration000196_DownRefusesOnLiveState(t *testing.T) {
	f := migration000196Seeded(t)
	down := migration000196DownSQL(t)
	u := f.User

	// Each refusing fixture holds exactly ONE guard condition on top of the
	// seeded completed request and HMAC key, which must never block by themselves.
	refuse := []struct {
		name string
		// want is a stable substring of the RAISE text for this one condition, so
		// a refusal that came from a different guard fails the case.
		want string
		seed func() error
	}{
		{"a pending request", "pending or disputed", func() error { return aprInsert(f.Tx, aprRequests, aprRow(u, nil)) }},
		{"a pending break_glass request", "pending or disputed", func() error { return aprInsert(f.Tx, aprRequests, aprBreakGlass(u, nil)) }},
		{"a disputed request", "pending or disputed", func() error { return aprInsert(f.Tx, aprRequests, aprDisputed(u, nil)) }},
		{"a proof cancel inside N_cancel_cp", "proof or dispute_resolved cancel", func() error {
			return aprInsert(f.Tx, aprRequests, aprCancelled(u, "proof", o{"cancelled_at": aprAgo(decide.NCancelCP - aprDay), "canceller_class": 1}))
		}},
		{"a dispute_resolved cancel inside N_cancel_cp", "proof or dispute_resolved cancel", func() error {
			return aprInsert(f.Tx, aprRequests, aprCancelled(u, "dispute_resolved", o{"cancelled_at": aprAgo(decide.NCancelCP - aprDay), "canceller_class": 1}))
		}},
		{"a password history row inside the retention window", "credential history row", func() error {
			return aprInsert(f.Tx, aprHistory, aprHistoryPassword(u, o{"retired_at": aprAgo(decide.HistoryRetention - aprDay)}))
		}},
		{"an address history row inside the retention window", "credential history row", func() error {
			return aprInsert(f.Tx, aprHistory, aprHistoryAddress(u, o{"retired_at": aprAgo(decide.HistoryRetention - aprDay)}))
		}},
		{"an address reservation", "address reservation", func() error {
			return aprInsert(f.Tx, aprReservations, aprReservation(u, f.Request, nil))
		}},
	}
	for _, tc := range refuse {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			agSavepoint(t, f.Tx, func() {
				require.NoError(t, tc.seed())

				_, spErr := f.Tx.Exec(`SAVEPOINT ag_down`)
				require.NoError(t, spErr)
				_, err := f.Tx.Exec(down)
				migrationRequirePQError(t, err, "P0001", "")
				var pqErr *pq.Error
				require.ErrorAs(t, err, &pqErr)
				assert.Contains(t, pqErr.Message, "cannot roll back 000196 while")
				assert.Contains(t, pqErr.Message, tc.want, "the refusal must name its own condition")

				// The refusal aborted the transaction; return to the point before the
				// down, then check the tables and the registered key stayed.
				_, rbErr := f.Tx.Exec(`ROLLBACK TO SAVEPOINT ag_down`)
				require.NoError(t, rbErr)
				aprRequireAll(t, f.Tx, true, "a refused down must leave the tables")
				assert.Equal(t, 1, aprCount(t, f.Tx, `SELECT count(*) FROM accountguard_hmac_keys WHERE id = $1`, f.KeyID))
			})
		})
	}

	// Aged-out and unrelated state must NOT block: the guard has to be precise,
	// or a rollback is impossible for 30 days after any use.
	allow := []struct {
		name string
		seed func() error
	}{
		{"only a completed request and an HMAC key", func() error { return nil }},
		{"a proof cancel past N_cancel_cp", func() error {
			return aprInsert(f.Tx, aprRequests, aprCancelled(u, "proof", o{"cancelled_at": aprAgo(decide.NCancelCP + aprDay), "canceller_class": 1}))
		}},
		{"a dispute_resolved cancel past N_cancel_cp", func() error {
			return aprInsert(f.Tx, aprRequests, aprCancelled(u, "dispute_resolved", o{"cancelled_at": aprAgo(decide.NCancelCP + aprDay), "canceller_class": 1}))
		}},
		{"a password history row past the retention window", func() error {
			return aprInsert(f.Tx, aprHistory, aprHistoryPassword(u, o{"retired_at": aprAgo(decide.HistoryRetention + aprDay)}))
		}},
		{"an address history row past the retention window", func() error {
			return aprInsert(f.Tx, aprHistory, aprHistoryAddress(u, o{"retired_at": aprAgo(decide.HistoryRetention + aprDay)}))
		}},
		{"a recent cancel for a reason other than proof or dispute_resolved", func() error {
			return aprInsert(f.Tx, aprRequests, aprCancelled(u, "superseded", o{"cancelled_at": aprAgo(aprDay)}))
		}},
	}
	for _, tc := range allow {
		t.Run("allows "+tc.name, func(t *testing.T) {
			agSavepoint(t, f.Tx, func() {
				require.NoError(t, tc.seed())
				_, err := f.Tx.Exec(down)
				require.NoError(t, err, "the down must run when nothing live remains")
				aprRequireAll(t, f.Tx, false, "a permitted down must drop every table")
			})
		})
	}
}

// ---------------------------------------------------------------------------
// ON DELETE CASCADE from users.
// ---------------------------------------------------------------------------

func TestMigration000196_UserDeleteCascades(t *testing.T) {
	f := migration000196Seeded(t)

	// Other owns a row in every user-scoped table; User keeps only what the
	// fixture gave it, so a delete that over-reaches shows up as a missing row.
	pending := uuid.New()
	require.NoError(t, aprInsert(f.Tx, aprRequests, aprRow(f.Other, o{"id": pending})))
	require.NoError(t, aprInsert(f.Tx, aprOrigins, o{"request_id": pending, "credential_kind": "totp", "credential_id": "cred-1", "material_version": int64(3)}))
	require.NoError(t, aprInsert(f.Tx, aprHistory, aprHistoryPassword(f.Other, nil)))
	require.NoError(t, aprInsert(f.Tx, aprReservations, aprReservation(f.Other, pending, nil)))

	counts := func(user, request uuid.UUID) map[string]int {
		return map[string]int{
			aprRequests:     aprCount(t, f.Tx, `SELECT count(*) FROM account_pending_requests WHERE user_id = $1`, user),
			aprOrigins:      aprCount(t, f.Tx, `SELECT count(*) FROM account_pending_request_origins WHERE request_id = $1`, request),
			aprHistory:      aprCount(t, f.Tx, `SELECT count(*) FROM user_credential_history WHERE user_id = $1`, user),
			aprReservations: aprCount(t, f.Tx, `SELECT count(*) FROM account_address_reservations WHERE user_id = $1`, user),
		}
	}
	require.Equal(t, map[string]int{aprRequests: 1, aprOrigins: 1, aprHistory: 1, aprReservations: 1}, counts(f.Other, pending),
		"the fixture must populate every table before the delete")

	_, err := f.Tx.Exec(`DELETE FROM users WHERE id = $1`, f.Other)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{aprRequests: 0, aprOrigins: 0, aprHistory: 0, aprReservations: 0}, counts(f.Other, pending),
		"deleting the user must remove its rows in all four user tables")
	assert.Equal(t, 1, aprCount(t, f.Tx, `SELECT count(*) FROM account_pending_requests WHERE user_id = $1`, f.User),
		"another user's request must survive")
	assert.Equal(t, 1, aprCount(t, f.Tx, `SELECT count(*) FROM accountguard_hmac_keys WHERE id = $1`, f.KeyID),
		"the key registry is not user data and must survive")
}

// ---------------------------------------------------------------------------
// The down locks every table BEFORE it checks.
// ---------------------------------------------------------------------------

func TestMigration000196_DownChecksOnlyAfterTakingItsLocks(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	down := migration000196DownSQL(t)

	// A committed user and completed request that go away with the user. The
	// completed request blocks nothing, and gives the reservation case a real
	// request to point at.
	user, request := uuid.New(), uuid.New()
	require.NoError(t, migration000189InsertUser(db, user))
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM users WHERE id = $1`, user)
		assert.NoError(t, err)
	})
	require.NoError(t, aprInsert(db, aprRequests, aprCompleted(user, o{"id": request})))

	// One case per table the down checks, each parking the down on THAT table's
	// lock, so a check moved ahead of any single lock is caught.
	cases := []struct {
		name, table, want string
		blocker           func(tx *sql.Tx) error
	}{
		{"a pending request", aprRequests, "pending or disputed", func(tx *sql.Tx) error {
			return aprInsert(tx, aprRequests, aprRow(user, nil))
		}},
		{"a recent credential history row", aprHistory, "credential history row", func(tx *sql.Tx) error {
			return aprInsert(tx, aprHistory, aprHistoryPassword(user, nil))
		}},
		{"an address reservation", aprReservations, "address reservation", func(tx *sql.Tx) error {
			return aprInsert(tx, aprReservations, aprReservation(user, request, nil))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The blocker COMMITS, so its row would trip the next case's down on
			// an earlier guard. Clear everything but the completed request.
			t.Cleanup(func() {
				for _, stmt := range []string{
					`DELETE FROM account_address_reservations WHERE user_id = $1`,
					`DELETE FROM user_credential_history WHERE user_id = $1`,
				} {
					_, err := db.Exec(stmt, user)
					assert.NoError(t, err)
				}
				_, err := db.Exec(`DELETE FROM account_pending_requests WHERE user_id = $1 AND id <> $2`, user, request)
				assert.NoError(t, err)
			})
			migrationRequireGuardRunsUnderLockCode(t, db, down, tc.table, "P0001", tc.want, tc.blocker)
		})
	}
}

// ---------------------------------------------------------------------------
// RT-7: the address CHECKs refuse any whitespace character anywhere (CWE-1289)
// ---------------------------------------------------------------------------

// aprSpace is a named whitespace character.
type aprSpace struct {
	name string
	r    rune
}

// aprWhitespace is every character a stored address must not contain. The migration's CHECKs
// cover [[:space:]] plus an explicit list of the Unicode spaces the class may miss, and the
// application's normalizer (strings.TrimSpace) folds the edge ones, so a stored address is
// already trimmed and an email has no interior space. The runes are built with rune(), never
// escaped in source.
func aprWhitespace() []aprSpace {
	chars := []aprSpace{
		{"SPACE", ' '}, {"TAB", '\t'}, {"LF", '\n'}, {"VT", 0x0B}, {"FF", 0x0C}, {"CR", '\r'},
		{"NEL U+0085", 0x0085}, {"NBSP U+00A0", 0x00A0}, {"OGHAM SPACE MARK U+1680", 0x1680},
		{"LINE SEPARATOR U+2028", 0x2028}, {"PARAGRAPH SEPARATOR U+2029", 0x2029},
		{"NNBSP U+202F", 0x202F}, {"MMSP U+205F", 0x205F}, {"IDEOGRAPHIC SPACE U+3000", 0x3000},
		{"ZWNBSP U+FEFF", 0xFEFF},
	}
	// U+2000 EN QUAD through U+200B ZERO WIDTH SPACE.
	for r := rune(0x2000); r <= 0x200B; r++ {
		chars = append(chars, aprSpace{"U+" + strings.ToUpper(strconv.FormatInt(int64(r), 16)), r})
	}
	return chars
}

func TestMigration000196_AddressChecksRefuseAnyWhitespace(t *testing.T) {
	f := migration000196Seeded(t)
	u := f.User

	tables := []struct {
		name       string
		table      string
		constraint string
		row        func(address string) map[string]any
	}{
		{"account_address_reservations", aprReservations, "aar_address_normalized",
			func(a string) map[string]any { return aprReservation(u, f.Request, o{"address": a}) }},
		{"user_credential_history", aprHistory, "uch_address_normalized",
			func(a string) map[string]any { return aprHistoryAddress(u, o{"address": a}) }},
		{"account_pending_requests", aprRequests, "apr_pending_address_normalized",
			func(a string) map[string]any { return aprRow(u, o{"pending_address": a}) }},
	}

	for _, tb := range tables {
		t.Run(tb.name, func(t *testing.T) {
			// Clean spellings are admitted, so the refusals below are the whitespace and not the row.
			for _, clean := range []string{"norm@example.test", "tag+x.y@example.test", "josé@exämple.test"} {
				agSavepoint(t, f.Tx, func() {
					assert.NoError(t, aprInsert(f.Tx, tb.table, tb.row(clean)), "the clean spelling %q is admitted", clean)
				})
			}
			for _, ws := range aprWhitespace() {
				c := string(ws.r)
				for _, pos := range []struct{ name, address string }{
					{"leading", c + "norm@example.test"},
					{"trailing", "norm@example.test" + c},
					{"interior", "no" + c + "rm@example.test"},
				} {
					t.Run(ws.name+" "+pos.name, func(t *testing.T) {
						agSavepoint(t, f.Tx, func() {
							err := aprInsert(f.Tx, tb.table, tb.row(pos.address))
							migrationRequirePQError(t, err, "23514", tb.constraint)
						})
					})
				}
			}
		})
	}
}
