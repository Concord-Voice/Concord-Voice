package store

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func countRows(t *testing.T, e *env, table string) int {
	t.Helper()
	var n int
	if err := e.f.Super.QueryRow(`SELECT count(*) FROM authority.` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC9 end to end: the stored bundle of a fresh commit verifies under A2's
// client verifier from genesis.
func TestEnrollNewCommitsABundleClientsAccept(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	var bundle []byte
	if err := e.f.App.QueryRow(`SELECT bundle FROM authority.account_events WHERE account_id = $1 AND seq = 1`, a.ID).Scan(&bundle); err != nil {
		t.Fatal(err)
	}
	tr := e.r.Trust(t, e.r.Current)
	if _, r := ac.VerifyBundle(tr, tr.HighWater(), ac.GenesisHead(e.r.ID, a.ID), bundle, nil); r != ac.ReasonOK {
		t.Fatalf("the authority's own enroll_new bundle verifies as %s", r)
	}
	if a.Head.State != ac.StateActive || a.Head.Seq != 1 || a.Head.Dseq != 1 {
		t.Fatalf("head after enroll_new: %+v", a.Head)
	}
}

// relayWithFreshAuthz re-sends b's intent under a new cp-authz (asserted + 1,
// fresh evidence salt), signed by the CAK: what a CP re-relay produces.
func relayWithFreshAuthz(t *testing.T, e *env, b authoritytest.Built) []byte {
	t.Helper()
	sub, ev, az := b.Parts.Submit, b.Parts.Evidence, b.Parts.CPAuthz
	ev.EvidenceSalt = bytes.Repeat([]byte{9}, 32)
	sub.CPEvidence = ev.Encode()
	az.AssertedAtMs++
	az.EvidenceDigest = ac.Digest(sub.CPEvidence)
	sub.CPAuthz = az.Encode()
	sub.CPAuthzSig = authoritytest.SignRaw(t, e.r.CAK, sub.CPAuthz)
	return sub.Encode()
}

func TestIdenticalReplayReRelayAndConflict(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	b := a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: authoritytest.NewDevice(t),
		Factors: []string{ac.FactorRegistration}})
	first := e.submit(b.Bytes)
	if again := e.submit(b.Bytes); !bytes.Equal(again.Body, first.Body) || again.Kind != KindCommitted {
		t.Fatalf("identical replay = %+v, want the stored bytes", again)
	}
	if out := e.submit(relayWithFreshAuthz(t, e, b)); !bytes.Equal(out.Body, first.Body) {
		t.Fatalf("re-relay with a fresh cp-authz (D-151) = %+v, want the stored result", out)
	}
	conflict := a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, RequestID: b.Parts.Intent.RequestID,
		Factors: []string{ac.FactorRegistration}})
	e.errorAs(conflict.Bytes, 409, ac.ErrorRequestConflict)
	if countRows(t, e, "account_events") != 1 {
		t.Fatal("a replay or a conflict wrote a second event")
	}
}

func TestConcurrentFirstEnrollmentOneCommitsOneWrongState(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	b1, b2 := a.EnrollNew(t), a.EnrollNew(t) // two different request_ids, same account, both from A0
	outs := make([]Outcome, 2)
	var wg sync.WaitGroup
	for i, b := range [][]byte{b1.Bytes, b2.Bytes} {
		wg.Add(1)
		go func(i int, body []byte) { defer wg.Done(); outs[i] = e.submit(body) }(i, b)
	}
	wg.Wait()
	kinds := map[Kind]int{}
	for _, o := range outs {
		kinds[o.Kind]++
	}
	if kinds[KindCommitted] != 1 || kinds[KindRefused] != 1 {
		t.Fatalf("racing first enrollments: %+v, want one committed and one refused", outs)
	}
	for _, o := range outs {
		if o.Kind == KindRefused && o.Code != string(ac.RefusalWrongState) {
			t.Fatalf("the loser was refused %s, want wrong_state (§3.5)", o.Code)
		}
	}
}

