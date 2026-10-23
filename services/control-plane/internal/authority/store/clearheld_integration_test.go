package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Task 9: clear-held (DoR:1729-1731, D-146, D-286, DoR mutation 8; B3-D1, B3-D9).
// The Owner reads the database's clock, so "now" below is dbMs bracketed before
// and after the verb. The env's pinned Store clock is caught up to the database
// clock (catchUp) before the Store submits anything after a clear, since row 22
// and the L1 writes are stamped with the database's time.
//
// The tests assume one test seam the implementer adds: Owner.afterRestoreCancel,
// a func() error called after commitRestoreCancel returns nil and before the L1
// transaction. A non-nil return aborts that account's clear and surfaces from
// ClearHeld (as the error or as a refused:<why> outcome).

// ---- helpers ----

// catchUp moves the env's pinned clock to the database's, so a Store submission
// after an Owner verb is not behind the event the verb stamped (D-36).
func (e *env) catchUp(t *testing.T) {
	t.Helper()
	if ms := u64(dbMs(t, e.f.Super)) + 1; ms > e.now.Load() {
		e.advance(ms - e.now.Load())
	}
}

// signedOwner is an Owner over the realm's pool carrying the realm's RSK.
func signedOwner(t *testing.T, e *env) *Owner {
	t.Helper()
	return ownerOver(t, e.f, e.r, OwnerConfig{Signer: e.r.RSK})
}

