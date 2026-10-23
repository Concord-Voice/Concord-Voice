package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/server"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// dirtySetup is authoritydb.DirtySetup under authoritydb.Setup's advisory
// lock as well. The store's tests GRANT and REVOKE authority_owner on
// authority_app, which is cluster-wide, holding Setup's lock alone (ruling 6),
// and CI runs packages two at a time; holding that lock here keeps this
// package's tests out of that window, so none can see the app role with owner
// membership, today or after a test here connects as authority_app. Setup's
// lock is taken before DirtySetup's, the order any test holding both must use.
func dirtySetup(t *testing.T) (owner, super *sql.DB) {
	t.Helper()
	authoritydb.Setup(t)
	return authoritydb.DirtySetup(t)
}

// B2-D2: migrate builds the schema, writes meta once, refuses another realm,
// and refuses while the serve lock is held (D-171).
func TestMigrateDB(t *testing.T) {
	owner, super := dirtySetup(t)
	ctx := context.Background()
	realm := uuid.NewString()
	if err := migrateDB(ctx, owner, realm); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := store.CheckSchema(ctx, owner); err != nil {
		t.Fatalf("schema after migrate: %v", err)
	}
	var got string
	if err := owner.QueryRow(`SELECT realm_id FROM authority.meta WHERE id = 1`).Scan(&got); err != nil || got != realm {
		t.Fatalf("meta realm %q, %v", got, err)
	}
	if err := migrateDB(ctx, owner, realm); err != nil {
		t.Fatalf("second migrate, same realm: %v", err)
	}
	if err := migrateDB(ctx, owner, uuid.NewString()); reason(err) != "trust" {
		t.Fatalf("another realm: %v, want reason trust", err)
	}
	lock, err := store.AcquireServeLock(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := migrateDB(ctx, owner, realm); reason(err) != "lock" {
		t.Fatalf("lock held: %v, want reason lock", err)
	}
}

// returnsWithin runs f and reports whether it returned within wait, and with
// what.
func returnsWithin(wait time.Duration, f func() error) (returned bool, err error) {
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return true, err
	case <-time.After(wait):
		return false, nil
	}
}

// shortDeadline makes the per-step deadline 300 ms for one test.
func shortDeadline(t *testing.T) {
	t.Helper()
	old := store.RequestDeadlineTx
	store.RequestDeadlineTx = 300 * time.Millisecond
	t.Cleanup(func() { store.RequestDeadlineTx = old })
}

// shortOwnerDeadline makes the owner-verb deadline 300 ms for one test.
func shortOwnerDeadline(t *testing.T) {
	t.Helper()
	old := ownerDeadline
	ownerDeadline = 300 * time.Millisecond
	t.Cleanup(func() { ownerDeadline = old })
}

// Carry 4 (M2), the lock step: with every connection of the pool held,
// AcquireServeLock waits for one. The step's own deadline ends the wait, and
// an expired deadline keeps the step's reason, config (the lock is not
// held; it was never asked about). The caller's context outlasts the
// window, so a step with no deadline of its own is still waiting at 2 s.
func TestMigrateDBBoundsTheLockStep(t *testing.T) {
	owner, _ := dirtySetup(t)
	shortDeadline(t)
	owner.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	held, err := owner.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	returned, err := returnsWithin(2*time.Second, func() error { return migrateDB(ctx, owner, uuid.NewString()) })
	if !returned {
		t.Fatal("migrateDB is still waiting for a connection after 2 s: the lock step has no deadline of its own")
	}
	if reason(err) != "config" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a lock step that timed out: %v (reason %q), want reason config over the deadline", err, reason(err))
	}
}