// Review Focus 1.
func TestConcurrentIdenticalRetryReturnsStoredOutcome(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	b := a.EnrollNew(t)
	outs := make([]Outcome, 2)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); outs[i] = e.submit(b.Bytes) }(i)
	}
	wg.Wait()
	if outs[0].Kind != KindCommitted || !bytes.Equal(outs[0].Body, outs[1].Body) {
		t.Fatalf("two identical retries in flight: %+v / %+v, want the same committed bytes", outs[0], outs[1])
	}
	if countRows(t, e, "account_events") != 1 {
		t.Fatal("two identical retries wrote two events")
	}
}

// Review round 3 (B1-D19): a retry that missed the pre-BEGIN lookup and waited
// on L1 gets the stored outcome under L1 even when no delegation covers db_now
// any more. A replay sends bytes already signed (DoR step 2: a hit goes to step 12).
func TestLockedReplayNeedsNoSigner(t *testing.T) {
	e := newEnv(t)
	b := e.r.NewAccount(t).EnrollNew(t)
	first := e.submit(b.Bytes)
	if first.Kind != KindCommitted {
		t.Fatalf("first submit = %+v", first)
	}
	e.now.Store(e.r.Current.D.NotAfterMs) // no installed delegation covers db_now now
	rq, out, ok := decodeSubmit(b.Bytes)
	if !ok {
		t.Fatalf("decode: %+v", out)
	}
	conn, err := e.f.App.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// commit skips the pre-BEGIN lookup, as for a retry that ran it before the first commit landed.
	if again := e.s.commit(context.Background(), conn, rq); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("locked replay with no usable signer = %+v, want the stored outcome", again)
	}
}

