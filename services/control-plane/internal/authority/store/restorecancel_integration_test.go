package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func TestRestoreCancelCommitsRow22OnAHeldPendingHead(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	pendingEntry := a.Head.Entry
	if _, err := e.s.commitRestoreCancel(context.Background(), a.ID); !errors.Is(err, ErrNotHeldPendingOrDisputed) {
		t.Fatalf("row 22 on an unheld head: %v, want ErrNotHeldPendingOrDisputed", err)
	}
	e.pinHeld(a.ID)
	out, err := e.s.commitRestoreCancel(context.Background(), a.ID)
	if err != nil || out.Kind != KindCommitted {
		t.Fatalf("row 22 on a held P: %+v, %v", out, err)
	}
	c, _ := ac.DecodeCommitted(out.Body)
	res, _ := ac.DecodeResult(c.Result)
	var bundle, ev []byte
	if err := e.f.App.QueryRow(`SELECT bundle, cp_evidence FROM authority.account_events WHERE account_id = $1 AND seq = $2`,
		a.ID, res.Seq).Scan(&bundle, &ev); err != nil {
		t.Fatal(err)
	}
	b, _ := ac.DecodeBundle(bundle)
	az, _ := ac.DecodeCPAuthz(b.CPAuthz)
	hs, _ := ac.DecodeSigned(b.HeldStatus)
	st, _ := ac.DecodeStatus(hs.Msg)
	switch {
	case res.State != ac.StateActive || res.Assurance != "" || !bytes.Equal(res.PrevEntryDigest, pendingEntry):
		t.Fatalf("result %+v: want exactly x (active), assurance \"\", linked to the P entry", res)
	case az.Actor != ac.ActorRestore || len(b.CPAuthzSig) != 0 || !bytes.Equal(az.EvidenceDigest, ac.Digest(b.HeldStatus)):
		t.Fatalf("cp-authz %+v: want actor restore, no CAK signature, evidence = SHA-384(held_status) (check 1)", az)
	case st.Held != 1 || st.AsOfMs != res.IssuedAtMs || st.Seq != res.Seq-1 || !bytes.Equal(st.HeadEntryDigest, pendingEntry):
		t.Fatalf("held_status %+v: want held = 1, as_of = issued_at, naming the from-head (checks 2–3)", st)
	case ev != nil:
		t.Fatal("cp_evidence must be NULL on a row-22 event (E3-02)")
	}
	var lc, held any
	_ = e.f.App.QueryRow(`SELECT last_cancel_at, held FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&lc, &held)
	if lc != nil || held != true {
		t.Fatalf("row 22 set last_cancel_at (%v) or cleared held (%v); B3 clears held", lc, held)
	}
}

// Step 9 binds row 22: no restore-cancel is signed with the clock behind the
// head (D-36) or under a delegation that does not list this node's RSK (T10).
func TestRestoreCancelRunsStep9(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	before := countRows(t, e, "account_events")
	e.now.Store(e.now.Load() - 60_000) // the new leader's clock is behind the head's issued_at
	if out, err := e.s.commitRestoreCancel(context.Background(), a.ID); err == nil || out.Code != string(ac.ErrorClock) {
		t.Fatalf("row 22 with the clock behind the head = %+v, %v; want 503 clock", out, err)
	}
	e.now.Store(e.now.Load() + 60_000)
	e.r.Install(t, e.f.App, e.r.NextDelegation(t, authoritytest.WithRSKs(authoritytest.Point(authoritytest.NewRealm(t).CAK))))
	if out, err := e.s.commitRestoreCancel(context.Background(), a.ID); err == nil || out.Code != string(ac.ErrorSigner) {
		t.Fatalf("row 22 under a delegation without this node's RSK = %+v, %v; want 503 signer", out, err)
	}
	if countRows(t, e, "account_events") != before {
		t.Fatal("a row 22 refused at step 9 wrote an event")
	}
}

// D-241 (DoR:478, review round 2): a head read and row 22 in one database
// millisecond never share an as_of_ms; row 22 signs at a later one.
func TestRestoreCancelNeverSharesAnAsOfWithARead(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	read, _, _ := readHead(t, e, a.ID)
	out, err := e.s.commitRestoreCancel(context.Background(), a.ID) // the pinned clock has not moved
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ac.DecodeCommitted(out.Body)
	res, _ := ac.DecodeResult(c.Result)
	if res.IssuedAtMs <= read.AsOfMs {
		t.Fatalf("row 22 issued at %d after a read at as_of %d: its held_status shares or precedes it", res.IssuedAtMs, read.AsOfMs)
	}
}

// D-241 bounded: when db_now never passes the read's as_of_ms, row 22 gives up
// with 503 clock, sets the D-147 leg (B1-D31) and writes nothing. The test
// above is the control: the same steps commit once the clock moves.
func TestRestoreCancelGivesUpWhenTheClockNeverPassesARead(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	e.s.hooks.Pause = func() {} // db_now never moves
	readHead(t, e, a.ID)
	before := countRows(t, e, "account_events")
	if e.s.nonClient503.Load() {
		t.Fatal("control: the D-147 leg is already set")
	}
	if out, err := e.s.commitRestoreCancel(context.Background(), a.ID); err == nil || out.HTTP != 503 || out.Code != string(ac.ErrorClock) {
		t.Fatalf("row 22 with a frozen clock = %+v, %v; want 503 clock", out, err)
	}
	if !e.s.nonClient503.Load() {
		t.Fatal("row 22 gave up on the clock without setting the D-147 leg")
	}
	if countRows(t, e, "account_events") != before {
		t.Fatal("a row 22 refused on the clock wrote an event")
	}
}

// T10: once no installed delegation's window covers db_now, row 22 is 503
// signer and writes nothing.
func TestRestoreCancelWithNoDelegationCoveringNowIsSigner(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	before := countRows(t, e, "account_events")
	e.advance(180 * 86_400_000) // past every delegation's not_after
	if out, err := e.s.commitRestoreCancel(context.Background(), a.ID); err == nil || out.HTTP != 503 || out.Code != string(ac.ErrorSigner) {
		t.Fatalf("row 22 with no delegation covering now = %+v, %v; want 503 signer", out, err)
	}
	if countRows(t, e, "account_events") != before {
		t.Fatal("a row 22 refused for its signer wrote an event")
	}
}

// T6 (DoR:347, :720, review round 3): held_status is a status, so row 22 needs
// `status` in the signing delegation's scope, and without it writes nothing.
func TestRestoreCancelNeedsStatusInScope(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	e.r.Install(t, e.f.App, e.r.NextDelegation(t, func(d *ac.Delegation) {
		d.Scope = slices.DeleteFunc(slices.Clone(d.Scope), func(s string) bool { return s == ac.ScopeStatus })
	}))
	before := countRows(t, e, "account_events")
	if out, err := e.s.commitRestoreCancel(context.Background(), a.ID); err == nil || out.Code != string(ac.ErrorSigner) {
		t.Fatalf("row 22 without status in scope = %+v, %v; want 503 signer", out, err)
	}
	if countRows(t, e, "account_events") != before {
		t.Fatal("a row 22 refused for its scope wrote an event")
	}
}

// A refusal is always a non-nil error (the contract), so B3's ClearHeld never
// clears held after a row 22 that did not commit (PR review).
func TestRestoreCancelLeaderRefusalIsAnError(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	before := countRows(t, e, "account_events")
	for _, tc := range []struct {
		name      string
		hook      func(context.Context, *sql.Conn) (bool, error)
		code      ac.ErrorCode
		notLeader bool
	}{
		{"standby", func(context.Context, *sql.Conn) (bool, error) { return true, nil }, ac.ErrorNotLeader, true},
		{"recovery probe fails", func(context.Context, *sql.Conn) (bool, error) { return false, errors.New("boom") }, ac.ErrorStore, false},
	} {
		e.s.hooks.InRecovery = tc.hook
		out, err := e.s.commitRestoreCancel(context.Background(), a.ID)
		if err == nil || out.Code != string(tc.code) {
			t.Fatalf("%s: row 22 = %+v, %v; want 503 %s and a non-nil error", tc.name, out, err, tc.code)
		}
		// N1: only a standby is ErrNotLeader, so clear-held exits not_leader for
		// a failover and config for a role read that failed.
		if errors.Is(err, ErrNotLeader) != tc.notLeader {
			t.Fatalf("%s: row 22 error %v, errors.Is ErrNotLeader = %v, want %v", tc.name, err, !tc.notLeader, tc.notLeader)
		}
	}
	e.s.hooks.InRecovery = nil
	if countRows(t, e, "account_events") != before {
		t.Fatal("a row 22 refused by the leader check wrote an event")
	}
}
