package authoritycontract

// R4's fix retention (#3587 PR-A2 red-team RT-1, RT-1b). A retained activation
// fix is the client's evidence that it saw the raw pending head activate, and
// two rules read it: R4 (DoR:738, "if activated, it fixes H_act for issued ≥
// T") and row 22's check 4 (DoR:642, "a held_status never overrides a fix, so
// a client that saw the from-head activate before the restore refuses the
// result"; :2149; E3-04, V3-09). One RSK "cannot point clients to an older EK"
// (DoR:2106). Dropping the fix for a later status, held (RT-1) or merely a
// newer activation (RT-1b), let an RSK revert a root change the client saw
// activate, and the client then wrapped to the OLD EK.

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// forgePendingReplace is a forgeActive account whose owner ran replace_root
// (row 11) to root2/EK2, and a co-member that saw it ACTIVATE (status S1 at
// t1 ≥ anb).
type forgePendingReplace struct {
	*forgeActive
	root2           []byte
	ek2, ekb2D      []byte
	entry2          []byte
	anb, t1         uint64
	s1              []byte    // the activation status the co-member retained
	headSawActivate ChainHead // the co-member's persisted head: raw pending, fix = S1
}

func newForgePendingReplace(t *testing.T) *forgePendingReplace {
	t.Helper()
	a := newForgeActive(t)
	root2k, ek2 := testKey(t), forgedEK(t)
	ekb2, ekb2D, ekb2Sig := a.ekBinding(root2k, 2, ek2)
	asserted := testT0 + 3_600_000
	anb := asserted + 259_200_000 + 900_000 // asserted + Δ + G of (0, 1)
	in := Intent{RequestID: newID(), Op: OpReplaceRoot, ExpiresAtMs: asserted + 600_000, ExpectedSeq: 1,
		ExpectedHeadDigest: a.entry1, PredecessorRootDigest: Digest(pointOf(t, a.root1)),
		CandidateRoot: pointOf(t, root2k), EKBindingDigest: ekb2D, DeviceIntentDigest: digestOf("dev-2")}
	auth := CPAuthz{Actor: ActorUser, AssertedAtMs: asserted, EvidenceDigest: digestOf("ev-2")}
	res := Result{Seq: 2, State: StatePending, PriorState: StateActive, ActiveRoot: pointOf(t, a.root1),
		EKBindingDigest: a.ekb1D, PendingRoot: pointOf(t, root2k), PendingEKBindingDigest: ekb2D,
		PrevEntryDigest: a.entry1, RecoveryGen: 1, ActivationNotBeforeMs: anb, Assurance: AssuranceUnverified,
		IssuedAtMs: asserted}
	b2, entry2 := a.finish(in, auth, res, forgeSigs{candidate: root2k, ekb: ekb2, ekbSig: ekb2Sig})

	p := &forgePendingReplace{forgeActive: a, root2: pointOf(t, root2k), ek2: ek2, ekb2D: ekb2D, entry2: entry2,
		anb: anb, t1: anb + 1_000}
	// S1: the co-member's status at t1 ≥ anb shows the change ACTIVATED (R5's
	// effective = Activate(H)): root2 and EK2 are live, and it wraps to EK2.
	p.s1 = p.activated(p.t1)
	head2, vs, _, r := VerifyChain(a.tr, HighWater{}, a.head1, ChainInput{Bundles: [][]byte{b2}, Status: p.s1},
		AccountMemory{}, nowOf(t, p.t1), UseNewSecret)
	require.Equal(t, ReasonOK, r, "harness: the activated replace_root must verify")
	require.Equal(t, p.s1, head2.Fix, "harness: VerifyChain retains S1 as the activation fix (R4)")
	spki, ok := vs.WrapKey(nil)
	require.True(t, ok)
	require.Equal(t, ek2, spki, "harness: the co-member now wraps to the NEW EK2")
	p.headSawActivate = head2
	return p
}

// activated is an RSK-signed status for seq 2 at asOf ≥ anb showing the
// derived activation (R5: effective = Activate(H), anb = 0).
func (p *forgePendingReplace) activated(asOf uint64) []byte {
	return p.status(Status{Seq: 2, HeadEntryDigest: p.entry2, State: StateActive,
		ActiveRootDigest: Digest(p.root2), EKBindingDigest: p.ekb2D, AsOfMs: asOf})
}

