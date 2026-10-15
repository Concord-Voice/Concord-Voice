package store

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// env pins db_now through Hooks.DBNow, so a test advances time exactly:
// Δ, G, N_cancel and every window edge become deterministic.
type env struct {
	t     *testing.T
	f     *authoritydb.Fixture
	r     *authoritytest.Realm
	s     *Store
	probe *fakeProbe
	now   atomic.Uint64
}

func newEnv(t *testing.T, opts ...authoritytest.RealmOption) *env {
	t.Helper()
	e := &env{t: t, f: authoritydb.Setup(t), r: authoritytest.NewRealm(t, opts...), probe: &fakeProbe{}}
	e.now.Store(e.r.Clock.Ms)
	e.s = newTestStore(t, e.f, e.r, e.probe, Hooks{DBNow: func(context.Context) (uint64, error) { return e.now.Load(), nil },
		Pause: func() { e.now.Add(1) }}) // D-241's fence waits for the pinned clock to move on
	seal(t, e.f)
	return e
}

func (e *env) advance(ms uint64) {
	e.now.Add(ms)
	e.r.Clock.Ms = e.now.Load()
}

func (e *env) submit(b []byte) Outcome { return e.s.Submit(context.Background(), b) }

func (e *env) device(b []byte) Outcome { return e.s.SubmitDevice(context.Background(), b) }

// commitOK submits and requires `committed`; it syncs a's head and returns the result.
func (e *env) commitOK(a *authoritytest.Account, b []byte) ac.Result {
	e.t.Helper()
	out := e.submit(b)
	if out.Kind != KindCommitted {
		e.t.Fatalf("want committed, got %+v (%s)", out, out.Code)
	}
	c, err := ac.DecodeCommitted(out.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := ac.DecodeResult(c.Result)
	if err != nil {
		e.t.Fatal(err)
	}
	a.Sync(e.t, e.f.App)
	return res
}

func (e *env) refusedAs(b []byte, want ac.Refusal) {
	e.t.Helper()
	if out := e.submit(b); out.Kind != KindRefused || out.Code != string(want) {
		e.t.Fatalf("want refused %s, got %+v", want, out)
	}
}

func (e *env) errorAs(b []byte, http int, want ac.ErrorCode) {
	e.t.Helper()
	if out := e.submit(b); out.Kind != KindError || out.HTTP != http || out.Code != string(want) {
		e.t.Fatalf("want %d %s, got %+v", http, want, out)
	}
}

// enrolledWith enrolls with a known genesis device, so later actor
// operations can sign with its dsk.
func (e *env) enrolledWith() (*authoritytest.Account, *authoritytest.Device) {
	e.t.Helper()
	a := e.r.NewAccount(e.t)
	dev := authoritytest.NewDevice(e.t)
	b := a.Build(e.t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: dev,
		Factors: []string{ac.FactorRegistration}})
	e.commitOK(a, b.Bytes)
	a.Activated()
	a.Actor = dev
	a.Devices[dev.ID] = dev
	return a, dev
}

func (e *env) pinHeld(acct string) {
	mustExec(e.t, e.f.Owner, `UPDATE authority.account_heads SET held = true WHERE account_id = $1`, acct)
}

// newEnvPop seeds n legacy-population accounts (each with a real RSA-4096 pin)
// before the seal, then seals.
func newEnvPop(t *testing.T, n int, opts ...authoritytest.RealmOption) (*env, []*authoritytest.Account) {
	t.Helper()
	e := &env{t: t, f: authoritydb.Setup(t), r: authoritytest.NewRealm(t, opts...), probe: &fakeProbe{}}
	e.now.Store(e.r.Clock.Ms)
	e.s = newTestStore(t, e.f, e.r, e.probe, Hooks{DBNow: func(context.Context) (uint64, error) { return e.now.Load(), nil },
		Pause: func() { e.now.Add(1) }})
	var out []*authoritytest.Account
	for i := 0; i < n; i++ {
		a := e.r.NewAccount(t)
		lk := authoritytest.NewLegacyKey(t)
		a.Legacy = &lk
		mustExec(t, e.f.App, `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`, a.ID, lk.Digest)
		a.Head = authoritytest.HeadView{State: ac.StateAbsent}
		out = append(out, a)
	}
	seal(t, e.f)
	return e, out
}

// drainBucket empties the global commit bucket (D-211).
func (e *env) drainBucket() {
	e.s.adm.mu.Lock()
	e.s.adm.tokens, e.s.adm.last = 0, e.s.adm.now()
	e.s.adm.mu.Unlock()
	e.s.adm.now = func() time.Time { return e.s.adm.last } // no refill during the test
}

// waitForLockWaiters polls pg_locks until n backends of this database wait on
// an ungranted lock: contention proven, not assumed (§3.2.5).
func waitForLockWaiters(t *testing.T, super *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := super.QueryRow(`SELECT count(DISTINCT l.pid) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d lock waiters after 5 s", n)
}
