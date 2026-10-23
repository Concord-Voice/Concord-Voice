package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// Task 4's live owner verbs: revoke-key, replication-gate and end-drain
// (DoR:1734, 1735, 1733; B3-D8, B3-D10). Their server-side half, the
// "takes effect without a restart" claim, is TestEndDrainTakesEffectWithoutRestart
// in internal/authority/server/owner_integration_test.go.

// liveEnv is a realm with delegation serial 1 installed, a Store over the app
// pool (to read what serve reads) and an Owner over the owner pool (to run the
// verbs). meta.replication_required is false, as the fixture seeds it.
type liveEnv struct {
	f *authoritydb.Fixture
	r *authoritytest.Realm
	o *Owner
	s *Store
}

func newLiveEnv(t *testing.T, cfg OwnerConfig) *liveEnv {
	t.Helper()
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{}) // seeds meta, installs r.Current
	cfg.DB = f.Owner
	cfg.Anchors = r.AnchorBytes
	o, err := OpenOwner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &liveEnv{f: f, r: r, o: o, s: s}
}

// supersede installs serial+1 of the current generation, so the delegation
// that was current is now superseded (T9).
func (e *liveEnv) supersede(t *testing.T, opts ...authoritytest.RealmOption) authoritytest.SignedDelegation {
	t.Helper()
	next := e.r.NextDelegation(t, opts...)
	e.r.Install(t, e.f.App, next)
	return next
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func revokedCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM authority.revoked_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// revokedKind is the kind revoked_keys holds for digest, and whether a row exists.
func revokedKind(t *testing.T, db *sql.DB, digest []byte) (string, bool) {
	t.Helper()
	var kind string
	err := db.QueryRow(`SELECT kind FROM authority.revoked_keys WHERE key_digest = $1`, digest).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return kind, true
}

func drainGrace(t *testing.T, db *sql.DB, serial uint64) int64 {
	t.Helper()
	var g int64
	if err := db.QueryRow(`SELECT drain_grace_ms FROM authority.delegations WHERE product_root_gen = 0 AND serial = $1`,
		serial).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

func replicationRequired(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var on bool
	if err := db.QueryRow(`SELECT replication_required FROM authority.meta WHERE id = 1`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	return on
}

func tlsPin(t *testing.T, point []byte) []byte {
	t.Helper()
	pin, err := ac.TLSPin(point)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

// ---- revoke-key ----

// B3-D10: a cp_tls row holds the pin, SHA-384 of the 120-byte SPKI DER, which
// is what the listener and the per-request check compare (DoR:1734).
func TestRevokeKeyCPTLSWritesThePin(t *testing.T) {
	e := newLiveEnv(t, OwnerConfig{})
	pin := tlsPin(t, authoritytest.Point(e.r.CPTLS))
	ctx := liveCtx(t)
	if revoked, err := e.s.Revoked(ctx, pin); err != nil || revoked {
		t.Fatalf("Revoked before the verb = %v, %v; want false (the control)", revoked, err)
	}
	if err := e.o.RevokeKey(ctx, "cp_tls", pin); err != nil {
		t.Fatalf("RevokeKey(cp_tls, pin) = %v, want nil", err)
	}
	if n := revokedCount(t, e.f.App); n != 1 {
		t.Fatalf("revoked_keys holds %d rows, want 1", n)
	}
	if kind, ok := revokedKind(t, e.f.App, pin); !ok || kind != "cp_tls" {
		t.Fatalf("the row for the pin is kind %q (present %v), want cp_tls", kind, ok)
	}
	if revoked, err := e.s.Revoked(ctx, pin); err != nil || !revoked {
		t.Fatalf("Revoked after the verb = %v, %v; want true", revoked, err)
	}
}

// Review Focus 4. A cp_tls row written as the key digest is accepted by the
// schema and never matches, so the CP key stays admitted: the verb refuses it,
// writes nothing, and names the pin form.
func TestRevokeKeyCPTLSKeyDigestRefused(t *testing.T) {
	e := newLiveEnv(t, OwnerConfig{})
	point := authoritytest.Point(e.r.CPTLS)
	err := e.o.RevokeKey(liveCtx(t), "cp_tls", ac.KeyDigest(point))
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("RevokeKey(cp_tls, KeyDigest(point)) = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "cp_tls takes the pin") {
		t.Fatalf("the refusal %q does not say \"cp_tls takes the pin\"", err)
	}
	if n := revokedCount(t, e.f.App); n != 0 {
		t.Fatalf("revoked_keys holds %d rows after a refusal, want 0", n)
	}
	if revoked, rerr := e.s.Revoked(liveCtx(t), tlsPin(t, point)); rerr != nil || revoked {
		t.Fatalf("the CP's real pin is revoked = %v, %v after a refused key-digest call", revoked, rerr)
	}
}

// A cak or operator row is ac.KeyDigest of the 97-byte point exactly as the
// delegation lists it (cak_public_keys and operator_public_keys are kP97), and
// a key listed under one kind is not accepted under the other.
func TestRevokeKeyCAKAndOperator(t *testing.T) {
	t.Run("the listed CAK point", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		d := ac.KeyDigest(e.r.Current.D.CAKPublicKeys[0])
		if err := e.o.RevokeKey(liveCtx(t), "cak", d); err != nil {
			t.Fatalf("RevokeKey(cak) = %v, want nil", err)
		}
		if kind, ok := revokedKind(t, e.f.App, d); !ok || kind != "cak" {
			t.Fatalf("the row is kind %q (present %v), want cak", kind, ok)
		}
		if revoked, err := e.s.Revoked(liveCtx(t), d); err != nil || !revoked {
			t.Fatalf("Revoked = %v, %v; want true", revoked, err)
		}
	})

	t.Run("the listed operator point", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		d := ac.KeyDigest(e.r.Current.D.OperatorPublicKeys[0])
		if err := e.o.RevokeKey(liveCtx(t), "operator", d); err != nil {
			t.Fatalf("RevokeKey(operator) = %v, want nil", err)
		}
		if kind, ok := revokedKind(t, e.f.App, d); !ok || kind != "operator" {
			t.Fatalf("the row is kind %q (present %v), want operator", kind, ok)
		}
	})

	// The runbook tells an operator to hash `tail -c 97` of the key's SPKI DER.
	// A P-384 SPKI ends with the uncompressed point, so that is the listed
	// point byte for byte, and the digest is the one the verb accepts.
	t.Run("the runbook's tail -c 97 of the SPKI", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		point := e.r.Current.D.CAKPublicKeys[0]
		spki, err := ac.P384SPKI(point)
		if err != nil {
			t.Fatal(err)
		}
		tail := spki[len(spki)-97:]
		if !bytes.Equal(tail, point) {
			t.Fatal("the last 97 bytes of the SPKI DER are not the listed point; the runbook's recipe is wrong")
		}
		if err := e.o.RevokeKey(liveCtx(t), "cak", ac.KeyDigest(tail)); err != nil {
			t.Fatalf("RevokeKey(cak, KeyDigest(tail -c 97)) = %v, want nil", err)
		}
	})

	t.Run("the SHA-384 of the whole SPKI is not a cak digest", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		spki, err := ac.P384SPKI(e.r.Current.D.CAKPublicKeys[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := e.o.RevokeKey(liveCtx(t), "cak", ac.Digest(spki)); !errors.Is(err, ErrRefused) {
			t.Fatalf("RevokeKey(cak, SHA-384(SPKI)) = %v, want ErrRefused", err)
		}
		if n := revokedCount(t, e.f.App); n != 0 {
			t.Fatalf("revoked_keys holds %d rows, want 0", n)
		}
	})

	for _, c := range []struct {
		name, kind string
		listed     func(e *liveEnv) []byte
	}{
		{"a listed operator key under kind cak", "cak", func(e *liveEnv) []byte { return e.r.Current.D.OperatorPublicKeys[0] }},
		{"a listed CAK under kind operator", "operator", func(e *liveEnv) []byte { return e.r.Current.D.CAKPublicKeys[0] }},
		{"a listed CAK under kind cp_tls", "cp_tls", func(e *liveEnv) []byte { return e.r.Current.D.CAKPublicKeys[0] }},
		{"a listed authority TLS key under kind cak", "cak", func(e *liveEnv) []byte { return e.r.Current.D.AuthorityTLSKeys[0] }},
	} {
		t.Run("refused: "+c.name, func(t *testing.T) {
			e := newLiveEnv(t, OwnerConfig{})
			if err := e.o.RevokeKey(liveCtx(t), c.kind, ac.KeyDigest(c.listed(e))); !errors.Is(err, ErrRefused) {
				t.Fatalf("RevokeKey(%s, wrong kind's digest) = %v, want ErrRefused", c.kind, err)
			}
			if n := revokedCount(t, e.f.App); n != 0 {
				t.Fatalf("revoked_keys holds %d rows after a refusal, want 0", n)
			}
		})
	}

	// Containment is for a key a ceremony has already dropped from the newest
	// delegation but a superseded one still lists (DoR:1060, P-OVERLAP).
	t.Run("a key listed only by a superseded delegation", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		oldCAK := e.r.Current.D.CAKPublicKeys[0]
		oldPin := e.r.Current.D.CPTLSPins[0]
		e.supersede(t, func(d *ac.Delegation) {
			d.CAKPublicKeys = [][]byte{authoritytest.Point(authoritytest.NewRealm(t).CAK)}
			d.CPTLSPins = [][]byte{tlsPin(t, authoritytest.Point(authoritytest.NewRealm(t).CPTLS))}
		})
		if err := e.o.RevokeKey(liveCtx(t), "cak", ac.KeyDigest(oldCAK)); err != nil {
			t.Fatalf("RevokeKey(cak) of a key only serial 1 lists = %v, want nil", err)
		}
		if err := e.o.RevokeKey(liveCtx(t), "cp_tls", oldPin); err != nil {
			t.Fatalf("RevokeKey(cp_tls) of a pin only serial 1 lists = %v, want nil", err)
		}
		if n := revokedCount(t, e.f.App); n != 2 {
			t.Fatalf("revoked_keys holds %d rows, want 2", n)
		}
	})
}

// The digest must be of a key some installed delegation lists (B3-D10): a
// mistyped containment row is accepted by the schema and silently never matches.
func TestRevokeKeyUnknownDigestRefused(t *testing.T) {
	for _, kind := range []string{"cak", "operator", "cp_tls"} {
		t.Run(kind, func(t *testing.T) {
			e := newLiveEnv(t, OwnerConfig{})
			if err := e.o.RevokeKey(liveCtx(t), kind, d48(0xab)); !errors.Is(err, ErrRefused) {
				t.Fatalf("RevokeKey(%s, unknown) = %v, want ErrRefused", kind, err)
			}
			if n := revokedCount(t, e.f.App); n != 0 {
				t.Fatalf("revoked_keys holds %d rows, want 0", n)
			}
		})
	}

	t.Run("a digest of the wrong length", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		if err := e.o.RevokeKey(liveCtx(t), "cak", ac.KeyDigest(e.r.Current.D.CAKPublicKeys[0])[:47]); err == nil {
			t.Fatal("RevokeKey accepted a 47-byte digest")
		}
		if n := revokedCount(t, e.f.App); n != 0 {
			t.Fatalf("revoked_keys holds %d rows, want 0", n)
		}
	})

	t.Run("a kind the table does not know", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		if err := e.o.RevokeKey(liveCtx(t), "rsk", ac.KeyDigest(e.r.Current.D.RSKPublicKeys[0])); err == nil {
			t.Fatal("RevokeKey accepted kind rsk")
		}
		if n := revokedCount(t, e.f.App); n != 0 {
			t.Fatalf("revoked_keys holds %d rows, want 0", n)
		}
	})

	t.Run("no delegation installed", func(t *testing.T) {
		f := authoritydb.Setup(t)
		r := authoritytest.NewRealm(t)
		f.SeedMeta(t, r.ID)
		o, err := OpenOwner(OwnerConfig{DB: f.Owner, Anchors: r.AnchorBytes})
		if err != nil {
			t.Fatal(err)
		}
		d := ac.KeyDigest(r.Current.D.CAKPublicKeys[0]) // listed by a delegation that was never installed
		if err := o.RevokeKey(liveCtx(t), "cak", d); !errors.Is(err, ErrRefused) {
			t.Fatalf("RevokeKey against an empty delegations table = %v, want ErrRefused", err)
		}
		if n := revokedCount(t, f.App); n != 0 {
			t.Fatalf("revoked_keys holds %d rows, want 0", n)
		}
	})
}