// pending is an RSK-signed status for seq 2 showing the raw pending head:
// held (anb = ANB_NEVER) when held, else as of a time before anb.
func (p *forgePendingReplace) pending(asOf uint64, held bool) []byte {
	st := Status{Seq: 2, HeadEntryDigest: p.entry2, State: StatePending, PriorState: StateActive,
		ActiveRootDigest: Digest(pointOf(p.t, p.root1)), EKBindingDigest: p.ekb1D,
		PendingRootDigest: Digest(p.root2), PendingEKBindingDigest: p.ekb2D,
		ActivationNotBeforeMs: p.anb, AsOfMs: asOf}
	if held {
		st.ActivationNotBeforeMs, st.Held = ANBNever, 1
	}
	return p.status(st)
}

// heldStatus is pending(asOf, true).
func (p *forgePendingReplace) heldStatus(asOf uint64) []byte { return p.pending(asOf, true) }

// restoreCancel is a row-22 bundle (cancel_pending / actor restore) issued at
// issued, carrying held as its held_status: it restores x = active with the
// OLD root1 and EK1.
func (p *forgePendingReplace) restoreCancel(issued uint64, held []byte) []byte {
	in := Intent{RequestID: newID(), Op: OpCancelPending, ExpiresAtMs: issued + 600_000, ExpectedSeq: 2,
		ExpectedRecoveryGen: 1, ExpectedHeadDigest: p.entry2}
	auth := CPAuthz{Actor: ActorRestore, AssertedAtMs: issued}
	res := Result{Seq: 3, State: StateActive, ActiveRoot: pointOf(p.t, p.root1), EKBindingDigest: p.ekb1D,
		PrevEntryDigest: p.entry2, RecoveryGen: 2, IssuedAtMs: issued}
	b, _ := p.finish(in, auth, res, forgeSigs{held: held})
	return b
}

// wrapsAfter reports what a co-member that accepted bundle onto head would
// wrap to next: the verdict of a fresh status for the result, and its key.
func (p *forgePendingReplace) wrapsAfter(head ChainHead, bundle []byte, asOf uint64) (Reason, []byte) {
	res, err := DecodeBundle(bundle)
	require.NoError(p.t, err)
	st := p.status(Status{Seq: 3, HeadEntryDigest: EntryDigest(res.Result), State: StateActive,
		ActiveRootDigest: Digest(pointOf(p.t, p.root1)), EKBindingDigest: p.ekb1D, AsOfMs: asOf})
	vs, _, r := VerifyStatus(p.tr, HighWater{}, head, st, AccountMemory{}, nowOf(p.t, asOf), UseNewSecret)
	spki, _ := vs.WrapKey(nil)
	return r, spki
}

// RT-1: one RSK serves a fresh status showing the head held (a refresh the
// client makes every τ anyway). It verifies (held, a withhold), and must not
// replace the activation fix, so the restore-cancel stays refused.
func TestRetainFixKeepsTheActivationFixAfterAHeldStatus(t *testing.T) {
	p := newForgePendingReplace(t)
	t2 := p.t1 + 60_000
	t3 := t2 + 60_000
	b3 := p.restoreCancel(t3, p.heldStatus(t3))

	// Control: with the activation fix in place, check 4 (with R4) refuses the
	// restore-cancel.
	_, r := VerifyBundle(p.tr, HighWater{}, p.headSawActivate, b3, nil)
	require.Equal(t, ReasonHypothesisConflict, r, "control: an activation fix refuses row 22")

	head3, _, _, r := VerifyChain(p.tr, HighWater{}, p.headSawActivate, ChainInput{Status: p.heldStatus(t2)},
		AccountMemory{}, nowOf(t, t2), UseNewSecret)
	require.Equal(t, ReasonHeld, r)
	require.Equal(t, p.s1, head3.Fix, "DoR:642: a held status never overrides an activation fix")

	head4, r := VerifyBundle(p.tr, HighWater{}, head3, b3, nil)
	if r == ReasonOK {
		r4, spki := p.wrapsAfter(head4, b3, t3+1_000)
		t.Logf("REGRESSION: the restore-cancel was accepted after a held status; VerifyStatus=%s, wraps to OLD EK1=%v",
			r4, bytes.Equal(spki, p.ek1))
	}
	require.Equal(t, ReasonHypothesisConflict, r,
		"DoR:642/:2149 (E3-04): a co-member that saw the from-head activate must refuse the restore-cancel")
}

