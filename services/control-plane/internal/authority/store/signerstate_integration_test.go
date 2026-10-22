package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
	"github.com/google/uuid"
)

func nodeFill(m *ac.Signer) {
	m.Role = "leader"
	m.DiskReady, m.ClockSynced = 1, 1
	m.SubmitLayouts = []string{"submit/1"}
}

func signerOf(t *testing.T, s *Store) ac.Signer {
	t.Helper()
	out := s.Signer(context.Background(), nodeFill)
	if out.HTTP != 200 {
		t.Fatalf("signer: %d %s", out.HTTP, out.Code)
	}
	m, err := ac.DecodeSigner(out.Body)
	if err != nil {
		t.Fatalf("the body does not decode as `signer`: %v", err)
	}
	return m
}

func TestSignerReportsTheDatabaseFields(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	m := signerOf(t, s)
	switch {
	case m.RealmID != r.ID, m.ProductRootGen != 0, m.DelegationSerial != 1, m.RestoreEpoch != 0,
		m.Reconciled != 1, m.MaxCommitID != 0, m.HeldCount != 0, m.LegacySealed != 0, len(m.PopulationDigest) != 0:
		t.Fatalf("meta fields: %+v", m)
	case m.ExtensionCapMs != ExtensionCapMs, m.NCancelMs != NCancelMs, m.VMs != VMs:
		t.Fatalf("constants: %d %d %d", m.ExtensionCapMs, m.NCancelMs, m.VMs)
	case !bytes.Equal(m.RSKDigest, ac.KeyDigest(r.RSK.PublicKey())):
		t.Fatal("rsk_digest is not this node's RSK")
	case m.ReplicationRequired != 0, m.ReplicationReady != 0: // SeedMeta turns the gate off
		t.Fatalf("replication: %d %d", m.ReplicationRequired, m.ReplicationReady)
	case m.Role != "leader", len(m.SubmitLayouts) == 0 || m.SubmitLayouts[0] != "submit/1":
		t.Fatal("fill was not applied")
	}
	env, err := ac.CheckTrustEnvelope(m.Trust)
	if err != nil || len(env.Items) != 1 || !bytes.Equal(env.Items[0], r.Current.Signed) {
		t.Fatalf("trust: %v, %d items", err, len(env.Items))
	}
}

// max_commit_id is the identity sequence's last value (DoR:529), read by
// authority_app through its USAGE grant (000001:448).
func TestSignerMaxCommitIDIsTheSequence(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	mustExec(t, f.Owner, `SELECT nextval(pg_get_serial_sequence('authority.account_events', 'commit_id'))`)
	if m := signerOf(t, s); m.MaxCommitID != 1 {
		t.Fatalf("max_commit_id %d, want 1", m.MaxCommitID)
	}
}

func TestSignerReplicationTickAgeAndSeal(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	digest := bytes.Repeat([]byte{9}, 48)
	mustExec(t, f.Owner, `UPDATE authority.meta SET replication_required = true,
		last_tick_at = clock_timestamp() - interval '95 seconds', legacy_sealed_at = clock_timestamp(),
		population_digest = $1, cancel_unavailable_ms = 123456`, digest)
	m := signerOf(t, s)
	if m.ReplicationRequired != 1 || m.ReplicationReady != 0 { // CI has no synchronous standby
		t.Fatalf("replication: %d %d", m.ReplicationRequired, m.ReplicationReady)
	}
	if m.TickAgeS < 95 || m.TickAgeS > 120 {
		t.Fatalf("tick_age_s %d, want about 95", m.TickAgeS)
	}
	if m.LegacySealed != 1 || !bytes.Equal(m.PopulationDigest, digest) {
		t.Fatalf("seal: %d %x", m.LegacySealed, m.PopulationDigest)
	}
	if m.CancelUnavailableMs != 123456 {
		t.Fatalf("cancel_unavailable_ms %d, want 123456", m.CancelUnavailableMs)
	}
}

func TestSignerWithNoDelegationIs503Signer(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	out := bareStore(t, f, r, r.ID).Signer(context.Background(), nodeFill)
	if out.HTTP != 503 || out.Code != string(ac.ErrorSigner) {
		t.Fatalf("%d %s, want 503 signer", out.HTTP, out.Code)
	}
}

// A body that does not encode (here an out-of-range role) is never released.
func TestSignerThatDoesNotEncodeIs503Signer(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	out := s.Signer(context.Background(), func(m *ac.Signer) { nodeFill(m); m.Role = "primary" })
	if out.HTTP != 503 || out.Code != string(ac.ErrorSigner) {
		t.Fatalf("%d %s, want 503 signer", out.HTTP, out.Code)
	}
}

// /v1/signer answers below the hard disk floor and with every read slot
// taken: it is the only channel out of a node (B2-D4).
func TestSignerSkipsTheDiskFloorAndTheReadSlots(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{Statfs: func() (uint64, error) { return 0, nil }})
	for i := 0; i < ReadSlots; i++ {
		if _, ok := s.adm.acquireRead(); !ok {
			t.Fatal("could not take a read slot")
		}
	}
	signerOf(t, s)
}