// B3-D8: a re-run on an existing row exits 0 and leaves one row.
func TestRevokeKeyIdempotent(t *testing.T) {
	for _, c := range []struct {
		kind   string
		digest func(e *liveEnv) []byte
	}{
		{"cp_tls", func(e *liveEnv) []byte { return e.r.Current.D.CPTLSPins[0] }},
		{"cak", func(e *liveEnv) []byte { return ac.KeyDigest(e.r.Current.D.CAKPublicKeys[0]) }},
		{"operator", func(e *liveEnv) []byte { return ac.KeyDigest(e.r.Current.D.OperatorPublicKeys[0]) }},
	} {
		t.Run(c.kind, func(t *testing.T) {
			e := newLiveEnv(t, OwnerConfig{})
			d := c.digest(e)
			for i := range 2 {
				if err := e.o.RevokeKey(liveCtx(t), c.kind, d); err != nil {
					t.Fatalf("call %d: RevokeKey = %v, want nil", i+1, err)
				}
			}
			if n := revokedCount(t, e.f.App); n != 1 {
				t.Fatalf("revoked_keys holds %d rows after two calls, want 1", n)
			}
		})
	}
}

// ---- replication-gate ----

func TestReplicationGate(t *testing.T) {
	// Once the gate is on, finish waits for a standby the single test node does
	// not have, so a probe that confirms stands in for it.
	p := &fakeProbe{}
	p.ok.Store(true)
	e := newLiveEnv(t, OwnerConfig{Probe: p})
	ctx := liveCtx(t)
	if replicationRequired(t, e.f.App) {
		t.Fatal("the fixture starts with replication_required = true")
	}
	// The grant fence (DoR:1401): the app pool cannot write the column, so only
	// the owner verb can move it.
	mustFail(t, e.f.App, "permission denied", `UPDATE authority.meta SET replication_required = $1`, true)
	if replicationRequired(t, e.f.App) {
		t.Fatal("the app pool changed replication_required")
	}

	steps := []struct {
		on, want bool
		what     string
	}{
		{true, true, "on"},
		{true, true, "on again"},
		{false, false, "off"},
		{false, false, "off again"},
	}
	for _, s := range steps {
		if err := e.o.ReplicationGate(ctx, s.on); err != nil {
			t.Fatalf("ReplicationGate(%s) = %v, want nil", s.what, err)
		}
		if got := replicationRequired(t, e.f.App); got != s.want {
			t.Fatalf("after %s: replication_required = %v, want %v", s.what, got, s.want)
		}
	}
	mustFail(t, e.f.App, "permission denied", `UPDATE authority.meta SET replication_required = $1`, true)
}