// RT-1 through VerifyBundle's own fix argument: a supplied fix replaces the
// stored one only when the retention rule allows, so a caller that hands
// VerifyBundle the newest status it holds cannot drop the activation fix.
func TestVerifyBundleFixArgumentNeverReplacesAnActivationFix(t *testing.T) {
	p := newForgePendingReplace(t)
	t3 := p.t1 + 120_000
	b3 := p.restoreCancel(t3, p.heldStatus(t3))
	_, r := VerifyBundle(p.tr, HighWater{}, p.headSawActivate, b3, &Fix{Status: p.heldStatus(p.t1 + 60_000)})
	require.Equal(t, ReasonHypothesisConflict, r,
		"DoR:642 (E3-04): a held status passed as the fix must not override the retained activation fix")

	// A supplied activation does replace a stored held fix (R4: an activation
	// replaces a pending fix), so the same restore-cancel is then refused.
	heldHead := p.headSawActivate
	heldHead.Fix = p.heldStatus(p.t1 + 60_000)
	_, r = VerifyBundle(p.tr, HighWater{}, heldHead, b3, nil)
	require.Equal(t, ReasonOK, r, "control: a held fix admits the restore-cancel (H_pend)")
	_, r = VerifyBundle(p.tr, HighWater{}, heldHead, b3, &Fix{Status: p.s1})
	require.Equal(t, ReasonHypothesisConflict, r, "a supplied activation fix replaces a stored pending one")
}

// RT-1b: the same slot also moved an activation fix FORWARD on every ordinary
// status refresh, shrinking R4's "H_act for issued ≥ T1" to "≥ the latest
// refresh". An RSK and a CP holding the OLD root (the thief the owner replaced
// it to evict) back-date a root cancel (row 13) into [T1, T2): it would match
// H_pend and restore root1/EK1. No held status is involved.
func TestRetainFixKeepsTheEarliestActivationAfterARefresh(t *testing.T) {
	p := newForgePendingReplace(t)
	t2 := p.t1 + 600_000 // the next τ refresh, still showing root2 active
	head3, _, _, r := VerifyChain(p.tr, HighWater{}, p.headSawActivate, ChainInput{Status: p.activated(t2)},
		AccountMemory{}, nowOf(t, t2), UseNewSecret)
	require.Equal(t, ReasonOK, r)
	require.Equal(t, p.s1, head3.Fix, "R4: the earliest activation fixes H_act for the widest range")

	// Row 13 (cancel_pending / user), current-root = the OLD root1 (H_pend's
	// root), CAK-signed cp-authz, issued at T1 + 60 s < T2.
	x := p.t1 + 60_000
	in := Intent{RequestID: newID(), Op: OpCancelPending, ExpiresAtMs: x + 600_000, ExpectedSeq: 2,
		ExpectedRecoveryGen: 1, ExpectedHeadDigest: p.entry2, PredecessorRootDigest: Digest(pointOf(t, p.root1))}
	auth := CPAuthz{Actor: ActorUser, AssertedAtMs: x, EvidenceDigest: digestOf("ev-cancel")}
	res := Result{Seq: 3, State: StateActive, ActiveRoot: pointOf(t, p.root1), EKBindingDigest: p.ekb1D,
		PrevEntryDigest: p.entry2, RecoveryGen: 2, IssuedAtMs: x}
	b3, _ := p.finish(in, auth, res, forgeSigs{current: p.root1})

	_, r = VerifyBundle(p.tr, HighWater{}, p.headSawActivate, b3, nil)
	require.Equal(t, ReasonHypothesisConflict, r, "control: the T1 activation fix refuses a cancel issued after T1")

	head4, r := VerifyBundle(p.tr, HighWater{}, head3, b3, nil)
	if r == ReasonOK {
		r4, spki := p.wrapsAfter(head4, b3, t2+1_000)
		t.Logf("REGRESSION: a back-dated root cancel was accepted after a refresh; VerifyStatus=%s, wraps to OLD EK1=%v",
			r4, bytes.Equal(spki, p.ek1))
	}
	require.Equal(t, ReasonHypothesisConflict, r,
		"DoR:738 (R4): an activation seen at T1 fixes H_act for every result issued at or after T1")
}

