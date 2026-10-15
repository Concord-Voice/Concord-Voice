package store

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// D-147 end to end (review round 2): a protective cancel refused at the hard
// disk floor sets the leg, and the next tick that commits adds the full tick.
func TestTickPersistsTheLegsAndConsumesTheNon503Flag(t *testing.T) {
	e := newEnv(t)
	mustExec(t, e.f.App, `UPDATE authority.meta SET last_tick_at = to_timestamp($1 / 1000.0)`, e.now.Load())
	a, _ := e.enrolledWith()
	e.s.hooks.Statfs = func() (uint64, error) { return DiskHardFloorBytes - 1, nil }
	e.errorAs(a.CancelByRoot(t).Bytes, 503, ac.ErrorDisk)
	e.s.hooks.Statfs = func() (uint64, error) { return 1 << 40, nil }
	e.advance(30_000)
	if err := e.s.Tick(context.Background(), TickInput{LastPeerContactMs: e.now.Load()}); err != nil {
		t.Fatal(err)
	}
	var acc int64
	_ = e.f.App.QueryRow(`SELECT cancel_unavailable_ms FROM authority.meta`).Scan(&acc)
	if acc != 30_000 || e.s.nonClient503.Load() {
		t.Fatalf("accumulator %d (want 30000), flag still set %v", acc, e.s.nonClient503.Load())
	}
}

// D-147: a tick that fails to commit keeps the non-client 503 leg, so the next
// tick that commits still adds the full tick.
func TestTickKeepsThe503LegWhenItFailsToCommit(t *testing.T) {
	e := newEnv(t)
	mustExec(t, e.f.App, `UPDATE authority.meta SET last_tick_at = to_timestamp($1 / 1000.0)`, e.now.Load())
	mustExec(t, e.f.Super, `CREATE FUNCTION authority.test_tick_fail() RETURNS trigger LANGUAGE plpgsql AS
		$$BEGIN RAISE EXCEPTION 'injected tick failure'; END$$`)
	mustExec(t, e.f.Super, `CREATE TRIGGER test_tick_fail BEFORE UPDATE ON authority.meta
		FOR EACH ROW EXECUTE FUNCTION authority.test_tick_fail()`)
	t.Cleanup(func() {
		_, _ = e.f.Super.Exec(`DROP TRIGGER IF EXISTS test_tick_fail ON authority.meta`)
		_, _ = e.f.Super.Exec(`DROP FUNCTION IF EXISTS authority.test_tick_fail()`)
	})
	e.s.nonClient503.Store(true)
	e.advance(30_000)
	if err := e.s.Tick(context.Background(), TickInput{LastPeerContactMs: e.now.Load()}); err == nil {
		t.Fatal("the injected failure did not reach Tick")
	}
	if !e.s.nonClient503.Load() {
		t.Fatal("a tick that did not commit consumed the non-client 503 leg")
	}
	mustExec(t, e.f.Super, `DROP TRIGGER test_tick_fail ON authority.meta`)
	if err := e.s.Tick(context.Background(), TickInput{LastPeerContactMs: e.now.Load()}); err != nil {
		t.Fatal(err)
	}
	var acc int64
	_ = e.f.App.QueryRow(`SELECT cancel_unavailable_ms FROM authority.meta`).Scan(&acc)
	if acc != 30_000 || e.s.nonClient503.Load() {
		t.Fatalf("accumulator %d (want 30000), flag still set %v", acc, e.s.nonClient503.Load())
	}
}