// ---- end-drain ----

func TestEndDrain(t *testing.T) {
	t.Run("a superseded delegation goes 660000 to 0, and a repeat is nil", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		e.supersede(t)
		if g := drainGrace(t, e.f.App, 1); g != 660000 {
			t.Fatalf("serial 1 starts with drain_grace_ms %d, want 660000 (the control)", g)
		}
		if err := e.o.EndDrain(liveCtx(t), 0, 1); err != nil {
			t.Fatalf("EndDrain(0, 1) = %v, want nil", err)
		}
		if g := drainGrace(t, e.f.App, 1); g != 0 {
			t.Fatalf("serial 1 drain_grace_ms = %d after EndDrain, want 0", g)
		}
		if g := drainGrace(t, e.f.App, 2); g != 660000 {
			t.Fatalf("serial 2 drain_grace_ms = %d, want 660000 untouched", g)
		}
		if err := e.o.EndDrain(liveCtx(t), 0, 1); err != nil {
			t.Fatalf("the repeat EndDrain(0, 1) = %v, want nil (B3-D8)", err)
		}
		if g := drainGrace(t, e.f.App, 1); g != 0 {
			t.Fatalf("serial 1 drain_grace_ms = %d after the repeat, want 0", g)
		}
	})

	t.Run("the current delegation is refused", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		if err := e.o.EndDrain(liveCtx(t), 0, 1); !errors.Is(err, ErrRefused) {
			t.Fatalf("EndDrain of the only delegation = %v, want ErrRefused", err)
		}
		if g := drainGrace(t, e.f.App, 1); g != 660000 {
			t.Fatalf("drain_grace_ms = %d after a refusal, want 660000", g)
		}
		e.supersede(t)
		if err := e.o.EndDrain(liveCtx(t), 0, 2); !errors.Is(err, ErrRefused) {
			t.Fatalf("EndDrain of the newest delegation = %v, want ErrRefused", err)
		}
		if g := drainGrace(t, e.f.App, 2); g != 660000 {
			t.Fatalf("serial 2 drain_grace_ms = %d after a refusal, want 660000", g)
		}
	})

	t.Run("an unknown generation and serial is refused", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		e.supersede(t)
		for _, c := range []struct{ gen, serial uint64 }{{0, 9}, {7, 1}, {0, 0}} {
			if err := e.o.EndDrain(liveCtx(t), c.gen, c.serial); !errors.Is(err, ErrRefused) {
				t.Fatalf("EndDrain(%d, %d) = %v, want ErrRefused", c.gen, c.serial, err)
			}
		}
		if g := drainGrace(t, e.f.App, 1); g != 660000 {
			t.Fatalf("serial 1 drain_grace_ms = %d after refusals, want 660000", g)
		}
	})

	// B1-D36: a delegation the trust ignored supersedes nothing, so serial 1 is
	// still current and the verb refuses.
	t.Run("a superseding delegation the trust ignored supersedes nothing", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		good := e.r.Current
		bad := e.r.NextDelegation(t)
		e.r.Current = good
		bad.SigB = append([]byte(nil), bad.SigB...)
		bad.SigB[0] ^= 0xff // fails dual control: AddSigned returns ErrIgnored
		e.r.Install(t, e.f.App, bad)
		if err := e.o.EndDrain(liveCtx(t), 0, 1); !errors.Is(err, ErrRefused) {
			t.Fatalf("EndDrain(0, 1) superseded only by an ignored delegation = %v, want ErrRefused", err)
		}
		if g := drainGrace(t, e.f.App, 1); g != 660000 {
			t.Fatalf("drain_grace_ms = %d, want 660000", g)
		}
	})
}

