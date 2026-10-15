package store

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// fakeProbe answers a fixed value and counts its calls.
type fakeProbe struct {
	ok    atomic.Bool
	calls atomic.Int64
}

func (p *fakeProbe) Replicated(context.Context, *sql.Conn, string) (bool, error) {
	p.calls.Add(1)
	return p.ok.Load(), nil
}

// newTestStore is the store every integration test uses. It seeds meta and
// installs the realm's delegation. Statfs reports 1 TiB free.
func newTestStore(t *testing.T, f *authoritydb.Fixture, r *authoritytest.Realm, probe ReplicationProbe, hooks Hooks) *Store {
	t.Helper()
	f.SeedMeta(t, r.ID)
	r.Install(t, f.App, r.Current)
	if hooks.Statfs == nil {
		hooks.Statfs = func() (uint64, error) { return 1 << 40, nil }
	}
	s, err := Open(Config{DB: f.App, Signer: r.RSK, Anchors: r.AnchorBytes, Probe: probe, Hooks: hooks,
		TxDeadline: 2 * time.Second, ReadDeadline: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireGate(t *testing.T, f *authoritydb.Fixture, on bool) {
	t.Helper()
	mustExec(t, f.Owner, `UPDATE authority.meta SET replication_required = $1`, on)
}

func TestGateDropsTheBytesWhenReplicationIsNotConfirmed(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	p := &fakeProbe{}
	s := newTestStore(t, f, r, p, Hooks{})
	requireGate(t, f, true)
	conn, _ := f.App.Conn(context.Background())
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	signed := Outcome{HTTP: 200, Kind: KindCommitted, Body: []byte("SIGNED-BYTES")}
	out := s.release(ctx, conn, signed)
	if out.HTTP != 503 || out.Code != string(ac.ErrorReplication) {
		t.Fatalf("release with a false probe = %+v, want 503 replication", out)
	}
	if string(out.Body) == "SIGNED-BYTES" {
		t.Fatal("the gate released the signed bytes")
	}
	if p.calls.Load() < 2 {
		t.Fatalf("the probe was polled %d times in 150 ms, want it polled every RELEASE_POLL_MS", p.calls.Load())
	}
	if !s.nonClient503.Load() {
		t.Fatal("503 replication did not set the tick's non-client 503 leg (D-147, B1-D31)")
	}
}

func TestGateReleasesOnConfirmationAndIsSkippedWhenNotRequired(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	p := &fakeProbe{}
	s := newTestStore(t, f, r, p, Hooks{})
	conn, _ := f.App.Conn(context.Background())
	defer func() { _ = conn.Close() }()
	signed := Outcome{HTTP: 200, Kind: KindCommitted, Body: []byte("SIGNED")}
	if out := s.release(context.Background(), conn, signed); string(out.Body) != "SIGNED" || p.calls.Load() != 0 {
		t.Fatalf("replication_required = false: %+v after %d probes, want the bytes and no probe", out, p.calls.Load())
	}
	requireGate(t, f, true)
	p.ok.Store(true)
	if out := s.release(context.Background(), conn, signed); string(out.Body) != "SIGNED" {
		t.Fatalf("confirmed replication: %+v", out)
	}
	if s.nonClient503.Load() {
		t.Fatal("a released outcome set the non-client 503 leg")
	}
}

func TestReplicationGateFlipTakesEffectOnTheNextRequest(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	conn, _ := f.App.Conn(context.Background())
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	signed := Outcome{HTTP: 200, Kind: KindCommitted, Body: []byte("S")}
	if out := s.release(ctx, conn, signed); out.HTTP != 200 {
		t.Fatal("gate off: not released")
	}
	requireGate(t, f, true) // D-225: no restart, no cache
	if out := s.release(ctx, conn, signed); out.HTTP != 503 {
		t.Fatalf("gate flipped on: %+v, want 503 replication on the very next call", out)
	}
}

func TestErrorEnvelopesPassTheGateUntouched(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	requireGate(t, f, true)
	conn, _ := f.App.Conn(context.Background())
	defer func() { _ = conn.Close() }()
	in := errorOutcome(400, ac.ErrorMalformed)
	if out := s.release(context.Background(), conn, in); out.Code != string(ac.ErrorMalformed) {
		t.Fatalf("an unrecorded error went through the gate: %+v", out)
	}
}

func TestLeaderCheck(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	conn, _ := f.App.Conn(context.Background())
	defer func() { _ = conn.Close() }()
	primary := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	if _, ok := primary.leaderCheck(context.Background(), conn); !ok {
		t.Fatal("the primary test cluster reported in recovery")
	}
	standby, _ := Open(Config{DB: f.App, Signer: r.RSK, Anchors: r.AnchorBytes, Hooks: Hooks{
		Statfs:     func() (uint64, error) { return 1 << 40, nil },
		InRecovery: func(context.Context, *sql.Conn) (bool, error) { return true, nil },
	}})
	out, ok := standby.leaderCheck(context.Background(), conn)
	if ok || out.HTTP != 503 || out.Code != string(ac.ErrorNotLeader) {
		t.Fatalf("a session in recovery = %+v, want 503 not_leader", out)
	}
	broken, _ := Open(Config{DB: f.App, Signer: r.RSK, Anchors: r.AnchorBytes, Hooks: Hooks{
		Statfs:     func() (uint64, error) { return 1 << 40, nil },
		InRecovery: func(context.Context, *sql.Conn) (bool, error) { return false, errors.New("boom") },
	}})
	if out, ok := broken.leaderCheck(context.Background(), conn); ok || out.HTTP != 503 {
		t.Fatalf("an unreadable recovery state = %+v, want a 503 (fail closed)", out)
	}
}