// The bookkeeping update is bounded: with meta held elsewhere, Purge fails after
// purgeBookkeeping instead of hanging (and holding B2's one-purge guard).
func TestPurgeBookkeepingIsBounded(t *testing.T) {
	e := newEnv(t)
	blocker, err := e.f.App.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.Exec(`SELECT 1 FROM authority.meta WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := e.s.Purge(context.Background()); err == nil {
		t.Fatal("Purge finished while meta was locked elsewhere")
	}
	if d := time.Since(start); d > purgeBookkeeping+2*time.Second {
		t.Fatalf("Purge took %v with meta locked, want about %v", d, purgeBookkeeping)
	}
}

// §3.2.4: last_purge_at is set after a short batch or when the run's own budget
// ends, so a backlog that outlasts the 20-s budget waits a day instead of
// restarting on every tick (review round 2). A caller's deadline is not the
// budget: it leaves last_purge_at alone, so the purge stays due (review round 3).
func TestPurgeRecordsARunThatEndsOnItsBudget(t *testing.T) {
	e := newEnv(t)
	lastPurgeMs := func() (int64, error) {
		var ms int64
		err := e.f.App.QueryRow(`SELECT floor(extract(epoch FROM last_purge_at) * 1000)::bigint FROM authority.meta`).Scan(&ms)
		return ms, err
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	if _, err := e.s.Purge(ctx); err == nil {
		t.Fatal("Purge under an expired caller deadline returned no error")
	}
	if ms, err := lastPurgeMs(); err == nil {
		t.Fatalf("a caller deadline recorded last_purge_at = %d ms; the purge must stay due", ms)
	}
	e.s.purgeBudget = 0 // the run's own budget is spent before the first batch, deterministically
	rep, err := e.s.Purge(context.Background())
	if err != nil || rep.Complete {
		t.Fatalf("Purge on a spent budget = %+v, %v; want an incomplete run and no error", rep, err)
	}
	if ms, err := lastPurgeMs(); err != nil || ms != i64(e.now.Load()) {
		t.Fatalf("last_purge_at = %d ms (%v), want db_now %d after the budget ended", ms, err, e.now.Load())
	}
}

// Review round 3: the tick bounds D-241's map.
func TestTickPrunesTheAsOfFence(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	readHead(t, e, a.ID)
	e.advance(asOfKeepMs + 1)
	if err := e.s.Tick(context.Background(), TickInput{LastPeerContactMs: e.now.Load()}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.s.asOfLast); n != 0 {
		t.Fatalf("a tick asOfKeepMs after the read left %d entries in the fence", n)
	}
}

// B1-D3: the tick's transaction takes the store's limits too (review round 2).
// With meta locked elsewhere it gives up at the 2-s lock timeout, not at its
// caller's deadline.
func TestTickGivesUpAtTheLockTimeout(t *testing.T) {
	e := newEnv(t)
	beginHolding(t, e.f.App, `SELECT 1 FROM authority.meta WHERE id = 1 FOR UPDATE`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := e.s.Tick(ctx, TickInput{LastPeerContactMs: e.now.Load()})
	if d := time.Since(start); err == nil || d > 4*time.Second {
		t.Fatalf("Tick with meta locked elsewhere = %v after %v, want the 2-s lock timeout", err, d)
	}
}

// B1-D3 on the purge calls: purge_erased blocked on L1 fails at the 2-s lock
// timeout, not at the 20-s budget, and leaves the purge due.
func TestPurgeGivesUpAtTheLockTimeout(t *testing.T) {
	e := newEnv(t)
	a := eraseAndAge(t, e, 31)
	beginHolding(t, e.f.App, `SELECT 1 FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`, a.ID)
	start := time.Now()
	_, err := e.s.Purge(context.Background())
	if d := time.Since(start); err == nil || d > 4*time.Second {
		t.Fatalf("Purge with L1 held elsewhere = %v after %v, want the 2-s lock timeout", err, d)
	}
	var due bool
	if err := e.f.Super.QueryRow(`SELECT last_purge_at IS NULL FROM authority.meta WHERE id = 1`).Scan(&due); err != nil || !due {
		t.Fatalf("last_purge_at after a timed-out purge: due=%v err=%v, want still due", due, err)
	}
}

// eraseAndAge commits an operator erase (row 19) and backdates erased_at.
func eraseAndAge(t *testing.T, e *env, days int) *authoritytest.Account {
	t.Helper()
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Erase(t, ac.ActorOperator).Bytes)
	mustExec(t, e.f.Super, `UPDATE authority.account_heads SET erased_at = now() - make_interval(days => $2) WHERE account_id = $1`, a.ID, days)
	return a
}

func TestPurgeDeletesAnErasedAccountsEventsAndKeepsTheTombstone(t *testing.T) {
	e := newEnv(t)
	a := eraseAndAge(t, e, 31)
	var bindings int
	_ = e.f.Super.QueryRow(`SELECT count(*) FROM authority.key_bindings WHERE account_id = $1`, a.ID).Scan(&bindings)
	rep, err := e.s.Purge(context.Background())
	if err != nil || rep.Purged != 1 || !rep.Complete {
		t.Fatalf("Purge = %+v, %v", rep, err)
	}
	var events, devs, after int
	var seq int64
	var root []byte
	_ = e.f.Super.QueryRow(`SELECT count(*) FROM authority.account_events WHERE account_id = $1`, a.ID).Scan(&events)
	_ = e.f.Super.QueryRow(`SELECT count(*) FROM authority.device_events WHERE account_id = $1`, a.ID).Scan(&devs)
	_ = e.f.Super.QueryRow(`SELECT count(*) FROM authority.key_bindings WHERE account_id = $1`, a.ID).Scan(&after)
	_ = e.f.Super.QueryRow(`SELECT seq, active_root FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&seq, &root)
	if events != 0 || devs != 0 || after != bindings || seq != 2 || len(root) != 97 {
		t.Fatalf("after purge: events %d, device events %d, bindings %d→%d, seq %d, root %d bytes", events, devs, bindings, after, seq, len(root))
	}
	if rep, _ := e.s.Purge(context.Background()); rep.Purged != 0 {
		t.Fatal("a purged tombstone was a candidate again (D-232)")
	}
	var lastMs int64
	if err := e.f.App.QueryRow(`SELECT floor(extract(epoch FROM last_purge_at) * 1000)::bigint FROM authority.meta`).
		Scan(&lastMs); err != nil || lastMs != i64(e.now.Load()) {
		t.Fatalf("last_purge_at = %d ms (%v), want db_now %d: Purge must record the database clock", lastMs, err, e.now.Load())
	}
}