// insertHeldA0 gives a never-committed account a held head at A0 (absent, seq 0,
// no pin), the shape a restore leaves behind for an account the backup predates.
func insertHeldA0(t *testing.T, db *sql.DB, account string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO authority.account_heads (account_id, state, seq, recovery_gen, susp_operator,
		susp_user, root_revoked, legacy_pin_revoked, held) VALUES ($1, 'absent', 0, 0, false, false, false, false, true)`, account)
}

type clearedHead struct {
	state          string
	held, forceUnv bool
	activation     sql.NullInt64
	replacement    sql.NullInt64
	pendingRoot    bool
}

func readCleared(t *testing.T, db *sql.DB, account string) clearedHead {
	t.Helper()
	var c clearedHead
	if err := db.QueryRow(`SELECT state, held, force_unverified, activation_floor_ms, replacement_floor_ms,
		pending_root IS NOT NULL FROM authority.account_heads WHERE account_id = $1`, account).
		Scan(&c.state, &c.held, &c.forceUnv, &c.activation, &c.replacement, &c.pendingRoot); err != nil {
		t.Fatal(err)
	}
	return c
}

// wantFloor requires got to be some now + G' with now inside [before, after].
func wantFloor(t *testing.T, label string, got sql.NullInt64, before, after int64) {
	t.Helper()
	g := int64(GPrimeMs)
	if !got.Valid || got.Int64 < before+g || got.Int64 > after+g {
		t.Fatalf("%s = %v, want database now + G' within [%d, %d]", label, got, before+g, after+g)
	}
}

func wantResults(t *testing.T, got []ClearResult, want ...ClearResult) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("ClearHeld results = %+v, want %+v", got, want)
	}
}

func heldOf(t *testing.T, db *sql.DB, account string) bool {
	t.Helper()
	return readCleared(t, db, account).held
}

// rowTwentyTwo is the stored cancel_pending event ClearHeld signed for account,
// and the ChainHead it must verify against (rebuilt before the verb ran).
func rowTwentyTwo(t *testing.T, e *env, account string) []byte {
	t.Helper()
	var b []byte
	if err := e.f.App.QueryRow(`SELECT bundle FROM authority.account_events WHERE account_id = $1
		AND op = 'cancel_pending' ORDER BY seq DESC LIMIT 1`, account).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// heldPDPr builds one held head of each of the three states row 22 serves.
func heldPDPr(t *testing.T, e *env) (p, d, pr *authoritytest.Account) {
	t.Helper()
	p, _ = e.enrolledWith()
	e.commitOK(p, p.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes) // P

	d, _ = e.enrolledWith()
	e.commitOK(d, d.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.commitOK(d, d.CancelByRoot(t).Bytes)
	if e.commitOK(d, d.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes); d.Head.State != ac.StateDisputed {
		t.Fatalf("fixture: head %s, want disputed", d.Head.State)
	}

	pr, _ = e.enrolledWith()
	bb, sig := bound(t, e, pr)
	if r := e.commitOK(pr, pr.Recover(t, bb, sig).Bytes); r.State != ac.StatePending {
		t.Fatalf("fixture: recover left %s, want pending", r.State)
	}
	for _, a := range []*authoritytest.Account{p, d, pr} {
		e.pinHeld(a.ID)
	}
	return p, d, pr
}

// ---- row 22 states ----

// P, D and Pr take commitRestoreCancel and then the L1 transaction (DoR:1730).
func TestClearHeldPDPr(t *testing.T) {
	e := newEnv(t)
	p, d, pr := heldPDPr(t, e)
	o := signedOwner(t, e)

	prevs := map[string]ac.ChainHead{}
	var tr *ac.Trust
	for _, a := range []*authoritytest.Account{p, d, pr} {
		head, trust, _, err := rebuild(t, e, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		prevs[a.ID], tr = head, trust
	}
	events := countRows(t, e, "account_events")

	before := dbMs(t, e.f.Super)
	got, err := o.ClearHeld(liveCtx(t), []string{p.ID, d.ID, pr.ID})
	after := dbMs(t, e.f.Super)
	if err != nil {
		t.Fatalf("ClearHeld = %v, want nil", err)
	}
	wantResults(t, got, ClearResult{p.ID, "restore_cancel"}, ClearResult{d.ID, "restore_cancel"}, ClearResult{pr.ID, "restore_cancel"})
	if n := countRows(t, e, "account_events"); n != events+3 {
		t.Fatalf("account_events = %d, want %d: one row 22 per account", n, events+3)
	}

	for _, a := range []*authoritytest.Account{p, d, pr} {
		bundle := rowTwentyTwo(t, e, a.ID)
		if _, r := ac.VerifyBundle(tr, tr.HighWater(), prevs[a.ID], bundle, nil); r != ac.ReasonOK {
			t.Fatalf("%s: the row-22 bundle verifies as %s, want ok (checks 1-4)", a.ID, r)
		}
		b, _ := ac.DecodeBundle(bundle)
		hs, _ := ac.DecodeSigned(b.HeldStatus)
		st, _ := ac.DecodeStatus(hs.Msg)
		if st.Held != 1 {
			t.Fatalf("%s: held_status.held = %d, want 1: row 22 is signed BEFORE held is cleared", a.ID, st.Held)
		}
		az, _ := ac.DecodeCPAuthz(b.CPAuthz)
		if az.Actor != ac.ActorRestore {
			t.Fatalf("%s: cp-authz actor %q, want restore", a.ID, az.Actor)
		}

		c := readCleared(t, e.f.Super, a.ID)
		if c.state != ac.StateActive || c.held || !c.forceUnv || c.pendingRoot {
			t.Fatalf("%s: head after clear-held = %+v, want active, held=false, force_unverified=true, no pending root", a.ID, c)
		}
		wantFloor(t, a.ID+" activation_floor_ms", c.activation, before, after)
		if set := readSet(t, e, a.ID); set.Pending != "" {
			t.Fatalf("%s: the device set still has pending %q: row 22 burns the slot (DR8)", a.ID, set.Pending)
		}
	}
	if set := readSet(t, e, pr.ID); len(set.RecoveryKey) == 0 {
		t.Fatal("row 22 dropped the bound recovery key: DR8 keeps it")
	}

	// A following replace_root activates as unverified (D-146).
	p.Sync(t, e.f.App)
	e.catchUp(t)
	if r := e.commitOK(p, p.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes); r.Assurance != ac.AssuranceUnverified {
		t.Fatalf("replace_root after clear-held = %q, want unverified", r.Assurance)
	}
}

// A0 has nothing to cancel: held := false only, and force_unverified stays
// unset so the next enroll_new is admitted (DoR mutation 8).
func TestClearHeldA0(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	insertHeldA0(t, e.f.Owner, a.ID)
	events := countRows(t, e, "account_events")
	o := signedOwner(t, e)

	got, err := o.ClearHeld(liveCtx(t), []string{a.ID})
	if err != nil {
		t.Fatalf("ClearHeld = %v, want nil", err)
	}
	wantResults(t, got, ClearResult{a.ID, "absent"})
	c := readCleared(t, e.f.Super, a.ID)
	if c.held || c.forceUnv || c.activation.Valid || c.replacement.Valid || c.state != ac.StateAbsent {
		t.Fatalf("head after clear-held on A0 = %+v, want absent, held=false, force_unverified unset, no floors", c)
	}
	if countRows(t, e, "account_events") != events {
		t.Fatal("clear-held on A0 wrote an event")
	}

	a.Sync(t, e.f.App)
	dev := authoritytest.NewDevice(t)
	e.catchUp(t)
	e.commitOK(a, a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: dev,
		Factors: []string{ac.FactorRegistration}}).Bytes)
	if a.Head.State != ac.StateActive {
		t.Fatalf("enroll_new after clear-held left %s, want active (DoR mutation 8)", a.Head.State)
	}
}

// Pe, A and S take the L1 transaction alone (DoR:1731): unverified, floors.
func TestClearHeldOtherStatesTakeL1(t *testing.T) {
	e := newEnv(t)
	pe, _ := e.enrolledWith()
	e.commitOK(pe, pe.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes) // Pe
	act, _ := e.enrolledWith()                                         // A
	susp, _ := e.enrolledWith()
	e.commitOK(susp, susp.Suspend(t, ac.ActorUser).Bytes) // S
	slot, _ := e.enrolledWith()                           // A with a filled recovery-key slot
	bound(t, e, slot)
	refresh := authoritytest.NewDevice(t).DSK
	e.deviceOK(slot, slot.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpBindRecovery, BindWait: ac.BindWaitDelta,
		Bind: &ac.RecoveryBind{RealmID: e.r.ID, AccountID: slot.ID, Kind: ac.BindKindBind,
			RecoveryKey: authoritytest.Point(refresh), ChainSeq: slot.Head.Seq, ChainEntryDigest: slot.Head.Entry,
			BindingRootDigest: ac.KeyDigest(slot.Head.Root), EscrowPublicKey: authoritytest.Point(authoritytest.NewDevice(t).DSK)},
		BindKey: refresh, Factors: []string{ac.FactorEmail, ac.FactorPassword}}).Bytes)
	all := []*authoritytest.Account{pe, act, susp, slot}
	for _, a := range all {
		e.pinHeld(a.ID)
	}
	events := countRows(t, e, "account_events")
	o := signedOwner(t, e)

	ids := []string{pe.ID, act.ID, susp.ID, slot.ID}
	before := dbMs(t, e.f.Super)
	got, err := o.ClearHeld(liveCtx(t), ids)
	after := dbMs(t, e.f.Super)
	if err != nil {
		t.Fatalf("ClearHeld = %v, want nil", err)
	}
	wantResults(t, got, ClearResult{pe.ID, "unverified"}, ClearResult{act.ID, "unverified"},
		ClearResult{susp.ID, "unverified"}, ClearResult{slot.ID, "unverified"})
	if countRows(t, e, "account_events") != events {
		t.Fatal("the L1-only states signed an event: only P, D and Pr take row 22")
	}
	for _, a := range all {
		c := readCleared(t, e.f.Super, a.ID)
		if c.held || !c.forceUnv {
			t.Fatalf("%s: head = %+v, want held=false, force_unverified=true", a.ID, c)
		}
		wantFloor(t, a.ID+" activation_floor_ms", c.activation, before, after)
	}
	wantFloor(t, "the filled slot's replacement_floor_ms", readCleared(t, e.f.Super, slot.ID).replacement, before, after)
	if c := readCleared(t, e.f.Super, act.ID); c.replacement.Valid {
		t.Fatalf("an empty slot got replacement_floor_ms %v", c.replacement)
	}

	// A held Pe activates no earlier than now + G'.
	floor := u64(readCleared(t, e.f.Super, pe.ID).activation.Int64)
	e.catchUp(t)
	if e.now.Load() >= floor-1 {
		t.Fatalf("fixture: the pinned clock %d is already at the floor %d", e.now.Load(), floor)
	}
	e.advance(floor - 1 - e.now.Load())
	if got := e.state(pe); got != ac.StatePending {
		t.Fatalf("at floor - 1 the Pe head is %q, want pending", got)
	}
	e.advance(1)
	if got := e.state(pe); got == ac.StatePending {
		t.Fatal("at the floor the Pe head is still pending")
	}
}

// pendingUntilFloor requires a pending head to stay pending past its own
// base + G (the commit's Δ + G) and right up to floor - 1: the instant a missing
// or lowered floor would have let it activate. The clock ends at floor - 1.
func pendingUntilFloor(t *testing.T, e *env, a *authoritytest.Account, floor uint64) {
	t.Helper()
	d := e.r.Current.D
	e.advance(d.DeltaMs + d.GMs + 1)
	if e.now.Load() >= floor-1 {
		t.Fatalf("fixture: base + G (%d) is not below the floor %d", e.now.Load(), floor)
	}
	if got := e.state(a); got != ac.StatePending {
		t.Fatalf("past base + G the head is %q, want pending: the cleared floor %d (D-146, DoR:1766) did not reach the replacement", got, floor)
	}
	e.advance(floor - 1 - e.now.Load())
	if got := e.state(a); got != ac.StatePending {
		t.Fatalf("at floor - 1 the head is %q, want pending", got)
	}
}

// D-146: after clear-held, rotate_root is wrong_state (force_unverified); once
// the replacement activates it is admitted. The replacement carries the G'
// floor clear-held set (DoR:1766): it is not activatable at base + G.
func TestClearHeldThenRotateRootWrongState(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.pinHeld(a.ID)
	o := signedOwner(t, e)
	before := dbMs(t, e.f.Super)
	if _, err := o.ClearHeld(liveCtx(t), []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	after := dbMs(t, e.f.Super)
	cleared := readCleared(t, e.f.Super, a.ID)
	wantFloor(t, "the cleared head's activation_floor_ms", cleared.activation, before, after)
	a.Sync(t, e.f.App)
	e.catchUp(t)
	e.refusedAs(a.RotateRoot(t).Bytes, ac.RefusalWrongState)

	newDev := authoritytest.NewDevice(t)
	r := e.commitOK(a, a.Build(t, authoritytest.Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true, NewDevice: newDev,
		Factors: []string{ac.FactorEmail, ac.FactorPassword}, PasswordLogin: true, WindowClear: true}).Bytes)
	if r.Assurance != ac.AssuranceUnverified {
		t.Fatalf("replace_root after clear-held = %q, want unverified", r.Assurance)
	}
	rep := readCleared(t, e.f.Super, a.ID)
	if rep.activation != cleared.activation {
		t.Fatalf("the replacement's activation_floor_ms = %v, want the cleared floor %v", rep.activation, cleared.activation)
	}
	floor := u64(cleared.activation.Int64)
	pendingUntilFloor(t, e, a, floor)
	e.advance(2) // past the floor
	a.Activated()
	a.Head.Root, a.Head.EKGen, a.Actor = authoritytest.Point(a.Root), a.Head.EKGen+1, newDev
	e.commitOK(a, a.RotateEK(t).Bytes) // any commit persists the activation
	if c := readCleared(t, e.f.Super, a.ID); c.forceUnv {
		t.Fatalf("force_unverified survived the replacement's activation: %+v (D-188)", c)
	}
	e.commitOK(a, a.RotateRoot(t).Bytes)
}

// DoR:1766 (D-146): the replace_root committed after clear-held activates as
// unverified WITH the G' floor. Delta + G (17 minutes) is far below G' (24 h),
// so a commit that drops the floor activates the replacement 23 h early. The
// floor is the cleared head's, carried through the commit, not re-derived.
func TestClearHeldFloorReachesTheNextReplacement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *env) *authoritytest.Account
	}{
		{"held A head", func(_ *testing.T, e *env) *authoritytest.Account {
			a, _ := e.enrolledWith()
			return a
		}},
		{"held S head (user-suspended)", func(t *testing.T, e *env) *authoritytest.Account {
			a, _ := e.enrolledWith()
			e.commitOK(a, a.Suspend(t, ac.ActorUser).Bytes)
			return a
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if g, d := GPrimeMs, e.r.Current.D.DeltaMs+e.r.Current.D.GMs; g <= d {
				t.Fatalf("fixture: G' %d must exceed delta + G %d", g, d)
			}
			a := tc.setup(t, e)
			e.pinHeld(a.ID)
			before := dbMs(t, e.f.Super)
			if _, err := signedOwner(t, e).ClearHeld(liveCtx(t), []string{a.ID}); err != nil {
				t.Fatal(err)
			}
			after := dbMs(t, e.f.Super)
			cleared := readCleared(t, e.f.Super, a.ID)
			wantFloor(t, "the cleared head's activation_floor_ms", cleared.activation, before, after)

			a.Sync(t, e.f.App)
			e.catchUp(t)
			r := e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
			if r.State != ac.StatePending {
				t.Fatalf("replace_root after clear-held left %s, want pending", r.State)
			}
			if rep := readCleared(t, e.f.Super, a.ID); rep.activation != cleared.activation {
				t.Fatalf("the replacement's activation_floor_ms = %v, want the cleared floor %v (clear time + G')",
					rep.activation, cleared.activation)
			}
			floor := u64(cleared.activation.Int64)
			pendingUntilFloor(t, e, a, floor)
			e.advance(1)
			if got := e.state(a); got == ac.StatePending {
				t.Fatal("at the floor the replacement is still pending")
			}
		})
	}
}

// A floor already past (db_now) is dropped by the next commit, not carried: the
// replacement activates at base + G, with no floor left on the row.
func TestExpiredFloorIsNotCarried(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	past := e.now.Load() - 1_000
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET activation_floor_ms = $2 WHERE account_id = $1`, a.ID, i64(past))
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	if c := readCleared(t, e.f.Super, a.ID); c.activation.Valid {
		t.Fatalf("an expired floor was carried: activation_floor_ms = %v, want NULL", c.activation)
	}
	e.activate() // Delta + G
	if got := e.state(a); got == ac.StatePending {
		t.Fatal("the replacement is still pending at base + G: an expired floor held it")
	}
}

