package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// chainHeadOf (import.go): the chain head an account's stored history
// rebuilds through ac.ReplayAuthorityBundle (B3-D15 R3). These fold several
// events, fold the two histories a client-form VerifyBundle fold cannot (a
// legacy-population account's pinned first result, and entries a renewal
// superseded), and break the history four ways. The import tests fold through
// the route.

// historyOf commits a four-event history: enroll_new, then suspend,
// unsuspend, suspend, all under the operator. It returns the account and the
// stored bundle of each seq (index 0 is seq 1).
func historyOf(t *testing.T, e *env) (*authoritytest.Account, [][]byte) {
	t.Helper()
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
	e.commitOK(a, a.Unsuspend(t, ac.ActorOperator).Bytes)
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
	var bundles [][]byte
	for seq := 1; seq <= 4; seq++ {
		bundles = append(bundles, eventBundle(t, e, a.ID, seq))
	}
	return a, bundles
}

// rebuild reads the account's head row under L1 (lockHead, so a seq-0 head of
// a population account takes its pin, as import's does) and the store's
// trust, and calls chainHeadOf as import does.
func rebuild(t *testing.T, e *env, account string) (ac.ChainHead, *ac.Trust, *headRow, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.f.App.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	h, _, err := lockHead(ctx, tx, account)
	if err != nil {
		t.Fatal(err)
	}
	tr, _, err := e.s.trustFor(ctx, e.f.App)
	if err != nil {
		t.Fatal(err)
	}
	head, err := chainHeadOf(ctx, tx, tr, tr.HighWater(), h)
	return head, tr, h, err
}

// rewriteHistory runs q against the account's events with the append-only
// guards off, as the restored fixture does.
func rewriteHistory(t *testing.T, e *env, q string, args ...any) {
	t.Helper()
	tx, err := e.f.Super.Begin()
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

func TestChainHeadOfRebuildsAMultiEventHistory(t *testing.T) {
	e := newEnv(t)
	a, bundles := historyOf(t, e)

	got, tr, h, err := rebuild(t, e, a.ID)
	if err != nil {
		t.Fatalf("chainHeadOf over four stored events = %v, want nil", err)
	}
	res, err := ac.DecodeResult(got.HeadResult)
	if err != nil {
		t.Fatal(err)
	}
	if res.Seq != 4 || h.Seq != 4 || !bytes.Equal(ac.EntryDigest(got.HeadResult), h.Entry) {
		t.Fatalf("the rebuilt head is at seq %d, the head row at %d; want both 4 with the row's entry", res.Seq, h.Seq)
	}

	// The head rebuilt at seq 3 is the one the seq-4 bundle links to: drop
	// seq 4 and move the head row back, as a restore would.
	rewriteHistory(t, e, `DELETE FROM authority.account_events WHERE account_id = $1 AND seq = 4`, a.ID)
	rewriteHistory(t, e, `UPDATE authority.account_heads SET seq = 3, head_entry_digest = (SELECT entry_digest
		FROM authority.account_events WHERE account_id = $1 AND seq = 3) WHERE account_id = $1`, a.ID)
	at3, _, _, err := rebuild(t, e, a.ID)
	if err != nil {
		t.Fatalf("chainHeadOf over three stored events = %v, want nil", err)
	}
	if _, r := ac.VerifyBundle(tr, tr.HighWater(), at3, bundles[3], nil); r != ac.ReasonOK {
		t.Fatalf("the seq-4 bundle against the rebuilt seq-3 head = %s, want ok", r)
	}
}

func TestChainHeadOfRefusesAnUnprovenHistory(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(t *testing.T, e *env, account string, bundles [][]byte)
	}{
		{"a stored bundle whose result signature does not verify", func(t *testing.T, e *env, account string, bundles [][]byte) {
			b, err := ac.DecodeBundle(bundles[1])
			if err != nil {
				t.Fatal(err)
			}
			b.ResultSig = bytes.Clone(b.ResultSig)
			b.ResultSig[len(b.ResultSig)-1] ^= 0x01
			forged := b.Encode()
			if forged == nil {
				t.Fatal("the forged bundle does not encode")
			}
			rewriteHistory(t, e, `UPDATE authority.account_events SET bundle = $2 WHERE account_id = $1 AND seq = 2`,
				account, forged)
		}},
		{"a gap in the stored history", func(t *testing.T, e *env, account string, _ [][]byte) {
			rewriteHistory(t, e, `DELETE FROM authority.account_events WHERE account_id = $1 AND seq = 2`, account)
		}},
		{"a head ahead of its stored history", func(t *testing.T, e *env, account string, _ [][]byte) {
			rewriteHistory(t, e, `DELETE FROM authority.account_events WHERE account_id = $1 AND seq = 4`, account)
		}},
		{"a head row whose pin its first result does not name", func(t *testing.T, e *env, account string, _ [][]byte) {
			// The fold reads the seq-0 pin from the head row: a pin the account
			// never had must not explain its first result.
			rewriteHistory(t, e, `UPDATE authority.account_heads SET legacy_ek_digest = $2 WHERE account_id = $1`,
				account, bytes.Repeat([]byte{0x5a}, 48))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			a, bundles := historyOf(t, e)
			if _, _, _, err := rebuild(t, e, a.ID); err != nil {
				t.Fatalf("the intact history = %v, want nil (the control)", err)
			}
			c.breakIt(t, e, a.ID, bundles)
			_, _, _, err := rebuild(t, e, a.ID)
			if !errors.Is(err, errHistoryUnproven) {
				t.Fatalf("chainHeadOf = %v, want errHistoryUnproven", err)
			}
		})
	}
}