func TestPurgeRefusals(t *testing.T) {
	e := newEnv(t)
	young := eraseAndAge(t, e, 29)
	old := eraseAndAge(t, e, 31)
	mustExec(t, e.f.Owner, `SELECT authority.enter_unreconciled()`)
	if rep, _ := e.s.Purge(context.Background()); rep.Purged != 0 {
		t.Fatal("purge ran while unreconciled")
	}
	mustExec(t, e.f.Super, `UPDATE authority.meta SET reconciled = true`)
	rep, _ := e.s.Purge(context.Background())
	if rep.Purged != 1 {
		t.Fatalf("purged %d, want only the account erased 31 d ago (%s), not %s", rep.Purged, old.ID, young.ID)
	}
}

// A pending erase qualifies on effective state: visible + 30 d, not held, its
// last event an erase. A held Pe and a cancelled Pe never qualify.
func TestPurgeActsOnEffectiveNotRawState(t *testing.T) {
	e := newEnv(t)
	pe, _ := e.enrolledWith()
	e.commitOK(pe, pe.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes)
	held, _ := e.enrolledWith()
	e.commitOK(held, held.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes)
	e.pinHeld(held.ID)
	cancelled, _ := e.enrolledWith()
	e.commitOK(cancelled, cancelled.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes)
	e.commitOK(cancelled, cancelled.CancelByRoot(t).Bytes)
	// purge_erased reads clock_timestamp(); move every activation base 31 d + Δ + G into the past.
	mustExec(t, e.f.Super, `UPDATE authority.account_heads SET activation_base_ms = activation_base_ms - 2592000000 - 86400000 - 900000
		WHERE pending_erase`)
	rep, _ := e.s.Purge(context.Background())
	if rep.Purged != 1 {
		t.Fatalf("purged %d, want exactly the unheld, uncancelled Pe", rep.Purged)
	}
	var state string
	_ = e.f.Super.QueryRow(`SELECT state FROM authority.account_heads WHERE account_id = $1`, pe.ID).Scan(&state)
	if state != ac.StateErased {
		t.Fatalf("the qualifying Pe was not materialised as E: %s", state)
	}
}

func TestGoAndSQLVisibleFormulasAgree(t *testing.T) {
	e := newEnv(t)
	cases := [][4]uint64{{1000, 100, 0, 0}, {1000, 100, 50, 0}, {1000, 100, ExtensionCapMs + 7, 0}, {1000, 100, 0, 9_999}, {0, 0, 0, 0}}
	for _, c := range cases {
		var sqlV int64
		if err := e.f.Owner.QueryRow(`SELECT authority.visible_ms($1, $2, $3, $4)`, i64(c[0]), i64(c[1]), i64(c[2]), i64(c[3])).Scan(&sqlV); err != nil {
			t.Fatal(err)
		}
		goV, ok := ac.VisibleInstant(c[0], c[1], c[2], ExtensionCapMs, c[3])
		if !ok || sqlV != i64(goV) {
			t.Fatalf("visible(%v): SQL %d, Go %d", c, sqlV, goV)
		}
	}
}