// The retention rule itself, arm by arm (retainFix; VerifyBundle's fix
// argument goes through the same keepsFix): an activation fix is replaced
// only by an earlier activation; a held or pending status never replaces it;
// an activation replaces a pending fix (RT-1c: that fix's H_pend range is
// lost); of two pending fixes the newer stays; a fix that names no head this
// one is, or none at all, is replaced.
func TestRetainFixRetentionRule(t *testing.T) {
	p := newForgePendingReplace(t)
	h := p.headSawActivate
	act := func(at uint64) []byte { return p.activated(at) }
	cases := []struct {
		name     string
		old, new []byte
		keep     bool
	}{
		{"activation, then a later held status", p.s1, p.heldStatus(p.t1 + 1), true},
		{"activation, then a later activation", p.s1, act(p.t1 + 1), true},
		{"activation, then the same activation", p.s1, p.s1, true},
		{"activation, then an earlier activation", p.s1, act(p.anb), false},
		{"activation, then an earlier pending status", p.s1, p.pending(p.anb-1, false), true},
		{"pending, then an activation", p.pending(p.anb-1, false), act(p.anb), false},
		{"held, then an earlier activation", p.heldStatus(p.t1 + 1), p.s1, false},
		{"pending, then a newer pending status", p.pending(p.anb-2, false), p.pending(p.anb-1, false), false},
		{"pending, then an older pending status", p.pending(p.anb-1, false), p.pending(p.anb-2, false), true},
		{"pending, then a held status as of the same instant", p.heldStatus(p.t1), p.heldStatus(p.t1), true},
		{"no fix", nil, p.s1, false},
		{"a fix naming another head", p.status(Status{Seq: 1, HeadEntryDigest: p.entry1, State: StateActive,
			ActiveRootDigest: Digest(pointOf(t, p.root1)), EKBindingDigest: p.ekb1D, AsOfMs: testT0}), p.heldStatus(p.t1), false},
		{"an undecodable fix", []byte("junk"), p.heldStatus(p.t1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := fixStatus(tc.new)
			require.NoError(t, err)
			head := h
			head.Fix = tc.old
			got := retainFix(head, tc.new, *st).Fix
			want := tc.new
			if tc.keep {
				want = tc.old
			}
			require.Equal(t, want, got)
		})
	}
}

// N-2: on a continuity head whose retained fix showed the activation, a
// status showing it pending again (newer, or older) still wraps to the
// activated EK, never back to the legacy key: the fix decides. Read as
// pending, the continuity verdict is ok too and WrapKey wrapped to the served
// legacy SPKI, so the verdict alone cannot pin this; the wrap target does.
func TestShownActivationKeepsTheWrapOnTheNewEK(t *testing.T) {
	n := 0
	for _, v := range loadVectors(t).Vectors {
		if v.Name != "r4/status/continuity-pending-after-a-shown-activation" &&
			v.Name != "r4/status/continuity-older-pending-than-a-shown-activation" {
			continue
		}
		tr, r := trustOf(t, v)
		require.Equal(t, ReasonOK, r)
		prev := prevHeadOf(t, v)
		_, vs, _, got := VerifyChain(tr, hwOf(v.HW), prev, ChainInput{Status: mustB64(t, v.BytesB64)},
			memOf(t, v.Mem), nowOf(t, v.NowMs), UseNewSecret)
		require.Equal(t, ReasonOK, got, v.Name)
		require.NotEmpty(t, prev.PendingEKSPKI)
		ek, ok := vs.WrapKey(nil) // no served legacy SPKI: only the activated EK can come back
		require.True(t, ok, v.Name)
		require.Equal(t, prev.PendingEKSPKI, ek, v.Name)
		n++
	}
	require.Equal(t, 2, n)
}