// A legacy-population account's seq 1 is a PoP enroll_existing that only its
// pin explains (V3-05): the client-form fold refuses it, the authority-form fold
// takes the pin from the head row and rebuilds the whole history.
func TestChainHeadOfRebuildsAPinnedHistory(t *testing.T) {
	e, accts := newEnvPop(t, 1)
	a := accts[0]
	e.commitOK(a, a.EnrollExisting(t, true, ac.FactorPassword).Bytes) // seq 1, the pin's PoP
	e.commitOK(a, a.CancelByPoP(t).Bytes)                             // seq 2

	got, tr, h, err := rebuild(t, e, a.ID)
	if err != nil {
		t.Fatalf("chainHeadOf over a pinned history = %v, want nil", err)
	}
	if h.Seq != 2 || !bytes.Equal(ac.EntryDigest(got.HeadResult), h.Entry) {
		t.Fatalf("the rebuilt head does not describe the seq-%d head row", h.Seq)
	}
	if len(h.Pin) == 0 {
		t.Fatal("fixture: the head row carries no pin")
	}
	genesis := ac.GenesisHead(h.RealmID, a.ID)
	if _, r := ac.VerifyBundle(tr, tr.HighWater(), genesis, eventBundle(t, e, a.ID, 1), nil); r == ac.ReasonOK {
		t.Fatal("control: the client-form VerifyBundle accepted the pinned first result with no pin")
	}
}

// T11 sets min_valid_serial at each renewal, so every entry under the old
// serial is superseded once the next is installed: the client-form fold stops
// at seq 1, the authority-form fold admits them and links seq 3 under s + 1.
func TestChainHeadOfRebuildsAcrossARenewal(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()                            // seq 1 under s
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // seq 2 under s
	e.r.Install(t, e.f.App, e.r.NextDelegation(t))
	e.commitOK(a, a.Unsuspend(t, ac.ActorOperator).Bytes) // seq 3 under s + 1

	got, tr, h, err := rebuild(t, e, a.ID)
	if err != nil {
		t.Fatalf("chainHeadOf across a renewal = %v, want nil", err)
	}
	if h.Seq != 3 || !bytes.Equal(ac.EntryDigest(got.HeadResult), h.Entry) {
		t.Fatalf("the rebuilt head does not describe the seq-%d head row", h.Seq)
	}
	genesis := ac.GenesisHead(h.RealmID, a.ID)
	if _, r := ac.VerifyBundle(tr, tr.HighWater(), genesis, eventBundle(t, e, a.ID, 1), nil); r != ac.ReasonSupersededUncovered {
		t.Fatalf("control: the client-form VerifyBundle of seq 1 after the renewal = %s, want superseded_uncovered", r)
	}
}