// B1-D33 (review round 4): while unreconciled, /v1/transactions answers 503
// reconcile (DoR:1713), a replay included. preBegin refuses before the replay
// lookup; the transaction checks again after L1, for a request that passed
// preBegin before enter_unreconciled() ran.
func TestTransactionsRefuseWhileUnreconciled(t *testing.T) {
	e := newEnv(t)
	b := e.r.NewAccount(t).EnrollNew(t)
	if first := e.submit(b.Bytes); first.Kind != KindCommitted {
		t.Fatalf("first submit = %+v", first)
	}
	next := e.r.NewAccount(t).EnrollNew(t)
	rq, out, ok := decodeSubmit(next.Bytes)
	if !ok {
		t.Fatalf("decode: %+v", out)
	}
	conn, err := e.f.App.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if out, proceed := e.s.preBegin(context.Background(), conn, rq); !proceed {
		t.Fatalf("preBegin while reconciled = %+v", out)
	}
	mustExec(t, e.f.Owner, `SELECT authority.enter_unreconciled()`)
	if out := e.s.commit(context.Background(), conn, rq); out.HTTP != 503 || out.Code != string(ac.ErrorReconcile) {
		t.Fatalf("a request that passed preBegin before the flip = %+v, want 503 reconcile", out)
	}
	for name, raw := range map[string][]byte{"a new request": next.Bytes, "a replay": b.Bytes} {
		if out := e.submit(raw); out.HTTP != 503 || out.Code != string(ac.ErrorReconcile) {
			t.Fatalf("%s while unreconciled = %+v, want 503 reconcile", name, out)
		}
	}
	var n int
	if err := e.f.App.QueryRow(`SELECT count(*) FROM authority.account_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("account_events = %d (%v), want only the first commit", n, err)
	}
}

// B1-D34 (review round 4): T8 needs the CAK's delegation to share the result's
// generation, with a serial no higher (DoR:722). checkCAK ran before BEGIN; a
// succession installed since moves st.sig, not rq.CAK. The realm fixture builds
// no successions, so the signer is set by hand; the first case is the control.
func TestCAKMustMatchTheSignersGenerationInsideTheTransaction(t *testing.T) {
	e := newEnv(t)
	cur := e.r.Current.D
	cak := cakMatch{Gen: cur.ProductRootGen, Serial: cur.Serial, Key: cur.CAKPublicKeys[0]}
	for _, c := range []struct {
		name string
		sig  sigDelegation
		want bool
	}{
		{"the generation and serial checkCAK saw", sigDelegation{Gen: cak.Gen, Serial: cak.Serial}, true},
		{"a succession since checkCAK", sigDelegation{Gen: cak.Gen + 1, Serial: 1}, false},
		{"a signer below the CAK's serial", sigDelegation{Gen: cak.Gen, Serial: cak.Serial - 1}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx, err := e.f.App.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			st := &txState{tx: tx, now: e.now.Load(), sig: &c.sig,
				rq: &request{CAK: cak, AZ: ac.CPAuthz{Actor: ac.ActorUser}}}
			if ok, err := e.s.drainAndRevocation(context.Background(), st); err != nil || ok != c.want {
				t.Fatalf("drainAndRevocation = %v, %v; want %v", ok, err, c.want)
			}
		})
	}
}

// B1-D9 (review round 4): cp_authz and device-result #7 ride only on a private
// op; an attached entry relies on its published bundle's (DoR:446, DoR:509).
// A2's authzConsistent refuses either one on an attached entry.
func TestAnAttachedDeviceEntryCarriesNoCPAuthz(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith() // enroll_new with a device part attaches dseq 1
	var raw []byte
	if err := e.f.App.QueryRow(`SELECT bundle FROM authority.device_events WHERE account_id = $1 AND dseq = 1`, a.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	db, err := ac.DecodeDeviceBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := ac.DecodeDeviceResult(db.DeviceResult)
	if err != nil {
		t.Fatal(err)
	}
	if len(db.CPAuthz) != 0 || len(db.CPAuthzSig) != 0 || len(dr.CPAuthzDigest) != 0 {
		t.Fatalf("attached entry: cp_authz %d B, cp_authz_sig %d B, #7 %d B; want all empty",
			len(db.CPAuthz), len(db.CPAuthzSig), len(dr.CPAuthzDigest))
	}
}

func TestReplayAfterAnOwnerLedgerDeleteReturnsTheStoredCommit(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	b := a.EnrollNew(t)
	first := e.submit(b.Bytes)
	// any ledger row: a is not synced, so this suspend names the pre-enroll head (stale_seq);
	// synced, row 8 would commit it from active
	e.refusedAs(a.Suspend(t, ac.ActorOperator).Bytes, ac.RefusalStaleSeq)
	mustExec(t, e.f.Owner, `DELETE FROM authority.request_ledger`)
	if again := e.submit(b.Bytes); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("replay after a ledger DELETE = %+v, want the stored commit (events are looked up first)", again)
	}
}

// D-184: committed, response lost, end-drain (drain_grace_ms = 0 on the old
// delegation as owner, B1-D5), identical re-POST → the stored result.
func TestD184ReplayConvergesAfterTheCAKIsDrained(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	b := a.EnrollNew(t)
	first := e.submit(b.Bytes)
	next := e.r.NextDelegation(t, func(d *ac.Delegation) { d.CAKPublicKeys = [][]byte{authoritytest.Point(authoritytest.NewRealm(t).CAK)} })
	e.r.Install(t, e.f.App, next)
	mustExec(t, e.f.Owner, `UPDATE authority.delegations SET drain_grace_ms = 0 WHERE serial = 1`)
	if again := e.submit(b.Bytes); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("identical re-POST after end-drain = %+v, want the stored result, not 400 bad_authz", again)
	}
}

// Review Focus 3.
func TestStep9RefusesWithoutUsableDelegation(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	unlisted := e.r.NextDelegation(t, authoritytest.WithRSKs(authoritytest.Point(authoritytest.NewRealm(t).CAK)))
	e.r.Install(t, e.f.App, unlisted)
	before := countRows(t, e, "account_events") + countRows(t, e, "request_ledger")
	e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 503, ac.ErrorSigner)
	e.advance(200 * 86_400_000) // past every window: no delegation covers db_now
	e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 503, ac.ErrorSigner)
	if after := countRows(t, e, "account_events") + countRows(t, e, "request_ledger"); after != before {
		t.Fatalf("a 503 signer wrote %d rows", after-before)
	}
}

// Review Focus 4: D-36.
func TestClockBehindHeadIs503AndReplayStillWorks(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	enroll := a.EnrollNew(t)
	first := e.submit(enroll.Bytes)
	a.Sync(t, e.f.App)
	a.Activated()
	e.now.Store(e.now.Load() - 60_000) // the new leader's clock is behind the head's issued_at
	e.r.Clock.Ms = e.now.Load()
	e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 503, ac.ErrorClock)
	if again := e.submit(enroll.Bytes); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("replay with the clock behind = %+v, want the stored commit", again)
	}
}

type failingSigner struct{ authoritytestSigner }
type authoritytestSigner interface {
	PublicKey() []byte
	Sign([]byte) ([]byte, error)
}

func (f failingSigner) Sign([]byte) ([]byte, error) { return nil, errors.New("hsm unavailable") }

func TestSignerErrorIs503WithZeroRows(t *testing.T) {
	e := newEnv(t)
	e.s.sig = failingSigner{e.r.RSK}
	a := e.r.NewAccount(t)
	e.errorAs(a.EnrollNew(t).Bytes, 503, ac.ErrorSigner)
	if countRows(t, e, "account_events")+countRows(t, e, "key_bindings")+countRows(t, e, "account_heads") != 0 {
		t.Fatal("a signer failure left rows behind (step 11 must roll back everything, including L1's head insert)")
	}
}

func TestStaleHeadIsRecorded(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	b := a.Build(t, authoritytest.Spec{Op: ac.OpSuspend, Factors: []string{ac.FactorPassword}, PasswordLogin: true,
		Mutate: func(p *authoritytest.Parts) { p.Intent.ExpectedHeadDigest = ac.Digest([]byte("not the head")) }})
	e.refusedAs(b.Bytes, ac.RefusalStaleHead)
}

// Review round 2: the decode runs under a general slot, and a body that fails
// to decode gives it back. With one general slot free, a malformed body takes
// it; if it kept it, the next ordinary op would be 503 busy.
func TestAMalformedBodyGivesItsGeneralSlotBack(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	for i := 0; i < MutatingSlots-2; i++ {
		if _, ok := e.s.adm.acquireMutating(classOther); !ok {
			t.Fatal("could not hold a general slot")
		}
	}
	e.errorAs([]byte("not a submit"), 400, ac.ErrorMalformed)
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
}

// The cancel grace: a root cancel asserted before effective commits until
// visible. At visible it meets the activated head, where no row admits a
// cancel (wrong_state). A replace within N_cancel of a cancel is D (row 11).
func TestCancelGraceAndActivationBoundary(t *testing.T) {
	e := newEnv(t)
	g, delta := e.r.Current.D.GMs, e.r.Current.D.DeltaMs
	a, _ := e.enrolledWith()
	asserted := e.now.Load()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.now.Store(asserted + delta + g - 1) // visible − 1
	e.r.Clock.Ms = asserted + delta - 1   // the cancel is asserted before effective
	e.commitOK(a, a.CancelByRoot(t).Bytes)
	if a.Head.State != ac.StateActive {
		t.Fatalf("a cancel within the grace: head %s, want active", a.Head.State)
	}
	e.r.Clock.Ms = e.now.Load()
	if e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes); a.Head.State != ac.StateDisputed {
		t.Fatalf("a replace within N_cancel of a cancel: %s, want disputed", a.Head.State)
	}
	b, _ := e.enrolledWith()
	asserted = e.now.Load()
	e.r.Clock.Ms = asserted
	e.commitOK(b, b.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.now.Store(asserted + delta + g) // exactly visible
	e.r.Clock.Ms = asserted + delta + g - 1
	e.refusedAs(b.CancelByRoot(t).Bytes, ac.RefusalWrongState)
}
