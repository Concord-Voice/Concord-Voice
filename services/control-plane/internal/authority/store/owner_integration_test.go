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
// standby refuses with ErrNotLeader. conn is the one place that check lives.
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
