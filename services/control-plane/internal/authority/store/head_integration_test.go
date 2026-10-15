package store

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

func inTx(t *testing.T, db *sql.DB, fn func(tx *sql.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestLockHeadCreatesAnAbsentHeadAndTakesThePinFromThePopulation(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	mustExec(t, f.App, `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`, testAcct, d48(7))
	inTx(t, f.App, func(tx *sql.Tx) {
		h, inPop, err := lockHead(context.Background(), tx, testAcct)
		if err != nil {
			t.Fatal(err)
		}
		if !inPop || h.State != ac.StateAbsent || h.Seq != 0 || !bytes.Equal(h.Pin, d48(7)) {
			t.Fatalf("head = %+v, inPop %v", h, inPop)
		}
		if h.published().FromCode() != ac.FromAp {
			t.Fatalf("a population account's genesis head classifies as %s, want Ap", h.published().FromCode())
		}
	})
}

func TestSaveHeadRoundTripsEveryColumn(t *testing.T) {
	f := authoritydb.Setup(t)
	f.SeedMeta(t, testRealm)
	var want headRow
	inTx(t, f.App, func(tx *sql.Tx) {
		h, _, err := lockHead(context.Background(), tx, testAcct)
		if err != nil {
			t.Fatal(err)
		}
		h.State, h.Prior, h.Seq, h.Entry, h.IssuedAtMs, h.RG = ac.StatePending, ac.StateActive, 2, d48(1), 50, 1
		h.Root, h.EK, h.EKGen, h.PendRoot, h.PendEK, h.PendEKGen = p97(1), d48(2), 1, p97(2), d48(3), 2
		h.ActBase, h.ActG, h.ActUnavailBase, h.ActFloor = u64p(100), u64p(10), u64p(0), u64p(7)
		h.Segment, h.SegmentRoot, h.PendEpoch = ac.AssuranceInitial, p97(1), d48(4)
		h.Dseq, h.DeviceHead, h.DeviceSet, h.DeviceSetDigest = 1, d48(5), []byte("set"), d48(6)
		h.LastCancelAtMs = 1_700_000_000_123
		if err := saveHead(context.Background(), tx, h); err != nil {
			t.Fatal(err)
		}
		want = *h
	})
	inTx(t, f.App, func(tx *sql.Tx) {
		got, _, err := lockHead(context.Background(), tx, testAcct)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != want.State || got.Seq != want.Seq || !bytes.Equal(got.PendRoot, want.PendRoot) ||
			*got.ActFloor != 7 || got.LastCancelAtMs != want.LastCancelAtMs || got.Segment != want.Segment ||
			!bytes.Equal(got.DeviceSet, want.DeviceSet) || got.PendEKGen != 2 {
			t.Fatalf("round trip lost a column:\n got  %+v\n want %+v", *got, want)
		}
		if got.published().ANB != 110 {
			t.Fatalf("published ANB = %d, want base + g = 110 (R2)", got.published().ANB)
		}
	})
}