// ---- D-225: each live verb ends on the replication check ----

// A verb that commits and cannot confirm a standby says so with
// ErrNotReplicated, leaves its write committed, and converges on a re-run.
func TestLiveVerbsEndOnTheReplicationCheck(t *testing.T) {
	cases := []struct {
		name      string
		prepare   func(t *testing.T, e *liveEnv) // runs with replication_required still false
		gateFirst bool                           // turn the gate on before the verb
		run       func(ctx context.Context, e *liveEnv) error
		committed func(t *testing.T, e *liveEnv) bool
	}{
		{
			name: "revoke-key", gateFirst: true,
			run: func(ctx context.Context, e *liveEnv) error {
				return e.o.RevokeKey(ctx, "cp_tls", e.r.Current.D.CPTLSPins[0])
			},
			committed: func(t *testing.T, e *liveEnv) bool {
				_, ok := revokedKind(t, e.f.App, e.r.Current.D.CPTLSPins[0])
				return ok
			},
		},
		{
			name: "replication-gate on",
			run:  func(ctx context.Context, e *liveEnv) error { return e.o.ReplicationGate(ctx, true) },
			committed: func(t *testing.T, e *liveEnv) bool {
				return replicationRequired(t, e.f.App)
			},
		},
		{
			name: "end-drain", gateFirst: true,
			prepare: func(t *testing.T, e *liveEnv) { e.supersede(t) },
			run:     func(ctx context.Context, e *liveEnv) error { return e.o.EndDrain(ctx, 0, 1) },
			committed: func(t *testing.T, e *liveEnv) bool {
				return drainGrace(t, e.f.App, 1) == 0
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &fakeProbe{}
			e := newLiveEnv(t, OwnerConfig{Probe: p})
			if c.prepare != nil {
				c.prepare(t, e)
			}
			if c.gateFirst {
				requireGate(t, e.f, true)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if err := c.run(ctx, e); !errors.Is(err, ErrNotReplicated) {
				t.Fatalf("%s with a probe that never confirms = %v, want ErrNotReplicated", c.name, err)
			}
			if p.calls.Load() == 0 {
				t.Fatalf("%s never asked the replication probe", c.name)
			}
			if !c.committed(t, e) {
				t.Fatalf("%s did not commit before waiting on replication (D-225: committed locally, not replicated)", c.name)
			}
			p.ok.Store(true)
			if err := c.run(liveCtx(t), e); err != nil {
				t.Fatalf("the re-run once a standby confirms = %v, want nil (B3-D8)", err)
			}
		})
	}
}

// A prepare that returned nil may have committed (restore-begin's trust
// install does), so a transaction that fails after it still ends on finish:
// the error is the body's when finish confirms, and joined with
// ErrNotReplicated, an exit of replication, when it cannot. Without prepare a
// body error returns before finish, since nothing committed.
func TestWriteAfterEndsOnFinishOncePrepareRan(t *testing.T) {
	p := &fakeProbe{}
	e := newLiveEnv(t, OwnerConfig{Probe: p})
	requireGate(t, e.f, true)
	bodyErr := refused("test", "the body refuses")
	prepared := func(context.Context, *sql.Conn) error { return nil }
	failing := func(context.Context, *sql.Tx) error { return bodyErr }

	if err := e.o.writeAfter(liveCtx(t), "test", nil, failing); !errors.Is(err, bodyErr) || errors.Is(err, ErrNotReplicated) {
		t.Fatalf("no prepare, body refuses = %v, want the body's error alone", err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("with no prepare, finish ran after a body error that committed nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := e.o.writeAfter(ctx, "test", prepared, failing)
	if !errors.Is(err, bodyErr) || !errors.Is(err, ErrNotReplicated) {
		t.Fatalf("prepare ran, body refuses, no standby confirms = %v, want the body's error joined with ErrNotReplicated", err)
	}
	if p.calls.Load() == 0 {
		t.Fatal("prepare ran and the body failed, but finish never asked the replication probe")
	}

	p.ok.Store(true)
	if err := e.o.writeAfter(liveCtx(t), "test", prepared, failing); !errors.Is(err, bodyErr) || errors.Is(err, ErrNotReplicated) {
		t.Fatalf("prepare ran, body refuses, a standby confirms = %v, want the body's error alone", err)
	}
}

// An owner verb's transaction waits at most 2 s for a lock and may idle at
// most 10 s, and keeps the session's statement_timeout: reconcile-complete's
// statements grow with the realm, so they are bounded by the verb's deadline
// alone.
func TestOwnerTransactionTimeouts(t *testing.T) {
	e := newLiveEnv(t, OwnerConfig{})
	ctx := liveCtx(t)
	var session string
	if err := e.f.Owner.QueryRowContext(ctx, `SELECT current_setting('statement_timeout')`).Scan(&session); err != nil {
		t.Fatal(err)
	}
	var lock, idle, stmt string
	if err := e.o.write(ctx, "test", func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT current_setting('lock_timeout'),
			current_setting('idle_in_transaction_session_timeout'), current_setting('statement_timeout')`).
			Scan(&lock, &idle, &stmt)
	}); err != nil {
		t.Fatal(err)
	}
	if lock != "2s" || idle != "10s" || stmt != session {
		t.Fatalf("lock_timeout %s, idle_in_transaction_session_timeout %s, statement_timeout %s; want 2s, 10s and the session's %s",
			lock, idle, stmt, session)
	}
}

// The real probe, as the owner. Task 3 proved finish with fakes; this proves
// the production wiring: authority_owner can call authority.replicated (the
// function's grant list names only authority_app besides its owner).
//
// On a standalone node nothing is in pg_stat_replication, so the probe
// correctly answers false and finish correctly answers ErrNotReplicated at its
// deadline: replication_required=true on a single node can never confirm.
// finish keeps polling through a probe error (waitReplicated swallows it until
// ctx ends), so a permission error would also surface as ErrNotReplicated;
// the probe is therefore also called directly, where an error cannot hide.
func TestOwnerFinishWithTheRealProbeAsOwner(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	o := newTestOwner(t, f, r, OwnerConfig{Probe: SQLProbe{}})
	requireGate(t, f, true)

	conn, err := f.Owner.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	var lsn string
	if err := conn.QueryRowContext(liveCtx(t), `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn); err != nil {
		t.Fatal(err)
	}
	ok, err := SQLProbe{}.Replicated(liveCtx(t), conn, lsn)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			t.Fatalf("authority_owner cannot call authority.replicated: %v", err)
		}
		t.Fatalf("SQLProbe as authority_owner = %v, want a plain answer", err)
	}
	if ok {
		t.Fatal("SQLProbe confirmed replication on a standalone node")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = o.finish(ctx, conn)
	if !errors.Is(err, ErrNotReplicated) {
		t.Fatalf("finish with the real probe on a standalone node = %v, want ErrNotReplicated", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Fatalf("finish failed on a permission error rather than on replication: %v", err)
	}
}
