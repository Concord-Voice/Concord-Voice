package decide

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Plan Task 10: the properties P2 and P4 and the A2 padding probe over generator 1
// (enumerate_test.go).

// enVeto is the K self-veto of T7 (#2): an address, backup address or SSO proof ref whose slot
// the request removes does not count toward cancelling it. The password is never excluded.
func enVeto(targets []CredRef) func(CredRef) bool {
	return func(r CredRef) bool {
		if r.Kind != CredAddress && r.Kind != CredBackupAddress && r.Kind != CredSSO {
			return false
		}
		for _, t := range targets {
			if t.Kind == r.Kind && t.ID == r.ID {
				return true
			}
		}
		return false
	}
}

// TestEnumerate_P2 (parent §7.6 P2, both directions): for every request R that is not a
// recovery_key row, pending or disputed, Decide(Cancel) cancels R if and only if the class of the
// proof p' (the self-veto-filtered proof) at R's OpenedAt is at least R's origin class and at
// least C1. A recovery_key row is never cancelled. The expected class comes from classOracle over
// the composition the generator knows, so a credential that matured after R opened (the fifth
// state) cannot count even if the implementation also uses it.
func TestEnumerate_P2(t *testing.T) {
	t.Parallel()
	st := newEnStats(t)

	verify := func(c enCase, kind ReqKind, status ReqStatus, oc Class, targets []CredRef, veto func(CredRef) bool) {
		r := enOpenReq("r", kind, status, oc, []CredRef{enOrigin}, targets)
		call := enCancelCall(c, r, 0, false)
		out := Decide(call.in)
		st.calls++
		label := fmt.Sprintf("kind=%d status=%d oc=%d targets=%v %s", kind, status, oc, targets, c.label())
		if kind == ReqRecoveryKey {
			if out.Verdict != VerdictRefused || out.Row != RowNone || len(out.Effects) != 0 {
				st.fail(label, fmt.Sprintf("a recovery_key row must never be cancelled: %+v", out))
			}
			return
		}
		cls := c.oracle(veto)
		if cls >= oc && cls >= C1 {
			if out.Verdict != VerdictApplied || out.Row != RowT7 || len(out.Effects) != 1 {
				st.fail(label, fmt.Sprintf("class %d >= %d must cancel: %+v", cls, oc, out))
				return
			}
			e := out.Effects[0]
			if e.Kind != EffectCancel || e.RequestID != "r" || e.Reason != CancelProof || e.CancellerClass != cls {
				st.fail(label, fmt.Sprintf("want cancel(proof, class %d), got %+v", cls, e))
			}
			return
		}
		if out.Verdict != VerdictRefused || out.Row != RowT7 || len(out.Effects) != 0 {
			st.fail(label, fmt.Sprintf("class %d < %d must not cancel: %+v", cls, oc, out))
		}
	}

	variants := [][]CredRef{{enTgtSSO}, {fxSSO}, {fxAddr}, {fxPW}}
	for _, shared := range []bool{false, true} {
		enForEach(true, shared, func(c enCase) {
			for _, targets := range variants {
				veto := enVeto(targets)
				for _, oc := range []Class{C1, C2, C3} {
					for _, status := range []ReqStatus{StatusPending, StatusDisputed} {
						verify(c, ReqCredentialRemove, status, oc, targets, veto)
					}
				}
			}
		})
	}
	// Every kind of request, on the small proofs (the class rule does not depend on the kind).
	kinds := []ReqKind{ReqCredentialRemove, ReqAddressChange, ReqBreakGlass, ReqSocialRecovery, ReqCircleChange, ReqRecoveryKey}
	enForEach(true, false, func(c enCase) {
		if len(c.ms) > 2 {
			return
		}
		for _, kind := range kinds {
			targets := []CredRef{enTgtSSO}
			if kind == ReqBreakGlass {
				targets = nil // a break-glass row has no Targets (ruling 4)
			}
			for _, oc := range []Class{C1, C2, C3} {
				for _, status := range []ReqStatus{StatusPending, StatusDisputed} {
					verify(c, kind, status, oc, targets, enVeto(targets))
				}
			}
		}
	})
	t.Logf("P2 decided %d cancels", st.calls)
	assert.Greater(t, st.calls, 100000)
}

// TestEnumerate_P2_BackupAddressSelfVeto: the generator's K category has no backup address, so
// the third kind of the self-veto gets its own check.
func TestEnumerate_P2_BackupAddressSelfVeto(t *testing.T) {
	bk := cref(CredBackupAddress, "bk")
	// They matured before the request opened (OpenedAt = t0-2h), so each would count.
	matured := t0.Add(-3 * time.Hour)
	creds := []Cred{credMaturing(fxPW, matured), credMaturing(bk, matured), credMaturing(fxAddr, matured)}
	for _, tc := range []struct {
		name    string
		targets []CredRef
		used    []CredRef
		cancels bool
	}{
		{"the backup address removed by the request does not count", []CredRef{bk}, []CredRef{bk}, false},
		{"another backup address slot does count", []CredRef{cref(CredBackupAddress, "other")}, []CredRef{bk}, true},
		{"the password never self-vetoes", []CredRef{fxPW}, []CredRef{fxPW}, true},
		{"the vetoed ref is dropped but the rest still counts", []CredRef{bk}, []CredRef{bk, fxPW}, true},
		{"a different kind in the same slot name is not the slot", []CredRef{cref(CredSSO, "bk")}, []CredRef{bk}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := enOpenReq("r", ReqCredentialRemove, StatusPending, C1, []CredRef{enOrigin}, tc.targets)
			in := Input{Now: t0, Act: Cancel{IDs: []string{"r"}}, Proof: Proof{Used: tc.used}, Creds: creds, Reqs: []Req{r}}
			out := Decide(in)
			assert.Equal(t, tc.cancels, out.Verdict == VerdictApplied)
			assert.Equal(t, RowT7, out.Row)
		})
	}
}