// held_count counts the held heads and only those: the brief's other tests see
// zero, which a constant would also satisfy.
func TestSignerCountsOnlyHeldHeads(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	held, other := uuid.NewString(), uuid.NewString()
	insertAbsentHead(t, f.App, held)
	insertAbsentHead(t, f.App, other)
	mustExec(t, f.Owner, `UPDATE authority.account_heads SET held = true WHERE account_id = $1`, held)
	if m := signerOf(t, s); m.HeldCount != 1 {
		t.Fatalf("held_count %d, want 1", m.HeldCount)
	}
}

// Signer bounds its own context by the read deadline, as every store read does,
// so a caller whose context carries none still gets one. Hooks.DBNow receives
// the context Signer is working under.
func TestSignerAppliesTheReadDeadlineToAParentWithNone(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	var left time.Duration
	var seen bool
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{DBNow: func(ctx context.Context) (uint64, error) {
		if dl, ok := ctx.Deadline(); ok {
			left, seen = time.Until(dl), true
		}
		return uint64(time.Now().UnixMilli()), nil
	}})
	left, seen = 0, false
	if out := s.Signer(context.Background(), nodeFill); out.HTTP != 200 {
		t.Fatalf("signer: %d %s", out.HTTP, out.Code)
	}
	if !seen {
		t.Fatal("Signer ran under a context with no deadline")
	}
	if left <= s.readDL-time.Second || left > s.readDL {
		t.Fatalf("deadline in %v, want within a second below the read deadline %v", left, s.readDL)
	}
}

// Deferred #11: installedTrust emits every succession before any delegation
// (DoR:502), whatever order the rows were inserted in. newTestStore installs
// the delegation first; the succession goes in after it.
func TestInstalledTrustPutsSuccessionsFirst(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	succ, _, _ := signedSuccession(t, r)
	mustExec(t, f.App, `INSERT INTO authority.successions (prev_root_gen, bytes, sig_a, sig_b) VALUES (0, $1, $2, $3)`,
		succ.Msg, succ.SigA, succ.SigB)
	b, err := installedTrust(context.Background(), f.App)
	if err != nil {
		t.Fatal(err)
	}
	env, err := ac.DecodeTrustEnvelope(b)
	if err != nil || len(env.Items) != 2 {
		t.Fatalf("the envelope: %d items, %v; want 2", len(env.Items), err)
	}
	want := []ac.Signed{succ, {Type: "delegation", Msg: r.Current.Msg, SigA: r.Current.SigA, SigB: r.Current.SigB}}
	for i, it := range env.Items {
		if !bytes.Equal(it, want[i].Encode()) {
			t.Fatalf("item %d is not the %s", i, want[i].Type)
		}
	}
}

// Deferred #11: more than TrustItems installed rows cannot encode, and
// installedTrust refuses with errSigner (503 signer) rather than emit an empty
// or truncated trust. TrustItems rows, the realm's delegation included, still
// encode. The filler rows are never verified here: installedTrust re-emits.
func TestInstalledTrustRefusesMoreThanTrustItems(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	fill := func(from, to int) {
		mustExec(t, f.Super, `INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b, digest)
			SELECT 1, g, $1::bytea || int4send(g), $2, $3, sha384($1::bytea || int4send(g)) FROM generate_series($4::int, $5::int) g`,
			r.Current.Msg, r.Current.SigA, r.Current.SigB, from, to)
	}
	fill(1, ac.TrustItems-1)
	if _, err := installedTrust(context.Background(), f.App); err != nil {
		t.Fatalf("control: %d rows: %v", ac.TrustItems, err)
	}
	fill(ac.TrustItems, ac.TrustItems)
	if _, err := installedTrust(context.Background(), f.App); !errors.Is(err, errSigner) {
		t.Fatalf("%d rows: %v, want errSigner", ac.TrustItems+1, err)
	}
}

// Deferred #11: replication_ready is replication_required AND a confirmed
// synchronous standby (DoR:1453). The test cluster has no standby, so a
// transaction that never commits replaces authority.replication_state() to
// report one, and signerState reads through it: with the gate off the field
// stays 0, and with the gate on it is 1. The rollback restores the function.
func TestReplicationReadyNeedsTheGate(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{}) // SeedMeta: replication_required = false
	ctx := context.Background()
	tx, err := f.Super.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE FUNCTION authority.replication_state() RETURNS integer
		LANGUAGE sql STABLE AS $$ SELECT 1 $$`); err != nil {
		t.Fatal(err)
	}
	m, err := s.signerState(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReplicationRequired != 0 || m.ReplicationReady != 0 {
		t.Fatalf("gate off, one standby: required %d, ready %d; want 0 and 0", m.ReplicationRequired, m.ReplicationReady)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica;
		UPDATE authority.meta SET replication_required = true WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if m, err = s.signerState(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if m.ReplicationRequired != 1 || m.ReplicationReady != 1 {
		t.Fatalf("control, gate on, one standby: required %d, ready %d; want 1 and 1", m.ReplicationRequired, m.ReplicationReady)
	}
}
