package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// newTestOwner opens an Owner over the fixture's authority_owner pool, the
// role the owner verbs run as (B3-D3). It seeds meta with
// replication_required=false, as newTestStore does; tests that exercise the
// gate turn it on through the same owner connection.
func newTestOwner(t *testing.T, f *authoritydb.Fixture, r *authoritytest.Realm, cfg OwnerConfig) *Owner {
	t.Helper()
	f.SeedMeta(t, r.ID)
	cfg.DB = f.Owner
	cfg.Anchors = r.AnchorBytes
	o, err := OpenOwner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// stepProbe answers false for its first falseFor calls and true afterwards,
// counting every call.
type stepProbe struct {
	falseFor int64
	calls    atomic.Int64
}

func (p *stepProbe) Replicated(context.Context, *sql.Conn, string) (bool, error) {
	return p.calls.Add(1) > p.falseFor, nil
}

// B3-D7: every owner verb calls the leader check on its own connection, and a
// standby refuses with ErrNotLeader. conn is where the verbs' check lives;
// checkBeforeLock runs it ahead of ServeLock, and row 22 repeats it on the
// commit core's own connection and, for the realm, inside its transaction.
func TestOwnerConnRefusesOnStandby(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	var asked atomic.Int64
	o := newTestOwner(t, f, r, OwnerConfig{
		InRecovery: func(context.Context, *sql.Conn) (bool, error) {
			asked.Add(1)
			return true, nil
		},
	})
	conn, err := o.conn(context.Background())
	if conn != nil {
		_ = conn.Close()
		t.Fatal("conn returned a connection to a standby")
	}
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("conn on a standby = %v, want ErrNotLeader", err)
	}
	if asked.Load() == 0 {
		t.Fatal("conn never consulted the recovery hook")
	}
}