// TestEnumerate_P4 (parent §7.6 P4, Decide form): an immature (or awaiting, contested, or
// mature-after-ref) credential never contributes rank. For every enumerated T2/T3/T5/T6/T7/T12
// call, the outcome equals that of the same input with every proof element that does not count
// at the reference instant removed. The plan compares Row and Verdict; the whole outcome is
// compared too, because Opening.Origins (A2) is the only place a proof ref could still show.
// T1, T4 and T2b are excluded: they ignore maturity by rule.
func TestEnumerate_P4(t *testing.T) {
	t.Parallel()
	st := newEnStats(t)
	narrowed := 0
	run := func(call enCall) {
		st.calls++
		a, b := Decide(call.in), Decide(*call.filtered)
		st.rows[a.Row]++
		if len(call.in.Proof.Used) != len(call.filtered.Proof.Used) {
			narrowed++
		}
		switch {
		case a.Row != b.Row || a.Verdict != b.Verdict:
			st.fail(call.label(), fmt.Sprintf("with the uncounted elements row %d verdict %d; without them row %d verdict %d",
				a.Row, a.Verdict, b.Row, b.Verdict))
		case !reflect.DeepEqual(a, b):
			st.fail(call.label(), fmt.Sprintf("an uncounted proof element changed the outcome:\nwith:    %+v\nwithout: %+v", a, b))
		}
	}
	enEmitRemove(true, run)
	enEmitOpenBG(true, run)
	enEmitCancel(true, run)
	enEmitContest(true, run)
	enEmitResolve(true, run)
	t.Logf("P4 compared %d pairs, %d of them with at least one uncounted proof element", st.calls, narrowed)
	for _, row := range []Row{RowNone, RowT2, RowT3, RowT5, RowT6, RowT6c, RowT7, RowT8, RowT9, RowT10, RowT12} {
		assert.Positive(t, st.rows[row], "no P4 call reached row %d", row)
	}
	assert.Greater(t, narrowed, st.calls/2, "most P4 calls must carry an uncounted element")
	assert.Greater(t, st.calls, 100000)
}

// TestEnumerate_PaddingARefNeverChangesTheOutcome (architect ruling A2): Opening.Origins holds
// only counted refs, so adding one ref that does not count -- an immature, awaiting or contested
// credential, a stale version of a credential, or a ref naming nothing -- to a proof leaves the
// whole outcome unchanged. Without this, padding a proof with an immature ref turns a duplicate
// open into a dispute (parent T3/T6, §6.3) and breaks P4. The padding credential sits in the
// snapshot in both runs, so only the proof differs.
func TestEnumerate_PaddingARefNeverChangesTheOutcome(t *testing.T) {
	st := newEnStats(t)
	acts := []Action{
		Remove{Target: enTgtTOTP, Candidate: enCand}, Remove{Target: enTgtSSO, Candidate: enCand},
		OpenBreakGlass{Candidate: enCand}, OpenBreakGlass{Reset: true, Candidate: enCand},
	}
	type pad struct {
		name string
		ref  CredRef
		cred *Cred
	}
	for _, ms := range gen2Multisets() {
		if len(ms) > 2 {
			continue
		}
		g2slots, elem := gen2Layout(ms, false)
		c := enCase{ms: ms, ref: t0, elem: elem, slots: make([]enSlot, len(g2slots))}
		occ := [gen2NumCats]int{}
		for i, g := range g2slots {
			c.slots[i] = enSlot{cat: g.cat, ref: g.ref, state: enMature}
			occ[g.cat]++
		}
		var pads []pad
		for cat := gen2Cat(0); cat < gen2NumCats; cat++ {
			next := gen2Refs[cat][occ[cat]]
			for _, s := range []enState{enImmature, enAwaiting, enContested} {
				cr := enCred(next, s, t0)
				pads = append(pads, pad{fmt.Sprintf("%s %s", s, cat), next, &cr})
			}
		}
		if len(c.slots) > 0 {
			stale := c.slots[0].ref
			stale.Version++
			pads = append(pads, pad{"stale version", stale, nil})
		}
		pads = append(pads, pad{"ghost", cref(CredTOTP, "ghost"), nil})

		for _, p := range pads {
			for _, act := range acts {
				for _, v := range []int{0, 2, 4, 5, 7} {
					reqs, recents, ok := enReqVariant(v, c, enTgtTOTP, false)
					if !ok {
						continue
					}
					creds := append(c.creds(), matureCred(enTgtTOTP), matureCred(enTgtSSO))
					creds = append(creds, enBystanders()...)
					if p.cred != nil {
						creds = append(creds, *p.cred)
					}
					base := Input{Now: t0, Act: act, Proof: c.proof(), Creds: creds, Reqs: reqs,
						RecentCancels: recents, CurrentKeyID: 1}
					padded := base
					padded.Proof = proofBound(append(c.used(), p.ref), gen2DeviceBound(append(c.used(), p.ref))...)
					st.calls++
					a, b := Decide(base), Decide(padded)
					if !reflect.DeepEqual(a, b) {
						st.fail(fmt.Sprintf("%T pad=%s variant=%d %s", act, p.name, v, c.label()),
							fmt.Sprintf("padding the proof with an uncounted ref changed the outcome:\nbase:   %+v\npadded: %+v", a, b))
					}
				}
			}
		}
	}
	t.Logf("%d padded/unpadded pairs", st.calls)
	assert.Greater(t, st.calls, 5000)
}