// Carry 4 (M2), the meta step: a transaction holding authority.meta
// ACCESS EXCLUSIVE blocks EnsureMeta's insert. The step's own deadline ends
// the wait with reason=config. The migration before it, which takes no
// context and which no deadline bounds, has already run: this is the
// already-migrated second pass, which finds nothing to apply.
func TestMigrateDBBoundsTheMetaStep(t *testing.T) {
	owner, super := dirtySetup(t)
	realm := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := migrateDB(ctx, owner, realm); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	tx, err := super.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, `LOCK TABLE authority.meta IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	shortDeadline(t)
	returned, err := returnsWithin(2*time.Second, func() error { return migrateDB(ctx, owner, realm) })
	if !returned {
		t.Fatal("migrateDB is still waiting on authority.meta after 2 s: the meta step has no deadline of its own")
	}
	if reason(err) != "config" {
		t.Fatalf("a meta step that timed out: %v (reason %q), want config", err, reason(err))
	}
}

// Carry C1 of the Task 11 review, against the real driver: an object the first
// migration creates already exists, so golang-migrate fails with a
// database.Error whose text carries the whole migration's SQL. The exit line
// carries the cause's message and nothing of the SQL. (A dropped connection
// mid-migration, the OpError shape, cannot be forced from a test; the shape
// rows in TestExitTextOfMigrationErrors stand in for it.)
func TestMigrateDBFailureCarriesNoSQL(t *testing.T) {
	owner, _ := dirtySetup(t)
	if _, err := owner.Exec(`CREATE TABLE authority.meta (id smallint)`); err != nil {
		t.Fatal(err)
	}
	err := migrateDB(context.Background(), owner, uuid.NewString())
	if reason(err) != "config" {
		t.Fatalf("a migration that fails: %v (reason %q), want config", err, reason(err))
	}
	for _, s := range []string{"CREATE TABLE authority.meta (", "Account authority, migration 000001"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("control: the raw error does not carry %q, so this test cannot see the SQL leak", s)
		}
	}
	var buf bytes.Buffer
	exitCode(logger.NewJSON(&buf).Logger, err)
	m := exitLine(t, 1, buf.String(), "config")
	if got := errorOf(t, m); got != `database: pq: relation "meta" already exists` {
		t.Fatalf("exit line error %q", got)
	}
	if strings.Contains(buf.String(), "CREATE") || strings.Contains(buf.String(), "Account authority") {
		t.Fatalf("the exit line carries the migration's SQL: %.200q", buf.String())
	}
}

// ---- Task 11: the owner verbs against the database ----
//
// SEAM: these tests replace the package variable openOwner, which main must
// declare as `var openOwner = server.OpenOwner` (same signature) and which
// every owner verb must open its store through. The shared test database is a
// TCP superuser URL, which server.Load's authority_owner-over-socket rule
// refuses, so the real OpenOwner cannot be reached from here.

type okProbe struct{ ok bool }

func (p okProbe) Replicated(context.Context, *sql.Conn, string) (bool, error) { return p.ok, nil }

type ownerRig struct {
	f      *authoritydb.Fixture
	rl     *authoritytest.Realm
	db     *sql.DB
	signer []bool // needSigner, per open
	dir    string
}

func newOwnerRig(t *testing.T, cfg store.OwnerConfig) *ownerRig {
	t.Helper()
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	r := &ownerRig{f: f, rl: rl, db: f.Owner, dir: ownerEnv(t)}
	old := openOwner
	t.Cleanup(func() { openOwner = old })
	openOwner = func(_ server.Config, need bool, log *slog.Logger) (*store.Owner, func(), error) {
		r.signer = append(r.signer, need)
		c := cfg
		c.DB, c.Anchors, c.Logger = r.db, rl.AnchorBytes, log
		if need {
			c.Signer = rl.RSK
		}
		o, err := store.OpenOwner(c)
		return o, func() {}, err
	}
	return r
}

func okRig(t *testing.T) *ownerRig { return newOwnerRig(t, store.OwnerConfig{Probe: okProbe{true}}) }

var kvLine = regexp.MustCompile(`^[a-z_]+=\S+( [a-z_]+=\S+)*$`)

// outLines is stdout as key=value lines (B3-D9); a JSON log line or any other
// shape fails the test.
func outLines(t *testing.T, stdout string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if l == "" {
			continue
		}
		if !kvLine.MatchString(l) {
			t.Fatalf("stdout line %q is not key=value (a success writes no log line, B3-D9)", l)
		}
		out = append(out, l)
	}
	return out
}

func okRun(t *testing.T, args ...string) []string {
	t.Helper()
	code, stdout, stderr := run(args...)
	if code != 0 || stderr != "" {
		t.Fatalf("%q: exit %d, stdout %q, stderr %q, want 0", args, code, stdout, stderr)
	}
	return outLines(t, stdout)
}

func refusedRun(t *testing.T, args ...string) {
	t.Helper()
	code, stdout, _ := run(args...)
	m := exitLine(t, code, stdout, "refused")
	if m["step"] != server.StepOwner {
		t.Fatalf("line %v, want step owner", m)
	}
}

func TestOwnerVerbsEndToEnd(t *testing.T) {
	r := okRig(t)
	var gate bool
	read := func(q string, dst any) {
		t.Helper()
		if err := r.f.Super.QueryRow(q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("replication-gate", func(t *testing.T) {
		for _, c := range []struct {
			arg  string
			want bool
		}{{"off", false}, {"on", true}, {"on", true}, {"off", false}} { // a re-run at the same value is exit 0 (B3-D8)
			okRun(t, "replication-gate", c.arg)
			read(`SELECT replication_required FROM authority.meta WHERE id = 1`, &gate)
			if gate != c.want {
				t.Fatalf("after replication-gate %s: replication_required = %v", c.arg, gate)
			}
		}
	})

	t.Run("revoke-key", func(t *testing.T) {
		pin := hex.EncodeToString(r.rl.Current.D.CPTLSPins[0])
		var n int
		okRun(t, "revoke-key", "cp_tls", pin)
		okRun(t, "revoke-key", "cp_tls", pin) // an existing row is exit 0
		read(`SELECT count(*) FROM authority.revoked_keys`, &n)
		if n != 1 {
			t.Fatalf("revoked_keys has %d rows after two runs, want 1", n)
		}
		refusedRun(t, "revoke-key", "cak", strings.Repeat("ab", 48)) // an unknown digest (B3-D10)
	})

	// Before end-drain: its renewal copies serial 1, RSK included
	// (NextDelegation), so after it the realm's RSK is listed in two
	// delegations and clear-held's D-223 check refuses to sign (ErrRSKReused),
	// as serve would refuse to start. A second rig cannot stand in, since
	// authoritydb.Setup holds the database's advisory lock for the whole test.
	t.Run("clear-held signs", func(t *testing.T) {
		before := len(r.signer)
		okRun(t, "clear-held", uuid.NewString())
		if got := r.signer[before:]; len(got) != 1 || !got[0] {
			t.Fatalf("clear-held opened the owner with needSigner %v, want [true]", got)
		}
	})

	t.Run("end-drain", func(t *testing.T) {
		r.rl.Install(t, r.f.App, r.rl.NextDelegation(t)) // serial 1 is now superseded
		okRun(t, "end-drain", "0:1")
		okRun(t, "end-drain", "0:1") // grace already 0 is exit 0
		refusedRun(t, "end-drain", "0:99")
	})

	t.Run("seal-legacy", func(t *testing.T) {
		recs := []ac.PopulationRecord{
			{AccountID: "00000000-0000-4000-8000-000000000001", LegacyEKDigest: bytes.Repeat([]byte{1}, 48)},
			{AccountID: "00000000-0000-4000-8000-000000000002", LegacyEKDigest: bytes.Repeat([]byte{2}, 48)},
		}
		for _, rec := range recs {
			if _, err := r.f.Owner.Exec(`INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`,
				rec.AccountID, rec.LegacyEKDigest); err != nil {
				t.Fatal(err)
			}
		}
		d, err := ac.PopulationDigest(recs)
		if err != nil {
			t.Fatal(err)
		}
		okRun(t, "seal-legacy", "--cp-digest", hex.EncodeToString(d))
		okRun(t, "seal-legacy", "--cp-digest", hex.EncodeToString(d)) // same digest: exit 0
		refusedRun(t, "seal-legacy", "--cp-digest", strings.Repeat("ee", 48))
	})

	t.Run("clear-held --dry-run", func(t *testing.T) {
		a := uuid.NewString()
		before := len(r.signer)
		lines := okRun(t, "clear-held", "--dry-run", a)
		if want := []string{"account=" + a + " outcome=" + store.ClearNotHeld}; !slices.Equal(lines, want) {
			t.Fatalf("stdout %q, want %q", lines, want)
		}
		if got := r.signer[before:]; len(got) != 1 || got[0] {
			t.Fatalf("a dry run opened the owner with needSigner %v, want [false]", got)
		}
	})

	t.Run("reconcile-complete", func(t *testing.T) {
		if _, err := r.f.Owner.Exec(`SELECT authority.enter_unreconciled()`); err != nil {
			t.Fatal(err)
		}
		pair := regexp.MustCompile(`^imported=\d+ held=\d+$`)
		first := okRun(t, "reconcile-complete")
		if len(first) != 1 || !pair.MatchString(first[0]) {
			t.Fatalf("stdout %q, want one `imported=N held=M` line", first)
		}
		if second := okRun(t, "reconcile-complete"); !slices.Equal(first, second) { // already reconciled: no change
			t.Fatalf("a re-run printed %q, first printed %q", second, first)
		}
	})

}

func exitFields(t *testing.T, stdout, wantReason string) map[string]any {
	t.Helper()
	m := exitLine(t, 1, stdout, wantReason)
	if got, want := slices.Sorted(maps.Keys(m)), []string{"error", "level", "msg", "op", "reason", "step", "time"}; !slices.Equal(got, want) {
		t.Fatalf("exit line fields %v, want %v", got, want)
	}
	if m["step"] != server.StepOwner || m["op"] != "authority.exit" {
		t.Fatalf("line %v, want step owner", m)
	}
	return m
}

// replicaExec runs q in a transaction under the replica role, which skips the
// schema's row guards, as the store package's own fixtures rely on.
func replicaExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerVerbExitLines(t *testing.T) {
	long := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	cases := []struct {
		name       string
		cfg        store.OwnerConfig
		args       []string
		setup      func(t *testing.T, r *ownerRig)
		reason     string
		wantErrTxt string
	}{
		{name: "ErrServeLockHeld", cfg: store.OwnerConfig{Probe: okProbe{true}}, args: []string{"reconcile-complete"}, reason: "lock",
			setup: func(t *testing.T, r *ownerRig) {
				l, err := store.AcquireServeLock(context.Background(), r.f.Super)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(l.Release)
			}},
		{name: "ErrNotLeader", args: []string{"replication-gate", "on"}, reason: "not_leader",
			cfg: store.OwnerConfig{InRecovery: func(context.Context, *sql.Conn) (bool, error) { return true, nil }}},
		{name: "ErrNotReplicated", cfg: store.OwnerConfig{Probe: okProbe{false}}, args: []string{"replication-gate", "on"},
			reason: "replication", wantErrTxt: "committed locally, not replicated, re-run",
			setup: func(t *testing.T, r *ownerRig) {
				shortOwnerDeadline(t)
				if _, err := r.f.Owner.Exec(`UPDATE authority.meta SET replication_required = true`); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "ErrRefused", cfg: store.OwnerConfig{Probe: okProbe{true}}, reason: "refused",
			args: []string{"revoke-key", "cak", strings.Repeat("ab", 48)}},
		{name: "ErrTrustUnusable (hand-off)", cfg: store.OwnerConfig{Probe: okProbe{true}}, args: []string{"restore-begin"}, reason: "refused",
			setup: func(t *testing.T, r *ownerRig) {
				if err := os.WriteFile(filepath.Join(r.dir, "trust"), []byte("not a trust file"), 0o600); err != nil { // ownerEnv's AUTHORITY_TRUST_PATH
					t.Fatal(err)
				}
			}},
		// Codex round 2, G4: the owner connection checks the realm for every verb.
		{name: "ErrRealmMismatch", cfg: store.OwnerConfig{Probe: okProbe{true}}, args: []string{"replication-gate", "on"}, reason: "trust",
			setup: func(t *testing.T, r *ownerRig) {
				replicaExec(t, r.f.Super, `UPDATE authority.meta SET realm_id = $1`, uuid.NewString())
			}},
		{name: "ErrNoMeta", cfg: store.OwnerConfig{Probe: okProbe{true}}, args: []string{"reconcile-complete"}, reason: "config",
			setup: func(t *testing.T, r *ownerRig) {
				replicaExec(t, r.f.Super, `DELETE FROM authority.meta`) // meta_never_deleted is a row trigger: replica skips it
			}},
		{name: "anything else", args: []string{"replication-gate", "on"}, reason: "config",
			cfg: store.OwnerConfig{InRecovery: func(context.Context, *sql.Conn) (bool, error) { return false, errors.New("boom") }}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newOwnerRig(t, c.cfg)
			if c.setup != nil {
				c.setup(t, r)
			}
			var o, e bytes.Buffer
			code := runMain(long(t), c.args, &o, &e)
			if e.Len() != 0 {
				t.Fatalf("stderr %q", e.String())
			}
			if o.Len() == 0 || code != 1 {
				t.Fatalf("exit %d, stdout %q", code, o.String())
			}
			m := exitFields(t, o.String(), c.reason)
			if c.wantErrTxt != "" && errorOf(t, m) != c.wantErrTxt {
				t.Fatalf("error %q, want %q (DoR:1743)", errorOf(t, m), c.wantErrTxt)
			}
		})
	}
}

// Hand-off: every owner verb is bounded. The store call blocks (the pool's one
// connection is held), so only the verb's own deadline can end it. The test
// shortens ownerDeadline alone, leaving the 8 s request deadline in place, so
// a verb bounded by server.Within (the request deadline) instead is still
// waiting at 3 s; so is a verb with no bound at all.
func TestOwnerVerbIsBoundedByADeadline(t *testing.T) {
	r := okRig(t)
	shortOwnerDeadline(t)
	one := authoritydb.OpenAs(t, authoritydb.URL(t, authoritydb.EnvURL, authoritydb.DBName), authoritydb.RoleOwner)
	one.SetMaxOpenConns(1)
	r.db = one
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	held, err := one.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	var o, e bytes.Buffer
	returned, code := false, 0
	done := make(chan int, 1)
	go func() { done <- runMain(ctx, []string{"replication-gate", "on"}, &o, &e) }()
	select {
	case code = <-done:
		returned = true
	case <-time.After(3 * time.Second):
	}
	if !returned {
		t.Fatal("the verb is still waiting for a connection after 3 s: it has no deadline of its own")
	}
	m := exitFields(t, o.String(), "config")
	if code != 1 || !strings.Contains(errorOf(t, m), "deadline") {
		t.Fatalf("exit %d, error %q, want 1 over a deadline error", code, errorOf(t, m))
	}
}

// restore-begin's success path: the verb reads AUTHORITY_TRUST_PATH itself,
// installs a trust file that verifies, and prints the point it recorded; a
// re-run while the restore is open prints the same point (B3-D8).
func TestOwnerRestoreBeginWithAValidTrustFile(t *testing.T) {
	r := okRig(t)
	file := ac.TrustEnvelope{Items: [][]byte{r.rl.Current.Signed}}.Encode()
	if file == nil {
		t.Fatal("the trust file does not encode")
	}
	if err := os.WriteFile(filepath.Join(r.dir, "trust"), file, 0o600); err != nil { // ownerEnv's AUTHORITY_TRUST_PATH
		t.Fatal(err)
	}
	point := regexp.MustCompile(`^restore_epoch=(\d+) backup_max_commit_id=\d+ backup_max_issued_at_ms=\d+ restore_started_at=\S+$`)
	first := okRun(t, "restore-begin")
	if len(first) != 1 || !point.MatchString(first[0]) {
		t.Fatalf("stdout %q, want one restore-point line", first)
	}
	var epoch int64
	var reconciled bool
	if err := r.f.Super.QueryRow(`SELECT restore_epoch, reconciled FROM authority.meta WHERE id = 1`).Scan(&epoch, &reconciled); err != nil {
		t.Fatal(err)
	}
	if got := point.FindStringSubmatch(first[0])[1]; got != strconv.FormatInt(epoch, 10) || epoch < 1 || reconciled {
		t.Fatalf("printed epoch %s; meta restore_epoch %d, reconciled %v; want the printed epoch, ≥ 1, unreconciled", got, epoch, reconciled)
	}
	if second := okRun(t, "restore-begin"); !slices.Equal(first, second) {
		t.Fatalf("a re-run printed %q, the first run %q", second, first)
	}
}