// A leader's conn hands back a usable connection, so the refusal above is the
// hook's doing and not conn failing for every caller.
func TestOwnerConnOpensOnLeader(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	o := newTestOwner(t, f, r, OwnerConfig{
		InRecovery: func(context.Context, *sql.Conn) (bool, error) { return false, nil },
	})
	conn, err := o.conn(context.Background())
	if err != nil {
		t.Fatalf("conn on a leader = %v, want nil", err)
	}
	defer func() { _ = conn.Close() }()
	var one int
	if err := conn.QueryRowContext(context.Background(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("the connection conn returned is unusable: %v", err)
	}
}

// A hook error fails closed, as step 1a does: it is neither ErrNotLeader (the
// operator would be told to find the primary) nor nil.
func TestOwnerConnFailsClosedWhenTheRoleReadFails(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	o := newTestOwner(t, f, r, OwnerConfig{
		InRecovery: func(context.Context, *sql.Conn) (bool, error) { return false, errors.New("boom") },
	})
	conn, err := o.conn(context.Background())
	if conn != nil {
		_ = conn.Close()
		t.Fatal("conn returned a connection after the role read failed")
	}
	if err == nil {
		t.Fatal("conn accepted a failed role read")
	}
	if errors.Is(err, ErrNotLeader) {
		t.Fatalf("a failed role read was reported as a standby: %v", err)
	}
}

// D-225 (B3-D8): finish returns only once a standby has confirmed the verb's
// commit, or says so when none did.
func TestOwnerFinishWaitsForReplication(t *testing.T) {
	t.Run("required and never confirmed: ErrNotReplicated", func(t *testing.T) {
		f := authoritydb.Setup(t)
		r := authoritytest.NewRealm(t)
		p := &fakeProbe{}
		o := newTestOwner(t, f, r, OwnerConfig{Probe: p})
		requireGate(t, f, true)
		conn, err := f.Owner.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		err = o.finish(ctx, conn)
		if !errors.Is(err, ErrNotReplicated) {
			t.Fatalf("finish with a false probe = %v, want ErrNotReplicated", err)
		}
		if p.calls.Load() < 2 {
			t.Fatalf("the probe was polled %d times in 150 ms, want it polled every RELEASE_POLL_MS", p.calls.Load())
		}
	})

	t.Run("required and confirmed: nil", func(t *testing.T) {
		f := authoritydb.Setup(t)
		r := authoritytest.NewRealm(t)
		p := &fakeProbe{}
		p.ok.Store(true)
		o := newTestOwner(t, f, r, OwnerConfig{Probe: p})
		requireGate(t, f, true)
		conn, err := f.Owner.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := o.finish(ctx, conn); err != nil {
			t.Fatalf("finish with a true probe = %v, want nil", err)
		}
		if p.calls.Load() == 0 {
			t.Fatal("finish returned nil without asking the probe")
		}
	})

	t.Run("not required: nil and the probe is never called", func(t *testing.T) {
		f := authoritydb.Setup(t)
		r := authoritytest.NewRealm(t)
		p := &fakeProbe{} // answers false: any call would stall finish
		o := newTestOwner(t, f, r, OwnerConfig{Probe: p})
		requireGate(t, f, false)
		conn, err := f.Owner.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := o.finish(ctx, conn); err != nil {
			t.Fatalf("finish with replication_required=false = %v, want nil", err)
		}
		if n := p.calls.Load(); n != 0 {
			t.Fatalf("the probe was called %d times with replication_required=false, want 0", n)
		}
	})
}

// B3-D8 / Task 3 step 3: release and finish wait through one helper, so they
// cannot diverge. Checked by behaviour: given the same probe script (false
// twice, then true), each returns success only after polling it a third time.
func TestReleaseAndFinishShareOneWait(t *testing.T) {
	t.Run("finish", func(t *testing.T) {
		f := authoritydb.Setup(t)
		r := authoritytest.NewRealm(t)
		p := &stepProbe{falseFor: 2}
		o := newTestOwner(t, f, r, OwnerConfig{Probe: p})
		requireGate(t, f, true)
		conn, err := f.Owner.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := o.finish(ctx, conn); err != nil {
			t.Fatalf("finish = %v, want nil once the probe turns true", err)
		}
		if n := p.calls.Load(); n < 3 {
			t.Fatalf("finish polled the probe %d times, want at least 3 (false, false, true)", n)
		}
	})

	t.Run("release", func(t *testing.T) {
		f := authoritydb.Setup(t)
		r := authoritytest.NewRealm(t)
		p := &stepProbe{falseFor: 2}
		s := newTestStore(t, f, r, p, Hooks{})
		requireGate(t, f, true)
		conn, err := f.App.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		signed := Outcome{HTTP: 200, Kind: KindCommitted, Body: []byte("SIGNED-BYTES")}
		out := s.release(ctx, conn, signed)
		if out.HTTP != 200 || string(out.Body) != "SIGNED-BYTES" {
			t.Fatalf("release = %+v, want the signed bytes once the probe turns true", out)
		}
		if n := p.calls.Load(); n < 3 {
			t.Fatalf("release polled the probe %d times, want at least 3 (false, false, true)", n)
		}
	})
}

// authoritySnapshot is every table an owner verb could write, as text: meta,
// the delegation rows, revoked_keys and account_heads. Equal snapshots mean a
// verb wrote nothing.
func authoritySnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT coalesce(string_agg(to_jsonb(m)::text, ';'), '') FROM authority.meta m`,
		`SELECT coalesce(string_agg((to_jsonb(d) - 'bytes' - 'sig_a' - 'sig_b')::text, ';' ORDER BY product_root_gen, serial), '') FROM authority.delegations d`,
		`SELECT coalesce(string_agg(to_jsonb(r)::text, ';' ORDER BY key_digest), '') FROM authority.revoked_keys r`,
		`SELECT coalesce(string_agg(to_jsonb(h)::text, ';' ORDER BY account_id), '') FROM authority.account_heads h`,
	} {
		var part string
		if err := db.QueryRow(q).Scan(&part); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		b.WriteString(part)
		b.WriteString("\n")
	}
	return b.String()
}

// B3-D7: every owner verb calls the leader check on its own connection, and a
// standby refuses with ErrNotLeader before it writes anything. Each case is
// arranged so that, on a primary, the verb WOULD write: a pin that is listed,
// a gate that is off, a delegation that is superseded.
//
// The table covers every owner verb, clear-held included.
func TestOwnerVerbOnStandbyIsNotLeader(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	newTestStore(t, f, r, &fakeProbe{}, Hooks{}) // seeds meta, installs serial 1
	next := r.NextDelegation(t)
	r.Install(t, f.App, next) // serial 1 is now superseded
	pin := r.Current.D.CPTLSPins[0]
	// A population the seal would accept on a primary: one row, and its digest.
	pop := legacyPop(1)
	mustExec(t, f.Owner, `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`,
		pop[0].AccountID, pop[0].LegacyEKDigest)
	popDigest := populationDigest(t, pop)
	// reconcile-complete writes only while unreconciled; restore-begin writes either way.
	mustExec(t, f.Owner, `SELECT authority.enter_unreconciled()`)
	trust := trustFile(t, r.Current)

	// clear-held: a held head a primary would clear, and a signer it would sign with.
	heldAcct := "00000000-0000-4000-8000-0000000000c1"
	insertHeldA0(t, f.Owner, heldAcct)

	var asked atomic.Int64
	o, err := OpenOwner(OwnerConfig{
		DB: f.Owner, Anchors: r.AnchorBytes, Signer: r.RSK,
		InRecovery: func(context.Context, *sql.Conn) (bool, error) {
			asked.Add(1)
			return true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	verbs := []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"revoke-key", func(ctx context.Context) error { return o.RevokeKey(ctx, "cp_tls", pin) }},
		{"replication-gate", func(ctx context.Context) error { return o.ReplicationGate(ctx, true) }},
		{"end-drain", func(ctx context.Context) error { return o.EndDrain(ctx, 0, 1) }},
		{"seal-legacy", func(ctx context.Context) error { return o.SealLegacy(ctx, popDigest) }},
		{"restore-begin", func(ctx context.Context) error { _, err := o.RestoreBegin(ctx, trust); return err }},
		{"reconcile-complete", func(ctx context.Context) error { _, err := o.ReconcileComplete(ctx); return err }},
		{"clear-held", func(ctx context.Context) error { _, err := o.ClearHeld(ctx, []string{heldAcct}); return err }},
	}
	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			before := authoritySnapshot(t, f.App)
			asked.Store(0)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := v.run(ctx); !errors.Is(err, ErrNotLeader) {
				t.Fatalf("%s on a standby = %v, want ErrNotLeader", v.name, err)
			}
			if asked.Load() == 0 {
				t.Fatalf("%s never consulted the role read", v.name)
			}
			if after := authoritySnapshot(t, f.App); after != before {
				t.Fatalf("%s wrote on a standby:\nbefore %s\nafter  %s", v.name, before, after)
			}
		})
	}
}

// ownerVerbCase is one owner verb, called as an operator would.
type ownerVerbCase struct {
	name string
	// arrange puts the database where, on the right realm's primary, the verb
	// would write; nil needs nothing.
	arrange func()
	run     func(ctx context.Context, o *Owner) error
}

// ownerVerbCases is every owner verb, clear-held and its plan included. The
// database a case is aimed at is f's, and trust is a trust file for the
// anchors the Owner under test holds, so only the realm check can refuse it.
func ownerVerbCases(t *testing.T, f *authoritydb.Fixture, pin []byte, trust []byte) []ownerVerbCase {
	t.Helper()
	pop := legacyPop(1)
	popDigest := populationDigest(t, pop)
	heldAcct := "00000000-0000-4000-8000-0000000000c1"
	return []ownerVerbCase{
		{name: "replication-gate on", run: func(ctx context.Context, o *Owner) error { return o.ReplicationGate(ctx, true) }},
		{name: "replication-gate off", arrange: func() { requireGate(t, f, true) },
			run: func(ctx context.Context, o *Owner) error { return o.ReplicationGate(ctx, false) }},
		{name: "reconcile-complete", arrange: func() { mustExec(t, f.Owner, `SELECT authority.enter_unreconciled()`) },
			run: func(ctx context.Context, o *Owner) error { _, err := o.ReconcileComplete(ctx); return err }},
		{name: "seal-legacy", arrange: func() {
			mustExec(t, f.Owner, `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`,
				pop[0].AccountID, pop[0].LegacyEKDigest)
		}, run: func(ctx context.Context, o *Owner) error { return o.SealLegacy(ctx, popDigest) }},
		{name: "end-drain", run: func(ctx context.Context, o *Owner) error { return o.EndDrain(ctx, 0, 1) }},
		{name: "revoke-key", run: func(ctx context.Context, o *Owner) error { return o.RevokeKey(ctx, "cp_tls", pin) }},
		{name: "restore-begin", run: func(ctx context.Context, o *Owner) error { _, err := o.RestoreBegin(ctx, trust); return err }},
		{name: "clear-held", arrange: func() { insertHeldA0(t, f.Owner, heldAcct) },
			run: func(ctx context.Context, o *Owner) error { _, err := o.ClearHeld(ctx, []string{heldAcct}); return err }},
		{name: "clear-held --dry-run", arrange: func() { insertHeldA0(t, f.Owner, heldAcct) },
			run: func(ctx context.Context, o *Owner) error {
				_, err := o.ClearHeldPlan(ctx, []string{heldAcct})
				return err
			}},
	}
}

// arrangeOnce runs each case's arrange once: a clear-held pair shares a row.
func arrangeOnce(cases []ownerVerbCase) {
	heldRow := false
	for _, c := range cases {
		if c.arrange == nil || (strings.HasPrefix(c.name, "clear-held") && heldRow) {
			continue
		}
		heldRow = heldRow || strings.HasPrefix(c.name, "clear-held")
		c.arrange()
	}
}

// B2-D10 on every owner verb (Codex round 2, G4): an Owner whose anchors name
// another realm than the database's meta is refused with ErrRealmMismatch by
// all of them, and the database is as it was. The cases are arranged so that,
// on the right realm's primary, each verb WOULD write. conn is where the check
// lives; before it, only restore-begin and clear-held checked at all.
func TestOwnerVerbOnAnotherRealmIsRefused(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	other := authoritytest.NewRealm(t) // a second realm: its own ID and anchors
	newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	rsk, err := signer.GenerateP384() // D-223: an RSK is listed in one delegation only
	if err != nil {
		t.Fatal(err)
	}
	r.Install(t, f.App, r.NextDelegation(t, authoritytest.WithRSKs(rsk.PublicKey()))) // serial 1 is superseded: end-drain has something to drain
	cases := ownerVerbCases(t, f, r.Current.D.CPTLSPins[0], trustFile(t, other.Current))
	arrangeOnce(cases)
	o, err := OpenOwner(OwnerConfig{DB: f.Owner, Anchors: other.AnchorBytes, Signer: other.RSK})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := authoritySnapshot(t, f.App)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.run(ctx, o); !errors.Is(err, ErrRealmMismatch) {
				t.Fatalf("%s over another realm's primary = %v, want ErrRealmMismatch", c.name, err)
			}
			if after := authoritySnapshot(t, f.App); after != before {
				t.Fatalf("%s wrote on another realm's primary:\nbefore %s\nafter  %s", c.name, before, after)
			}
		})
	}

	// The control (T2): every verb, over the right realm and the same arranged
	// database, passes the realm check and runs, so each refusal above is the
	// realm's and not an arrangement fault of that verb. A fresh run of each case
	// with a trust file for the right realm.
	probe := &fakeProbe{}
	probe.ok.Store(true)
	right, err := OpenOwner(OwnerConfig{DB: f.Owner, Anchors: r.AnchorBytes, Signer: rsk, Probe: probe})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ownerVerbCases(t, f, r.Current.D.CPTLSPins[0], trustFile(t, r.Current)) {
		t.Run("control: "+c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.run(ctx, right); err != nil {
				t.Fatalf("%s over the right realm = %v, want nil (the control)", c.name, err)
			}
		})
	}
}

// R1: the leader and realm checks run before the verb takes ServeLock. With
// another session holding the lock (serve, on a primary of the right realm), a
// standby or another realm's primary must still be refused as that and not as
// ErrServeLockHeld, whose remedy does not apply there. Each verb that takes the
// lock is checked: restore-begin and reconcile-complete (stopped), and
// clear-held. The control is the right realm's primary, which meets the lock.
func TestOwnerVerbChecksPrecedeServeLock(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	other := authoritytest.NewRealm(t)
	newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	locking := map[string]bool{"restore-begin": true, "reconcile-complete": true, "clear-held": true}
	cases := ownerVerbCases(t, f, r.Current.D.CPTLSPins[0], trustFile(t, r.Current))
	arrangeOnce(cases)
	held, err := AcquireServeLock(context.Background(), f.App)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	for _, tc := range []struct {
		name string
		cfg  OwnerConfig
		want error
	}{
		{"a standby", OwnerConfig{Anchors: r.AnchorBytes, Signer: r.RSK,
			InRecovery: func(context.Context, *sql.Conn) (bool, error) { return true, nil }}, ErrNotLeader},
		{"another realm's primary", OwnerConfig{Anchors: other.AnchorBytes, Signer: other.RSK}, ErrRealmMismatch},
		{"control: the right realm's primary", OwnerConfig{Anchors: r.AnchorBytes, Signer: r.RSK}, ErrServeLockHeld},
	} {
		tc.cfg.DB = f.Owner
		o, err := OpenOwner(tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if !locking[c.name] {
				continue
			}
			t.Run(tc.name+"/"+c.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := c.run(ctx, o)
				if !errors.Is(err, tc.want) {
					t.Fatalf("%s with ServeLock held elsewhere, %s = %v, want %v", c.name, tc.name, err, tc.want)
				}
				if tc.want != ErrServeLockHeld && errors.Is(err, ErrServeLockHeld) {
					t.Fatalf("%s, %s = %v: refused as the lock, not as what it is", c.name, tc.name, err)
				}
			})
		}
	}
}

// A migrated database with no meta row is ErrNoMeta on every owner verb (B2-D10),
// the CLI's config exit, and not a verb run against an empty authority.
func TestOwnerVerbWithNoMetaRowIsRefused(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t) // meta is never seeded: the bareStore shape
	cases := ownerVerbCases(t, f, r.Current.D.CPTLSPins[0], trustFile(t, r.Current))
	o, err := OpenOwner(OwnerConfig{DB: f.Owner, Anchors: r.AnchorBytes, Signer: r.RSK})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.run(ctx, o); !errors.Is(err, ErrNoMeta) {
				t.Fatalf("%s with no meta row = %v, want ErrNoMeta", c.name, err)
			}
		})
	}
	var n int
	if err := f.Super.QueryRow(`SELECT count(*) FROM authority.meta`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("meta has %d rows (%v) after refused verbs, want none", n, err)
	}
}

// The leader check is first (B3-D7): a standby over another realm's database is
// ErrNotLeader, not ErrRealmMismatch, so the operator is sent to the primary
// before being told anything about its realm.
func TestOwnerVerbOnAStandbyOfAnotherRealmIsNotLeader(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	other := authoritytest.NewRealm(t)
	newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	cases := ownerVerbCases(t, f, r.Current.D.CPTLSPins[0], trustFile(t, other.Current))
	o, err := OpenOwner(OwnerConfig{DB: f.Owner, Anchors: other.AnchorBytes, Signer: other.RSK,
		InRecovery: func(context.Context, *sql.Conn) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := c.run(ctx, o)
			if !errors.Is(err, ErrNotLeader) || errors.Is(err, ErrRealmMismatch) {
				t.Fatalf("%s on a standby of another realm = %v, want ErrNotLeader alone", c.name, err)
			}
		})
	}
}