// A commit that arms its own floor (row 18, an erasure-list erase: D-186's
// reconcile-complete + G') and also inherits a still-future one ends with the
// later of the two.
func TestCarriedFloorTakesTheLaterOfTwo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		carried func(now uint64) uint64
		want    func(now uint64) uint64
	}{
		{"carried is later than the armed floor", func(now uint64) uint64 { return now + 3*GPrimeMs },
			func(now uint64) uint64 { return now + 3*GPrimeMs }},
		{"armed is later than the carried floor", func(now uint64) uint64 { return now + GPrimeMs/2 },
			func(now uint64) uint64 { return now + GPrimeMs }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.enrolledWith()
			rc := e.now.Load() // reconcile_completed_at, so the commit arms rc + G'
			mustExec(t, e.f.Owner, `UPDATE authority.meta SET reconcile_completed_at = to_timestamp($1 / 1000.0)`, rc)
			mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET activation_floor_ms = $2 WHERE account_id = $1`,
				a.ID, i64(tc.carried(rc)))
			e.commitOK(a, a.Erase(t, ac.ActorUser, ac.FactorErasureList).Bytes)
			want := tc.want(rc)
			if c := readCleared(t, e.f.Super, a.ID); !c.activation.Valid || u64(c.activation.Int64) != want {
				t.Fatalf("activation_floor_ms = %v, want %d: the later of the carried and the armed floor", c.activation, want)
			}
			e.activate()
			if got := e.state(a); got != ac.StatePending {
				t.Fatalf("at base + G the erase is %q, want pending behind the floor", got)
			}
		})
	}
}

// D-286: a held account's filled slot does not complete while held, and
// completes no earlier than clear-held + G'.
func TestHeldSlotDoesNotComplete(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	bound(t, e, a)
	refresh := authoritytest.NewDevice(t).DSK
	e.deviceOK(a, a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpBindRecovery, BindWait: ac.BindWaitDelta,
		Bind: &ac.RecoveryBind{RealmID: e.r.ID, AccountID: a.ID, Kind: ac.BindKindBind,
			RecoveryKey: authoritytest.Point(refresh), ChainSeq: a.Head.Seq, ChainEntryDigest: a.Head.Entry,
			BindingRootDigest: ac.KeyDigest(a.Head.Root), EscrowPublicKey: authoritytest.Point(authoritytest.NewDevice(t).DSK)},
		BindKey: refresh, Factors: []string{ac.FactorEmail, ac.FactorPassword}}).Bytes)
	e.pinHeld(a.ID)
	raw := func() []byte {
		var d []byte
		if err := e.f.App.QueryRow(`SELECT device_set_digest FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	start := raw()
	completed := func() bool {
		_, dst, _ := readHead(t, e, a.ID)
		return string(dst.DeviceSetDigest) != string(start)
	}

	e.activate() // the slot's own delta + g has elapsed
	if completed() {
		t.Fatal("a held account's recovery-key slot completed")
	}
	o := signedOwner(t, e)
	before := dbMs(t, e.f.Super)
	if _, err := o.ClearHeld(liveCtx(t), []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	after := dbMs(t, e.f.Super)
	c := readCleared(t, e.f.Super, a.ID)
	wantFloor(t, "replacement_floor_ms", c.replacement, before, after)
	floor := u64(c.replacement.Int64)
	if e.now.Load() >= floor-1 {
		t.Fatalf("fixture: the pinned clock %d is already at the floor %d", e.now.Load(), floor)
	}
	e.advance(floor - 1 - e.now.Load())
	if completed() {
		t.Fatal("the slot completed before clear-held + G'")
	}
	e.advance(1)
	if !completed() {
		t.Fatal("the slot did not complete at clear-held + G'")
	}
}

// ---- resumability ----

// A crash between row 22 and the L1 transaction leaves an active, still-held
// head; a re-run takes it as an ordinary L1 case and signs no second row 22.
func TestClearHeldResumesAfterRowTwentyTwo(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	events := countRows(t, e, "account_events")
	o := signedOwner(t, e)

	o.afterRestoreCancel = func() error { return errors.New("injected crash after row 22") }
	got, err := o.ClearHeld(liveCtx(t), []string{a.ID})
	if err == nil && (len(got) != 1 || !strings.HasPrefix(got[0].Outcome, "refused")) {
		t.Fatalf("ClearHeld across the injected failure = %+v, %v; want an error or a refused outcome", got, err)
	}
	if !heldOf(t, e.f.Super, a.ID) {
		t.Fatal("the failed run cleared held")
	}
	if n := countRows(t, e, "account_events"); n != events+1 {
		t.Fatalf("account_events = %d after the failed run, want %d: row 22 committed", n, events+1)
	}

	o.afterRestoreCancel = nil
	got, err = o.ClearHeld(liveCtx(t), []string{a.ID})
	if err != nil {
		t.Fatalf("re-run = %v, want nil", err)
	}
	wantResults(t, got, ClearResult{a.ID, "unverified"})
	if c := readCleared(t, e.f.Super, a.ID); c.held || !c.forceUnv {
		t.Fatalf("head after the re-run = %+v, want held=false, force_unverified=true", c)
	}
	if n := countRows(t, e, "account_events"); n != events+1 {
		t.Fatalf("account_events = %d after the re-run, want %d: no second row 22", n, events+1)
	}
}

// ---- refusals ----

func TestClearHeldRefusesWhileServeRuns(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	o := signedOwner(t, e)
	events := countRows(t, e, "account_events")

	held, err := AcquireServeLock(liveCtx(t), e.f.Owner)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	before := authoritySnapshot(t, e.f.App)
	if _, err := o.ClearHeld(liveCtx(t), []string{a.ID}); !errors.Is(err, ErrServeLockHeld) {
		t.Fatalf("ClearHeld while serve holds the lock = %v, want ErrServeLockHeld", err)
	}
	if after := authoritySnapshot(t, e.f.App); after != before || countRows(t, e, "account_events") != events {
		t.Fatalf("a refused ClearHeld wrote:\nbefore %s\nafter  %s", before, after)
	}
	held.Release()
	if _, err := o.ClearHeld(liveCtx(t), []string{a.ID}); err != nil {
		t.Fatalf("ClearHeld once the lock is free = %v, want nil (the control)", err)
	}
	if holdsServeLock(t, e.f.Owner) {
		t.Fatal("ClearHeld returned still holding the serve lock")
	}
}

func TestClearHeldPlanWritesNothing(t *testing.T) {
	e := newEnv(t)
	p, _ := e.enrolledWith()
	e.commitOK(p, p.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	act, _ := e.enrolledWith()
	free, _ := e.enrolledWith() // never held
	a0 := e.r.NewAccount(t)
	e.pinHeld(p.ID)
	e.pinHeld(act.ID)
	insertHeldA0(t, e.f.Owner, a0.ID)
	o := signedOwner(t, e)

	// The plan takes no lock: it runs while serve holds the serve lock.
	lock, err := AcquireServeLock(liveCtx(t), e.f.Owner)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	before := authoritySnapshot(t, e.f.App)
	events := countRows(t, e, "account_events")
	got, err := o.ClearHeldPlan(liveCtx(t), []string{p.ID, a0.ID, act.ID, free.ID})
	if err != nil {
		t.Fatalf("ClearHeldPlan = %v, want nil", err)
	}
	wantResults(t, got, ClearResult{p.ID, "restore_cancel"}, ClearResult{a0.ID, "absent"},
		ClearResult{act.ID, "unverified"}, ClearResult{free.ID, "not_held"})
	if after := authoritySnapshot(t, e.f.App); after != before {
		t.Fatalf("ClearHeldPlan wrote:\nbefore %s\nafter  %s", before, after)
	}
	if n := countRows(t, e, "account_events"); n != events {
		t.Fatalf("ClearHeldPlan signed an event (%d, was %d)", n, events)
	}
	for _, id := range []string{p.ID, a0.ID, act.ID} {
		if !heldOf(t, e.f.Super, id) {
			t.Fatalf("ClearHeldPlan cleared held on %s", id)
		}
	}
}

func TestClearHeldRefusesBadInput(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	o := signedOwner(t, e)
	before := authoritySnapshot(t, e.f.App)
	events := countRows(t, e, "account_events")

	tooMany := []string{a.ID}
	for i := 1; i < 65; i++ {
		tooMany = append(tooMany, fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	for _, tc := range []struct {
		name string
		o    *Owner
		ids  []string
	}{
		{"65 accounts", o, tooMany},
		{"a duplicate", o, []string{a.ID, a.ID}},
		{"no signer", ownerOver(t, e.f, e.r, OwnerConfig{}), []string{a.ID}},
	} {
		if _, err := tc.o.ClearHeld(liveCtx(t), tc.ids); err == nil {
			t.Fatalf("%s: ClearHeld = nil, want an error", tc.name)
		}
		if after := authoritySnapshot(t, e.f.App); after != before || countRows(t, e, "account_events") != events {
			t.Fatalf("%s: a refused ClearHeld wrote:\nbefore %s\nafter  %s", tc.name, before, after)
		}
	}
	// The control: the same account alone is accepted.
	if _, err := o.ClearHeld(liveCtx(t), []string{a.ID}); err != nil {
		t.Fatalf("one account = %v, want nil (the control)", err)
	}
}

// The verb refuses before signing when its RSK is not listed in the installed
// HW delegation (T10): nothing is signed and nothing is cleared.
func TestClearHeldRefusesUndelegatedSigner(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.pinHeld(a.ID)
	events := countRows(t, e, "account_events")
	before := authoritySnapshot(t, e.f.App)

	stranger := ownerOver(t, e.f, e.r, OwnerConfig{Signer: authoritytest.NewRealm(t).RSK}) // a key no delegation lists
	if _, err := stranger.ClearHeld(liveCtx(t), []string{a.ID}); err == nil {
		t.Fatal("ClearHeld signed with an RSK the installed delegation does not list")
	}
	if after := authoritySnapshot(t, e.f.App); after != before || countRows(t, e, "account_events") != events {
		t.Fatalf("a refused ClearHeld wrote:\nbefore %s\nafter  %s", before, after)
	}
	if !heldOf(t, e.f.Super, a.ID) {
		t.Fatal("a refused ClearHeld cleared held")
	}
	if _, err := signedOwner(t, e).ClearHeld(liveCtx(t), []string{a.ID}); err != nil {
		t.Fatalf("ClearHeld with the delegated RSK = %v, want nil (the control)", err)
	}
}

// One account's refusal is its outcome, and the accounts after it are still
// cleared (Task 9 step 3): the refused account stays held, the next one does not.
func TestClearHeldRefusalDoesNotAbortTheRest(t *testing.T) {
	e := newEnv(t)
	p, _ := e.enrolledWith()
	e.commitOK(p, p.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	act, _ := e.enrolledWith()
	e.pinHeld(p.ID)
	e.pinHeld(act.ID)
	o := signedOwner(t, e)
	o.afterRestoreCancel = func() error { return fmt.Errorf("%w: injected refusal", ErrRefused) }

	got, err := o.ClearHeld(liveCtx(t), []string{p.ID, act.ID})
	if err != nil {
		t.Fatalf("ClearHeld = %v, want nil: a per-account refusal is an outcome, not the verb's error", err)
	}
	if len(got) != 2 || got[0].Account != p.ID || !strings.HasPrefix(got[0].Outcome, "refused:") {
		t.Fatalf("results = %+v, want %s refused first", got, p.ID)
	}
	if got[1] != (ClearResult{act.ID, "unverified"}) {
		t.Fatalf("results = %+v, want %s unverified after the refusal", got, act.ID)
	}
	if !heldOf(t, e.f.Super, p.ID) || heldOf(t, e.f.Super, act.ID) {
		t.Fatal("want the refused account held and the next one cleared")
	}
}

// failoverAfterConn is an InRecovery hook that answers primary for the owner
// connection's role read (the first call) and standby for every later one: the
// leader check of the next row 22, after a failover mid-run.
func failoverAfterConn() func(context.Context, *sql.Conn) (bool, error) {
	var calls atomic.Int64
	return func(context.Context, *sql.Conn) (bool, error) { return calls.Add(1) > 1, nil }
}

// Codex round 2, G5: only an account's own refusal is its outcome. A database
// failure on one account (here a head another transaction holds beyond the 2 s
// lock_timeout, 55P03) stops the run there: the accounts before it keep their
// results, the ones after it are not touched, and the error is the database's,
// never a refused outcome.
func TestClearHeldStopsOnADatabaseFailure(t *testing.T) {
	e := newEnv(t)
	a1, _ := e.enrolledWith()
	a2, _ := e.enrolledWith()
	a3, _ := e.enrolledWith()
	for _, a := range []*authoritytest.Account{a1, a2, a3} {
		e.pinHeld(a.ID)
	}
	beginHolding(t, e.f.Super, `SELECT 1 FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`, a2.ID)
	o := signedOwner(t, e)

	got, err := o.ClearHeld(liveCtx(t), []string{a1.ID, a2.ID, a3.ID})
	var pe *pq.Error
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("ClearHeld = %v, want an error carrying SQLSTATE 55P03", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Fatalf("ClearHeld = %v: a lock wait read as a refusal", err)
	}
	wantResults(t, got, ClearResult{a1.ID, "unverified"})
	if heldOf(t, e.f.Super, a1.ID) || !heldOf(t, e.f.Super, a2.ID) || !heldOf(t, e.f.Super, a3.ID) {
		t.Fatal("want a1 cleared, and a2 and a3 still held: the run stopped at a2")
	}
}

// The other half of G5: a policy refusal of row 22 is still the account's
// outcome and the run goes on. The installed delegation here lacks
// cancel_pending in its scope (policy step 2, policy_disabled), a refusal the
// commit core makes on every held P, D and Pr alike (a held D head with anb 0,
// the runbook's V3-12 example, is cleared by this build and cannot be used).
// The outcome is the policy's text, with no store error prefix, and the next
// account is cleared.
func TestClearHeldPolicyRefusalContinues(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enrolledWith()
	e.commitOK(d, d.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.commitOK(d, d.CancelByRoot(t).Bytes)
	e.commitOK(d, d.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	if d.Head.State != ac.StateDisputed {
		t.Fatalf("fixture: head %s, want disputed", d.Head.State)
	}
	next, _ := e.enrolledWith()
	e.pinHeld(d.ID)
	e.pinHeld(next.ID)
	rsk, err := signer.GenerateP384() // D-223: an RSK is listed in one delegation only
	if err != nil {
		t.Fatal(err)
	}
	e.r.Install(t, e.f.App, e.r.NextDelegation(t, authoritytest.WithRSKs(rsk.PublicKey()), func(del *ac.Delegation) {
		del.Scope = slices.DeleteFunc(slices.Clone(del.Scope), func(s string) bool { return s == ac.OpCancelPending })
	}))
	o := ownerOver(t, e.f, e.r, OwnerConfig{Signer: rsk})

	got, err := o.ClearHeld(liveCtx(t), []string{d.ID, next.ID})
	if err != nil {
		t.Fatalf("ClearHeld = %v, want nil: a policy refusal is an outcome", err)
	}
	if len(got) != 2 || got[0].Account != d.ID || got[0].Outcome != "refused:row 22 not admitted: policy_disabled" {
		t.Fatalf("results = %+v, want %s refused with the policy's text and no store prefix", got, d.ID)
	}
	if got[1] != (ClearResult{next.ID, "unverified"}) {
		t.Fatalf("results = %+v, want %s unverified after the refusal", got, next.ID)
	}
	if !heldOf(t, e.f.Super, d.ID) || heldOf(t, e.f.Super, next.ID) {
		t.Fatal("want the refused account held and the next one cleared")
	}
}

// A plain error from the seam after row 22 is no refusal: the run stops, the
// error is returned as it is with no results, and row 22 stays committed (the
// resumable state) while the second account is untouched.
func TestClearHeldStopsOnAPlainErrorAfterRow22(t *testing.T) {
	e := newEnv(t)
	p, _ := e.enrolledWith()
	e.commitOK(p, p.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	act, _ := e.enrolledWith()
	e.pinHeld(p.ID)
	e.pinHeld(act.ID)
	o := signedOwner(t, e)
	cause := errors.New("injected database failure")
	o.afterRestoreCancel = func() error { return cause }
	events := countRows(t, e, "account_events")

	got, err := o.ClearHeld(liveCtx(t), []string{p.ID, act.ID})
	if !errors.Is(err, cause) {
		t.Fatalf("ClearHeld = %v, want the injected error", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %+v, want none: the stopped account gets no result", got)
	}
	if n := countRows(t, e, "account_events"); n != events+1 {
		t.Fatalf("account_events = %d, want %d: row 22 committed before the stop", n, events+1)
	}
	if !heldOf(t, e.f.Super, act.ID) {
		t.Fatal("the second account was cleared after the run stopped")
	}
}

// A failover between two accounts is ErrNotLeader (N1), so the CLI exits
// not_leader and not config: the first account is cleared and reported, the
// second is not touched. The probe says nothing replicated, and finish still
// runs on the stop (G5): the error carries both the cause and ErrNotReplicated.
func TestClearHeldStopsOnAFailoverAndStillFinishes(t *testing.T) {
	setup := func(t *testing.T, cfg OwnerConfig) (*env, *Owner, *authoritytest.Account, *authoritytest.Account) {
		e := newEnv(t)
		act, _ := e.enrolledWith()
		p, _ := e.enrolledWith()
		e.commitOK(p, p.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
		e.pinHeld(act.ID)
		e.pinHeld(p.ID)
		cfg.Signer, cfg.InRecovery = e.r.RSK, failoverAfterConn()
		return e, ownerOver(t, e.f, e.r, cfg), act, p
	}

	t.Run("not_leader", func(t *testing.T) {
		e, o, act, p := setup(t, OwnerConfig{})
		got, err := o.ClearHeld(liveCtx(t), []string{act.ID, p.ID})
		if !errors.Is(err, ErrNotLeader) {
			t.Fatalf("ClearHeld after a failover = %v, want ErrNotLeader", err)
		}
		wantResults(t, got, ClearResult{act.ID, "unverified"})
		if heldOf(t, e.f.Super, act.ID) || !heldOf(t, e.f.Super, p.ID) {
			t.Fatal("want the first account cleared and the second still held")
		}
	})

	t.Run("finish runs on a stop", func(t *testing.T) {
		e, o, act, p := setup(t, OwnerConfig{Probe: &fakeProbe{}}) // answers not replicated
		requireGate(t, e.f, true)
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		got, err := o.ClearHeld(ctx, []string{act.ID, p.ID})
		if !errors.Is(err, ErrNotLeader) || !errors.Is(err, ErrNotReplicated) {
			t.Fatalf("ClearHeld = %v, want both ErrNotLeader and ErrNotReplicated: the first account committed", err)
		}
		wantResults(t, got, ClearResult{act.ID, "unverified"})
	})
}
