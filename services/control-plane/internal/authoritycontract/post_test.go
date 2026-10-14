package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func d48(s string) []byte { return digestOf(s) }

func TestIntentForOp(t *testing.T) {
	root := testPoint(t)
	base := Intent{RealmID: newID(), AccountID: newID(), RequestID: newID(), ExpiresAtMs: 1}
	ok := func(op string, mut func(*Intent)) Intent {
		in := base
		in.Op = op
		mut(&in)
		return in
	}
	rot := ok(OpRotateRoot, func(i *Intent) {
		i.CandidateRoot, i.EKBindingDigest, i.PredecessorRootDigest, i.DeviceIntentDigest = root, d48("e"), d48("p"), d48("D")
	})
	require.Equal(t, Refusal(""), IntentForOp(rot, ActorUser, false, false))
	bad := rot
	bad.RecoveryBindDigest = d48("B")
	require.Equal(t, RefusalMalformedForOp, IntentForOp(bad, ActorUser, false, false), "#16 on a non-recover op")
	bad = rot
	bad.DeviceIntentDigest = nil
	require.Equal(t, RefusalMalformedForOp, IntentForOp(bad, ActorUser, false, false), "#15 required")

	opUnsuspend := ok(OpUnsuspend, func(i *Intent) { i.PredecessorRootDigest = d48("p") })
	require.Equal(t, Refusal(""), IntentForOp(opUnsuspend, ActorOperator, false, false), "operator unsuspend has no #15")
	userUnsuspend := opUnsuspend
	require.Equal(t, RefusalMalformedForOp, IntentForOp(userUnsuspend, ActorUser, false, false), "user unsuspend needs #15")

	rootCancel := ok(OpCancelPending, func(i *Intent) { i.PredecessorRootDigest = d48("p") })
	require.Equal(t, Refusal(""), IntentForOp(rootCancel, ActorUser, false, true))
	require.Equal(t, RefusalMalformedForOp, IntentForOp(rootCancel, ActorUser, false, false), "predecessor iff the root cancels")
	popCancel := ok(OpCancelPending, func(i *Intent) { i.LegacyEKDigest = d48("pin") })
	require.Equal(t, Refusal(""), IntentForOp(popCancel, ActorUser, true, false))
	require.Equal(t, RefusalMalformedForOp, IntentForOp(popCancel, ActorUser, false, false), "legacy iff PoP")

	suspend := ok(OpSuspend, func(*Intent) {})
	require.Equal(t, Refusal(""), IntentForOp(suspend, ActorUser, false, false))
	suspend.CandidateRoot = root
	require.Equal(t, RefusalMalformedForOp, IntentForOp(suspend, ActorUser, false, false))
}

func TestSelectRowByPresence(t *testing.T) {
	ap := pub{State: StateAbsent, Pin: d48("pin")}
	r, reason := selectRow(ap, OpEnrollExisting, ActorUser, pCandidate|pEK|pPoP)
	require.Equal(t, ReasonOK, reason)
	require.Equal(t, 2, r.N)
	r, _ = selectRow(ap, OpEnrollExisting, ActorUser, pCandidate|pEK)
	require.Equal(t, 3, r.N)
	_, reason = selectRow(pub{State: StateAbsent, Pin: d48("pin"), PinRevoked: 1}, OpEnrollExisting, ActorUser, pCandidate|pEK|pPoP)
	require.Equal(t, ReasonTransitionInvalid, reason, "a PoP from Apr: no row")
	pAbs := pub{State: StatePending, Prior: StateAbsent, Pin: d48("pin"), PendRoot: testPoint(t)}
	r, _ = selectRow(pAbs, OpEnrollExisting, ActorUser, pCandidate|pEK|pPoP)
	require.Equal(t, 4, r.N, "row 4 admits an optional PoP")
	r, _ = selectRow(pAbs, OpEnrollExisting, ActorUser, pCandidate|pEK)
	require.Equal(t, 4, r.N)
	pr := pub{State: StatePending, Prior: StateActive, Root: testPoint(t), PendRoot: testPoint(t), Op: OpRecover}
	_, reason = selectRow(pr, OpCancelPending, ActorUser, pCurrent)
	require.Equal(t, ReasonTransitionInvalid, reason, "a root cancel of a Pr head")
	r, _ = selectRow(pr, OpCancelPending, ActorUser, pRecovery)
	require.Equal(t, 21, r.N)
	_, reason = selectRow(pub{State: StateActive, Root: testPoint(t)}, OpRotateRoot, ActorUser, pCandidate|pEK)
	require.Equal(t, ReasonTransitionInvalid, reason, "a missing required signature")
	_, reason = selectRow(pub{State: StateSuspended, Susp: []string{ActorUser}, Root: testPoint(t)}, OpSuspend, ActorUser, 0)
	require.Equal(t, ReasonTransitionInvalid, reason, "row 7: user ∉ set")
}

// Each To rule and assurance rule against a hand-built result.
func TestCheckPost(t *testing.T) {
	root, cand := testPoint(t), testPoint(t)
	ac := pub{Seq: 4, State: StateActive, Root: root, EK: d48("ek"), Pin: d48("pin"), RG: 2}
	row11, _ := RowByN(11)
	res := Result{State: StatePending, PriorState: StateActive, SuspendedBy: []string{}, ActiveRoot: root}
	res.EKBindingDigest, res.PendingRoot, res.PendingEKBindingDigest = d48("ek"), cand, d48("ek2")
	res.LegacyEKDigest, res.RecoveryGen, res.Assurance = d48("pin"), 3, AssuranceRecovery
	in := Intent{CandidateRoot: cand, EKBindingDigest: d48("ek2")}
	c := rowCtx{in: in, res: res, outcomeP: true, segment: Assurance(AssuranceInitial)}
	require.Equal(t, ReasonOK, checkPost(row11, ac, c))

	c.segment = Assurance(AssuranceUnverified)
	require.Equal(t, ReasonTransitionInvalid, checkPost(row11, ac, c), "D-182: recovery from an unverified segment")
	c.res.Assurance = AssuranceUnverified
	require.Equal(t, ReasonOK, checkPost(row11, ac, c))

	c.res.RecoveryGen = 2
	require.Equal(t, ReasonTransitionInvalid, checkPost(row11, ac, c), "Δrg")
	c.res.RecoveryGen = 3

	disp := c
	disp.outcomeP = false
	disp.res.State, disp.res.PendingRoot, disp.res.PendingEKBindingDigest, disp.res.Assurance = StateDisputed, nil, nil, ""
	require.Equal(t, ReasonOK, checkPost(row11, ac, disp), "rows 2, 3, 11 accept P or D (A6)")

	// Row 12 keeps the suspended set: set immutability while P or D.
	ps := pub{Seq: 5, State: StatePending, Prior: StateSuspended, Susp: []string{ActorUser}, Root: root, EK: d48("ek"),
		PendRoot: cand, PendEK: d48("ek2"), Pin: d48("pin"), RG: 3}
	row12, _ := RowByN(12)
	r12 := Result{State: StateDisputed, PriorState: StateSuspended, SuspendedBy: []string{ActorUser}, ActiveRoot: root,
		EKBindingDigest: d48("ek"), LegacyEKDigest: d48("pin"), RecoveryGen: 4}
	require.Equal(t, ReasonOK, checkPost(row12, ps, rowCtx{res: r12, in: Intent{CandidateRoot: testPoint(t)}}))
	r12.SuspendedBy = []string{}
	require.Equal(t, ReasonTransitionInvalid, checkPost(row12, ps, rowCtx{res: r12}), "the set is immutable while P or D")

	// Row 5's segment rule.
	row5, _ := RowByN(5)
	r5 := Result{State: StateActive, SuspendedBy: []string{}, ActiveRoot: cand, EKBindingDigest: d48("ek2"),
		LegacyEKDigest: d48("pin"), RecoveryGen: 2, Assurance: AssuranceContinuity}
	c5 := rowCtx{in: in, res: r5, segment: Assurance(AssuranceInitial)}
	require.Equal(t, ReasonOK, checkPost(row5, ac, c5))
	c5.res.Assurance = AssuranceRecovery
	require.Equal(t, ReasonTransitionInvalid, checkPost(row5, ac, c5), "the segment rule")
	c5.segment, c5.res.Assurance = Assurance(AssuranceUnverified), AssuranceUnverified
	require.Equal(t, ReasonOK, checkPost(row5, ac, c5), "D-142")
}

func TestCheckIntentValues(t *testing.T) {
	root := testPoint(t)
	f := pub{Seq: 4, Entry: d48("e4"), State: StateActive, Root: root, RG: 2, Pin: d48("pin")}
	row6, _ := RowByN(6)
	in := Intent{Op: OpRotateEK, ExpectedSeq: 4, ExpectedRecoveryGen: 2, ExpectedHeadDigest: d48("e4"),
		PredecessorRootDigest: Digest(root)}
	require.Equal(t, ReasonOK, checkIntentValues(row6, f, in))
	bad := in
	bad.ExpectedHeadDigest = d48("e3")
	require.Equal(t, ReasonTransitionInvalid, checkIntentValues(row6, f, bad), "an expected_head_digest mismatch")
	bad = in
	bad.PredecessorRootDigest = d48("x")
	require.Equal(t, ReasonTransitionInvalid, checkIntentValues(row6, f, bad), "a predecessor_root_digest mismatch")
	bad = in
	bad.ExpectedRecoveryGen = 1
	require.Equal(t, ReasonTransitionInvalid, checkIntentValues(row6, f, bad))
}

// ── E2 adversarial additions (beyond the plan's four tests) ─────────────────

// A two-part position (PoP, ek-binding, binding proof) with one part missing
// never satisfies a requirement and is never admitted as an extra: the half
// a verifier skips (an empty signature) must not let the row's label through.
func TestPresenceOfHalfPairs(t *testing.T) {
	full := Bundle{SigCandidate: d48("c"), EKBinding: []byte("ekb"), EKBindingSig: d48("es"),
		LegacyPopSig: []byte("pop"), LegacySPKI: []byte("spki")}
	require.Equal(t, pCandidate|pEK|pPoP, presenceOf(full))
	ap := pub{State: StateAbsent, Pin: d48("pin")}
	r, reason := selectRow(ap, OpEnrollExisting, ActorUser, presenceOf(full))
	require.Equal(t, ReasonOK, reason)
	require.Equal(t, 2, r.N)

	for name, mut := range map[string]func(*Bundle){
		"legacy_spki without legacy_pop_sig": func(b *Bundle) { b.LegacyPopSig = nil },
		"legacy_pop_sig without legacy_spki": func(b *Bundle) { b.LegacySPKI = nil },
		"ek_binding_sig without ek_binding":  func(b *Bundle) { b.EKBinding = nil },
		"ek_binding without ek_binding_sig":  func(b *Bundle) { b.EKBindingSig = nil },
	} {
		b := full
		mut(&b)
		p := presenceOf(b)
		require.NotZero(t, p&pHalf, name)
		_, reason := selectRow(ap, OpEnrollExisting, ActorUser, p)
		require.Equal(t, ReasonTransitionInvalid, reason, "%s: no row admits half a pair", name)
	}

	// A half binding proof on recover: required both, so no row.
	ac := pub{State: StateActive, Root: testPoint(t)}
	rec := Bundle{SigCandidate: d48("c"), EKBinding: []byte("ekb"), EKBindingSig: d48("es"), SigRecovery: d48("r"),
		RecoveryBind: []byte("B"), RecoveryBindSig: d48("bs")}
	r, reason = selectRow(ac, OpRecover, ActorUser, presenceOf(rec))
	require.Equal(t, ReasonOK, reason)
	require.Equal(t, 20, r.N)
	rec.RecoveryBindSig = nil
	_, reason = selectRow(ac, OpRecover, ActorUser, presenceOf(rec))
	require.Equal(t, ReasonTransitionInvalid, reason, "recovery_bind without recovery_bind_sig")

	require.Equal(t, pCurrent|pOperator|pHeld, presenceOf(Bundle{SigCurrentRoot: d48("r"), SigOperator: d48("o"),
		HeldStatus: []byte("h")}))
	require.Zero(t, presenceOf(Bundle{Intent: []byte("i"), CPAuthz: []byte("a"), CPAuthzSig: d48("s"),
		Result: []byte("r"), ResultSig: d48("t")}), "cp_authz_sig and result_sig are not row signatures")
}

// §2.8 monitor rule, applied to the client columns: a segment the caller does
// not know ("" or a value outside the four labels) is unverified. The result
// may then carry unverified and nothing stronger, and never "" — "" on a
// root-proposing or rotate row reads as "leaves assurance unchanged", which
// inherits the current label (LD-35 laundering).
func TestAllowedAssuranceUnknownSegment(t *testing.T) {
	root, cand := testPoint(t), testPoint(t)
	ac := pub{Seq: 4, State: StateActive, Root: root, EK: d48("ek"), Pin: d48("pin"), RG: 2}
	in := Intent{CandidateRoot: cand, EKBindingDigest: d48("ek2")}
	row5, _ := RowByN(5)
	row11, _ := RowByN(11)
	r5 := Result{State: StateActive, SuspendedBy: []string{}, ActiveRoot: cand, EKBindingDigest: d48("ek2"),
		LegacyEKDigest: d48("pin"), RecoveryGen: 2}
	r11 := Result{State: StatePending, PriorState: StateActive, SuspendedBy: []string{}, ActiveRoot: root,
		EKBindingDigest: d48("ek"), PendingRoot: cand, PendingEKBindingDigest: d48("ek2"), LegacyEKDigest: d48("pin"),
		RecoveryGen: 3}
	for _, seg := range []Assurance{"", "bogus"} {
		for a, want := range map[string]Reason{"": ReasonTransitionInvalid, AssuranceUnverified: ReasonOK,
			AssuranceRecovery: ReasonTransitionInvalid, AssuranceContinuity: ReasonTransitionInvalid,
			AssuranceInitial: ReasonTransitionInvalid} {
			r5.Assurance = a
			require.Equal(t, want, checkPost(row5, ac, rowCtx{in: in, res: r5, segment: seg}), "row 5, segment %q, %q", seg, a)
			r11.Assurance = a
			require.Equal(t, want, checkPost(row11, ac, rowCtx{in: in, res: r11, outcomeP: true, segment: seg}),
				"row 11, segment %q, %q", seg, a)
		}
	}
	require.Equal(t, []string{AssuranceUnverified}, allowedAssurance(row5, ac, rowCtx{}))
	require.Equal(t, []string{AssuranceUnverified}, allowedAssurance(row11, ac, rowCtx{outcomeP: true}))
	row20, _ := RowByN(20)
	require.Equal(t, []string{AssuranceUnverified}, allowedAssurance(row20, ac, rowCtx{outcomeP: true}),
		"an unknown binding segment is unverified (§2.8: 'or for a recover the binding segment's')")
	require.Equal(t, []string{AssuranceUnverified}, allowedAssurance(row20, ac, rowCtx{outcomeP: true, bindingSegment: "bogus"}))
}

// Row 20 (LD-35, D-254, the Δ13 cap): no stronger than min(recovery, the
// binding segment); the current segment does not cap it; "" is never allowed.
func TestCheckPostRow20Cap(t *testing.T) {
	root, cand := testPoint(t), testPoint(t)
	row20, _ := RowByN(20)
	f := pub{Seq: 6, State: StateActive, Root: root, EK: d48("ek"), Pin: d48("pin"), RG: 3}
	res := Result{State: StatePending, PriorState: StateActive, SuspendedBy: []string{}, ActiveRoot: root,
		EKBindingDigest: d48("ek"), PendingRoot: cand, PendingEKBindingDigest: d48("ek2"), LegacyEKDigest: d48("pin"),
		RecoveryGen: 4}
	c := rowCtx{in: Intent{CandidateRoot: cand, EKBindingDigest: d48("ek2")}, res: res, outcomeP: true}
	cases := []struct {
		binding, current Assurance
		claim            string
		want             Reason
	}{
		{AssuranceInitial, AssuranceInitial, AssuranceRecovery, ReasonOK},
		{AssuranceInitial, AssuranceUnverified, AssuranceRecovery, ReasonOK}, // a thief's segment does not cap it
		{AssuranceContinuity, AssuranceInitial, AssuranceRecovery, ReasonOK},
		{AssuranceUnverified, AssuranceInitial, AssuranceRecovery, ReasonTransitionInvalid}, // never launders
		{AssuranceUnverified, AssuranceInitial, AssuranceUnverified, ReasonOK},
		{AssuranceInitial, AssuranceInitial, AssuranceUnverified, ReasonOK}, // force_unverified
		{AssuranceInitial, AssuranceInitial, AssuranceContinuity, ReasonTransitionInvalid},
		{AssuranceInitial, AssuranceInitial, AssuranceInitial, ReasonTransitionInvalid},
		{AssuranceInitial, AssuranceInitial, "", ReasonTransitionInvalid},
	}
	for _, tc := range cases {
		c.bindingSegment, c.segment, c.res.Assurance = tc.binding, tc.current, tc.claim
		require.Equal(t, tc.want, checkPost(row20, f, c), "binding %q current %q claim %q", tc.binding, tc.current, tc.claim)
	}
}

// postCase is one §2.9 row, its from-state, and the result the DoR's To,
// Δrg and assurance columns describe — written from the row text, not from
// expected(). bad is a single-field change the row does not admit.
type postCase struct {
	name string
	n    int
	f    pub
	c    rowCtx
	bad  func(*Result)
}

func postCases(t *testing.T) []postCase {
	root, cand, old := testPoint(t), testPoint(t), testPoint(t)
	pin, ek, ek2 := d48("pin"), d48("ek"), d48("ek2")
	in := Intent{CandidateRoot: cand, EKBindingDigest: ek2}
	none := []string{}
	ini := Assurance(AssuranceInitial)
	return []postCase{
		{"1 A0→Ac", 1, pub{State: StateAbsent}, rowCtx{in: in, res: Result{State: StateActive, SuspendedBy: none,
			ActiveRoot: cand, EKBindingDigest: ek2, Assurance: AssuranceInitial}},
			func(r *Result) { r.ActiveRoot = root }},
		{"2 Ap→P(absent)", 2, pub{Seq: 1, State: StateAbsent, Pin: pin, RG: 1}, rowCtx{in: in, outcomeP: true,
			res: Result{State: StatePending, PriorState: StateAbsent, SuspendedBy: none, PendingRoot: cand,
				PendingEKBindingDigest: ek2, LegacyEKDigest: pin, RecoveryGen: 2, Assurance: AssuranceContinuity}},
			func(r *Result) { r.Assurance = AssuranceRecovery }},
		{"3 Apr→P(absent)", 3, pub{Seq: 1, State: StateAbsent, Pin: pin, PinRevoked: 1, RG: 1}, rowCtx{in: in, outcomeP: true,
			res: Result{State: StatePending, PriorState: StateAbsent, SuspendedBy: none, PendingRoot: cand,
				PendingEKBindingDigest: ek2, LegacyEKDigest: pin, LegacyPinRevoked: 1, RecoveryGen: 2,
				Assurance: AssuranceUnverified}},
			func(r *Result) { r.Assurance = AssuranceRecovery }},
		{"3 Ap→D(absent)", 3, pub{Seq: 1, State: StateAbsent, Pin: pin, RG: 1}, rowCtx{in: in,
			res: Result{State: StateDisputed, PriorState: StateAbsent, SuspendedBy: none, LegacyEKDigest: pin, RecoveryGen: 2}},
			func(r *Result) { r.PendingRoot = cand }},
		{"4 P(absent)→D(absent)", 4, pub{Seq: 2, State: StatePending, Prior: StateAbsent, PendRoot: old, PendEK: ek,
			Pin: pin, RG: 2}, rowCtx{in: in, res: Result{State: StateDisputed, PriorState: StateAbsent, SuspendedBy: none,
			LegacyEKDigest: pin, RecoveryGen: 3}},
			func(r *Result) { r.PendingRoot, r.PendingEKBindingDigest = old, ek }}, // the competitor's candidate burned
		{"6 Ac→Ac", 6, pub{Seq: 4, State: StateActive, Root: root, EK: ek, Pin: pin, RG: 2}, rowCtx{in: in,
			res: Result{State: StateActive, SuspendedBy: none, ActiveRoot: root, EKBindingDigest: ek2, LegacyEKDigest: pin,
				RecoveryGen: 2}},
			func(r *Result) { r.EKBindingDigest = ek }},
		{"7 So→set ∪ {user}", 7, pub{Seq: 4, State: StateSuspended, Susp: []string{ActorOperator}, Root: root, EK: ek, Pin: pin,
			RG: 2}, rowCtx{res: Result{State: StateSuspended, SuspendedBy: []string{ActorOperator, ActorUser}, ActiveRoot: root,
			EKBindingDigest: ek, LegacyEKDigest: pin, RecoveryGen: 2}},
			func(r *Result) { r.SuspendedBy = []string{ActorUser} }},
		{"8 Su→set ∪ {operator}", 8, pub{Seq: 4, State: StateSuspended, Susp: []string{ActorUser}, Root: root, EK: ek, Pin: pin,
			RG: 2}, rowCtx{res: Result{State: StateSuspended, SuspendedBy: []string{ActorOperator, ActorUser}, ActiveRoot: root,
			EKBindingDigest: ek, LegacyEKDigest: pin, RecoveryGen: 2}},
			func(r *Result) { r.RecoveryGen = 3 }},
		{"9 Su→Ac", 9, pub{Seq: 4, State: StateSuspended, Susp: []string{ActorUser}, Root: root, EK: ek, Pin: pin, RG: 2},
			rowCtx{res: Result{State: StateActive, SuspendedBy: none, ActiveRoot: root, EKBindingDigest: ek,
				LegacyEKDigest: pin, RecoveryGen: 2}},
			func(r *Result) { r.State = StateSuspended }},
		{"10 So→set \\ {operator}", 10, pub{Seq: 4, State: StateSuspended, Susp: []string{ActorOperator, ActorUser}, Root: root,
			EK: ek, Pin: pin, RG: 2}, rowCtx{res: Result{State: StateSuspended, SuspendedBy: []string{ActorUser},
			ActiveRoot: root, EKBindingDigest: ek, LegacyEKDigest: pin, RecoveryGen: 2}},
			func(r *Result) { r.SuspendedBy = []string{ActorOperator} }},
		{"11 Sor→P(suspended), unverified", 11, pub{Seq: 5, State: StateSuspended, Susp: []string{ActorOperator},
			Root: root, RootRevoked: 1, EK: ek, Pin: pin, RG: 3}, rowCtx{in: in, outcomeP: true, segment: ini,
			res: Result{State: StatePending, PriorState: StateSuspended, SuspendedBy: []string{ActorOperator},
				ActiveRoot: root, RootRevoked: 1, EKBindingDigest: ek, PendingRoot: cand, PendingEKBindingDigest: ek2,
				LegacyEKDigest: pin, RecoveryGen: 4, Assurance: AssuranceUnverified}},
			func(r *Result) { r.Assurance = AssuranceRecovery }}, // Sor: unverified only
		{"13 P(suspended)→Su", 13, pub{Seq: 6, State: StatePending, Prior: StateSuspended, Susp: []string{ActorUser},
			Root: root, EK: ek, PendRoot: cand, PendEK: ek2, Pin: pin, RG: 4}, rowCtx{res: Result{State: StateSuspended,
			SuspendedBy: []string{ActorUser}, ActiveRoot: root, EKBindingDigest: ek, LegacyEKDigest: pin, RecoveryGen: 5}},
			func(r *Result) { r.PendingRoot = cand }},
		{"14 D(absent)→Ap", 14, pub{Seq: 3, State: StateDisputed, Prior: StateAbsent, Pin: pin, RG: 3},
			rowCtx{res: Result{State: StateAbsent, SuspendedBy: none, LegacyEKDigest: pin, RecoveryGen: 4}},
			func(r *Result) { r.LegacyPinRevoked = 1 }},
		{"15 Ac→Sor", 15, pub{Seq: 4, State: StateActive, Root: root, EK: ek, Pin: pin, RG: 2},
			rowCtx{res: Result{State: StateSuspended, SuspendedBy: []string{ActorOperator}, ActiveRoot: root, RootRevoked: 1,
				EKBindingDigest: ek, LegacyEKDigest: pin, RecoveryGen: 3}},
			func(r *Result) { r.RootRevoked = 0 }},
		{"16 P(active)→Sor", 16, pub{Seq: 5, State: StatePending, Prior: StateActive, Root: root, EK: ek, PendRoot: cand,
			PendEK: ek2, Pin: pin, RG: 3}, rowCtx{res: Result{State: StateSuspended, SuspendedBy: []string{ActorOperator},
			ActiveRoot: root, RootRevoked: 1, EKBindingDigest: ek, LegacyEKDigest: pin, RecoveryGen: 4}},
			func(r *Result) { r.ActiveRoot = cand }},
		{"17 Ap→Apr", 17, pub{Seq: 1, State: StateAbsent, Pin: pin, RG: 1},
			rowCtx{res: Result{State: StateAbsent, SuspendedBy: none, LegacyEKDigest: pin, LegacyPinRevoked: 1, RecoveryGen: 2}},
			func(r *Result) { r.LegacyEKDigest = nil }}, // the pin is carried, revoked
		{"18 D(suspended)→Pe(suspended)", 18, pub{Seq: 6, State: StateDisputed, Prior: StateSuspended,
			Susp: []string{ActorUser}, Root: root, EK: ek, Pin: pin, RG: 4}, rowCtx{res: Result{State: StatePending,
			PriorState: StateSuspended, SuspendedBy: []string{ActorUser}, ActiveRoot: root, EKBindingDigest: ek,
			LegacyEKDigest: pin, RecoveryGen: 5}},
			func(r *Result) { r.PriorState = StateDisputed }},
		{"19 P(active)→E", 19, pub{Seq: 5, State: StatePending, Prior: StateActive, Root: root, EK: ek, PendRoot: cand,
			PendEK: ek2, Pin: pin, RG: 3}, rowCtx{res: Result{State: StateErased, SuspendedBy: none, ActiveRoot: root,
			EKBindingDigest: ek, LegacyEKDigest: pin, RecoveryGen: 3}},
			func(r *Result) { r.ActiveRoot = nil }}, // the from-state root kept
		{"20 Pe(active)→Pr(active)", 20, pub{Seq: 5, State: StatePending, Prior: StateActive, Root: root, EK: ek, Pin: pin,
			RG: 3, Op: OpErase}, rowCtx{in: in, outcomeP: true, bindingSegment: ini, res: Result{State: StatePending,
			PriorState: StateActive, SuspendedBy: none, ActiveRoot: root, EKBindingDigest: ek, PendingRoot: cand,
			PendingEKBindingDigest: ek2, LegacyEKDigest: pin, RecoveryGen: 4, Assurance: AssuranceRecovery}},
			func(r *Result) { r.Assurance = "" }},
		{"21 Pr(suspended)→Su", 21, pub{Seq: 6, State: StatePending, Prior: StateSuspended, Susp: []string{ActorUser},
			Root: root, EK: ek, PendRoot: cand, PendEK: ek2, Pin: pin, RG: 4, Op: OpRecover},
			rowCtx{res: Result{State: StateSuspended, SuspendedBy: []string{ActorUser}, ActiveRoot: root, EKBindingDigest: ek,
				LegacyEKDigest: pin, RecoveryGen: 5}},
			func(r *Result) { r.SuspendedBy = none }},
		{"22 D(absent)→Apr, the pin revocation carried", 22, pub{Seq: 4, State: StateDisputed, Prior: StateAbsent, Pin: pin,
			PinRevoked: 1, RG: 3}, rowCtx{res: Result{State: StateAbsent, SuspendedBy: none, LegacyEKDigest: pin,
			LegacyPinRevoked: 1, RecoveryGen: 4}},
			func(r *Result) { r.LegacyPinRevoked = 0 }},
	}
}

func TestCheckPostEveryToRule(t *testing.T) {
	for _, tc := range postCases(t) {
		row, ok := RowByN(tc.n)
		require.True(t, ok)
		require.Equal(t, ReasonOK, checkPost(row, tc.f, tc.c), tc.name)
		c := tc.c
		tc.bad(&c.res)
		require.Equal(t, ReasonTransitionInvalid, checkPost(row, tc.f, c), "%s: the change the row does not admit", tc.name)
		c = tc.c
		c.res.RecoveryGen++
		require.Equal(t, ReasonTransitionInvalid, checkPost(row, tc.f, c), "%s: Δrg", tc.name)
		c = tc.c
		c.res.LegacyEKDigest = d48("other")
		require.Equal(t, ReasonTransitionInvalid, checkPost(row, tc.f, c), "%s: the pin is immutable", tc.name)
	}
	// The seq-0 hypothesis carries the seq-0 pin the caller knows (V3-05,
	// D-149; chain.go), "" included, so the first result's #18 is compared
	// with it like every later one: a result cannot introduce a pin.
	row1, _ := RowByN(1)
	first := postCases(t)[0]
	first.c.res.LegacyEKDigest = d48("first-sight")
	require.Equal(t, ReasonTransitionInvalid, checkPost(row1, first.f, first.c), "seq 0: no pin known, none introduced")
	require.Equal(t, ReasonTransitionInvalid, checkPost(Row{}, pub{State: StateAbsent}, rowCtx{res: Result{State: StateAbsent}}),
		"the zero Row admits nothing")
}

// TestCheckPostRefusesAPinInjectedAtSeqZero pins the path the seq-0 exemption
// opened: A0 -> a row-18 erase whose result carries a pin -> Pe(absent) "with a
// live pin" -> a row-14 PoP cancel -> Ap -> a row-2 enrolment labelled
// continuity under the injector's legacy key. Its first step is refused, and
// so is every step that would introduce a pin the seq-0 hypothesis lacks.
func TestCheckPostRefusesAPinInjectedAtSeqZero(t *testing.T) {
	injected := d48("injected")
	none := []string{}
	row18, _ := RowByN(18)
	erase := rowCtx{res: Result{State: StatePending, PriorState: StateAbsent, SuspendedBy: none, RecoveryGen: 1}}
	require.Equal(t, ReasonOK, checkPost(row18, genesisPub(), erase), "control: the erase from A0 without a pin")
	erase.res.LegacyEKDigest = injected
	require.Equal(t, ReasonTransitionInvalid, checkPost(row18, genesisPub(), erase), "an erase from A0 cannot introduce a pin")

	// From a Pe(absent) head whose pin is "", the row-14 cancel's To (Ap)
	// carries "", so it cannot introduce one either.
	row14, _ := RowByN(14)
	pe := pub{Seq: 1, State: StatePending, Prior: StateAbsent, RG: 1, Op: OpErase}
	cancel := rowCtx{res: Result{State: StateAbsent, SuspendedBy: none, LegacyEKDigest: injected, RecoveryGen: 2}}
	require.Equal(t, ReasonTransitionInvalid, checkPost(row14, pe, cancel), "a cancel cannot introduce a pin")

	// A seq-0 hypothesis that carries the first-sight pin accepts that pin and
	// refuses any other, the row-2 enrolment included.
	row2, _ := RowByN(2)
	pin := d48("first-sight")
	ap := pub{State: StateAbsent, Pin: pin}
	in := Intent{CandidateRoot: testPoint(t), EKBindingDigest: d48("ek")}
	enrol := rowCtx{in: in, outcomeP: true, res: Result{State: StatePending, PriorState: StateAbsent, SuspendedBy: none,
		PendingRoot: in.CandidateRoot, PendingEKBindingDigest: in.EKBindingDigest, LegacyEKDigest: pin, RecoveryGen: 1,
		Assurance: AssuranceContinuity}}
	require.Equal(t, ReasonOK, checkPost(row2, ap, enrol), "control: the enrolment under the first-sight pin")
	enrol.res.LegacyEKDigest = injected
	require.Equal(t, ReasonTransitionInvalid, checkPost(row2, ap, enrol), "an enrolment under another pin")
}

// Fail-closed defaults: an op §2.7 does not list, a PoP digest with no pin to
// equal, and a row selected for another op.
func TestPostFailsClosed(t *testing.T) {
	require.Equal(t, RefusalMalformedForOp, IntentForOp(Intent{Op: OpAddDevice}, ActorUser, false, false),
		"a private op has no published intent")
	require.Equal(t, RefusalMalformedForOp, IntentForOp(Intent{Op: "bogus"}, ActorUser, false, false))

	row14, _ := RowByN(14)
	f := pub{Seq: 3, Entry: d48("e3"), State: StateDisputed, Prior: StateAbsent, RG: 3}
	in := Intent{Op: OpCancelPending, ExpectedSeq: 3, ExpectedRecoveryGen: 3, ExpectedHeadDigest: d48("e3"),
		LegacyEKDigest: d48("pin")}
	require.Equal(t, ReasonTransitionInvalid, checkIntentValues(row14, f, in), "= pin iff PoP: no pin, no PoP")
	f.Pin = d48("pin")
	require.Equal(t, ReasonOK, checkIntentValues(row14, f, in))
	in.LegacyEKDigest = d48("other")
	require.Equal(t, ReasonTransitionInvalid, checkIntentValues(row14, f, in))
	in.LegacyEKDigest = d48("pin")
	in.Op = OpRevokeRoot
	require.Equal(t, ReasonTransitionInvalid, checkIntentValues(row14, f, in), "a row selected for another op")

	row1, _ := RowByN(1)
	enroll := Intent{Op: OpEnrollNew}
	require.Equal(t, ReasonOK, checkIntentValues(row1, genesisPub(), enroll))
	enroll.ExpectedHeadDigest = d48("e0")
	require.Equal(t, ReasonTransitionInvalid, checkIntentValues(row1, genesisPub(), enroll), "A4: \"\" exactly at seq 0")
}

// ── Review-fix additions: every §2.7 position and the From-column guards ────

// intentRule is one §2.7 cell (DoR:535–549) as the table states it, written
// from the DoR row rather than from intentNeeds, so a mutant in one reading
// shows against the other.
type intentRule string

const (
	irReq  intentRule = "req"  // req
	irNone intentRule = "-"    // —: MUST be ""
	irPoP  intentRule = "pop"  // = pin iff PoP
	irRoot intentRule = "root" // active root iff the root cancels
)

// intentRow is §2.7's row for one (op, actor), in column order:
// candidate_root, ek_binding_digest, legacy_ek_digest,
// predecessor_root_digest, device_intent_digest (#15), recovery_bind_digest
// (#16). expected_head_digest is decision A4's (checkIntentValues).
type intentRow struct {
	op, actor string
	cells     [6]intentRule
}

func intentPositionNames() [6]string {
	return [6]string{"candidate_root", "ek_binding_digest", "legacy_ek_digest", "predecessor_root_digest",
		"device_intent_digest", "recovery_bind_digest"}
}

func intentRows() []intentRow {
	none := [6]intentRule{irNone, irNone, irNone, irNone, irNone, irNone}
	return []intentRow{
		{OpEnrollNew, ActorUser, [6]intentRule{irReq, irReq, irNone, irNone, irReq, irNone}},
		{OpEnrollExisting, ActorUser, [6]intentRule{irReq, irReq, irPoP, irNone, irReq, irNone}},
		{OpRotateRoot, ActorUser, [6]intentRule{irReq, irReq, irNone, irReq, irReq, irNone}},
		{OpRotateEK, ActorUser, [6]intentRule{irNone, irReq, irNone, irReq, irReq, irNone}},
		{OpSuspend, ActorUser, none},
		{OpSuspend, ActorOperator, none},
		{OpUnsuspend, ActorUser, [6]intentRule{irNone, irNone, irNone, irReq, irReq, irNone}},
		{OpUnsuspend, ActorOperator, [6]intentRule{irNone, irNone, irNone, irReq, irNone, irNone}},
		{OpReplaceRoot, ActorUser, [6]intentRule{irReq, irReq, irNone, irReq, irReq, irNone}},
		{OpRecover, ActorUser, [6]intentRule{irReq, irReq, irNone, irReq, irReq, irReq}},
		{OpCancelPending, ActorUser, [6]intentRule{irNone, irNone, irPoP, irRoot, irNone, irNone}},
		{OpCancelPending, ActorRestore, [6]intentRule{irNone, irNone, irPoP, irRoot, irNone, irNone}},
		{OpRevokeRoot, ActorOperator, none},
		{OpErase, ActorUser, none},
		{OpErase, ActorOperator, none},
	}
}

func intentPositions(in *Intent) [6]*[]byte {
	return [6]*[]byte{&in.CandidateRoot, &in.EKBindingDigest, &in.LegacyEKDigest, &in.PredecessorRootDigest,
		&in.DeviceIntentDigest, &in.RecoveryBindDigest}
}

// cellPresent is whether a valid intent fills a cell under these flags.
func cellPresent(t *testing.T, r intentRule, pop, rootCancels bool) bool {
	t.Helper()
	switch r {
	case irReq:
		return true
	case irNone:
		return false
	case irPoP:
		return pop
	case irRoot:
		return rootCancels
	}
	require.FailNow(t, "unknown §2.7 cell", "%q", r)
	return false
}

// validIntent fills exactly the cells the row requires under these flags.
func validIntent(t *testing.T, row intentRow, fill [6][]byte, pop, rootCancels bool) Intent {
	t.Helper()
	in := Intent{RealmID: newID(), AccountID: newID(), RequestID: newID(), Op: row.op, ExpiresAtMs: 1}
	pos := intentPositions(&in)
	for i, r := range row.cells {
		if cellPresent(t, r, pop, rootCancels) {
			*pos[i] = fill[i]
		}
	}
	return in
}

// flipPosition empties a filled position and fills an empty one.
func flipPosition(in Intent, i int, fill [6][]byte) Intent {
	p := intentPositions(&in)[i]
	if len(*p) > 0 {
		*p = nil
		return in
	}
	*p = fill[i]
	return in
}

// Every published op's valid intent is admitted under every (pop,
// rootCancels) pair, and flipping any ONE of the six positions is
// malformed_for_op: a required position emptied, a "—" position filled, an
// "iff" position set against its flag (§2.7: "Any position not listed MUST be
// \"\""). The rows cover all eleven published ops, so dropping an op from
// intentNeeds turns its valid intents into malformed_for_op, refusing intents
// the authority admits (A6).
func TestIntentForOpEveryOpEveryPosition(t *testing.T) {
	fill := [6][]byte{testPoint(t), d48("ek"), d48("pin"), d48("pred"), d48("D"), d48("B")}
	names := intentPositionNames()
	seen := map[string]bool{}
	for _, row := range intentRows() {
		seen[row.op] = true
		for _, flags := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
			pop, rootCancels := flags[0], flags[1]
			label := row.op + "/" + row.actor
			in := validIntent(t, row, fill, pop, rootCancels)
			require.Equal(t, Refusal(""), IntentForOp(in, row.actor, pop, rootCancels),
				"%s pop=%t rootCancels=%t: the valid intent", label, pop, rootCancels)
			for i := range row.cells {
				require.Equal(t, RefusalMalformedForOp, IntentForOp(flipPosition(in, i, fill), row.actor, pop, rootCancels),
					"%s pop=%t rootCancels=%t: %s flipped", label, pop, rootCancels, names[i])
			}
		}
	}
	ops := make([]string, 0, len(seen))
	for op := range seen {
		ops = append(ops, op)
	}
	require.ElementsMatch(t, publishedOps, ops, "the rows cover exactly the eleven published ops")
}

// The From-column guards (§2.9 rows 4, 7, 9, 10, 12, 13, 14, 16, 17), each
// with an admitted and a refused from-state. A guard value the table does not
// define holds nowhere, so a row carrying one is never selected rather than
// selected unguarded.
func TestGuardHolds(t *testing.T) {
	root, pin := testPoint(t), d48("pin")
	pending := func(prior string) pub { return pub{State: StatePending, Prior: prior} }
	cases := []struct {
		name string
		g    guard
		f    pub
		want bool
	}{
		{"gNone holds everywhere", gNone, pub{}, true},
		{"row 7: user ∉ set", gUserNotInSet, pub{State: StateSuspended, Susp: []string{ActorOperator}}, true},
		{"row 7: user ∈ set", gUserNotInSet, pub{State: StateSuspended, Susp: []string{ActorUser}}, false},
		{"row 9: user ∈ set, unrevoked", gUserInSetUnrevoked, pub{Susp: []string{ActorUser}, Root: root}, true},
		{"row 9: root_revoked = 1", gUserInSetUnrevoked, pub{Susp: []string{ActorUser}, Root: root, RootRevoked: 1}, false},
		{"row 9: user ∉ set", gUserInSetUnrevoked, pub{Susp: []string{ActorOperator}, Root: root}, false},
		{"row 10: operator ∈ set, unrevoked", gOpInSetUnrevoked, pub{Susp: []string{ActorOperator}, Root: root}, true},
		{"row 10: root_revoked = 1", gOpInSetUnrevoked, pub{Susp: []string{ActorOperator}, Root: root, RootRevoked: 1}, false},
		{"row 10: operator ∉ set", gOpInSetUnrevoked, pub{Susp: []string{ActorUser}, Root: root}, false},
		{"row 4: prior absent", gPriorAbsent, pending(StateAbsent), true},
		{"row 4: prior active", gPriorAbsent, pending(StateActive), false},
		{"row 12: prior active", gPriorActiveOrSusp, pending(StateActive), true},
		{"row 12: prior suspended", gPriorActiveOrSusp, pending(StateSuspended), true},
		{"row 12: prior absent", gPriorActiveOrSusp, pending(StateAbsent), false},
		{"row 13: a root, unrevoked", gRootUnrevoked, pub{Root: root}, true},
		{"row 13: root_revoked = 1", gRootUnrevoked, pub{Root: root, RootRevoked: 1}, false},
		{"row 13: no root", gRootUnrevoked, pub{}, false},
		{"row 14: prior absent, live pin", gLivePinPriorAbsent, pub{Prior: StateAbsent, Pin: pin}, true},
		{"row 14: pin revoked", gLivePinPriorAbsent, pub{Prior: StateAbsent, Pin: pin, PinRevoked: 1}, false},
		{"row 14: no pin", gLivePinPriorAbsent, pub{Prior: StateAbsent}, false},
		{"row 14: prior active", gLivePinPriorAbsent, pub{Prior: StateActive, Pin: pin}, false},
		{"row 16: prior active", gPriorNotAbsent, pending(StateActive), true},
		{"row 16: prior absent", gPriorNotAbsent, pending(StateAbsent), false},
		{"row 17: Ap", gAbsentPrior, pub{State: StateAbsent, Pin: pin}, true},
		{"row 17: P(absent)", gAbsentPrior, pending(StateAbsent), true},
		{"row 17: P(active)", gAbsentPrior, pending(StateActive), false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, guardHolds(tc.g, tc.f), tc.name)
	}
	for _, g := range []guard{gAbsentPrior + 1, guard(255)} {
		for _, f := range []pub{{}, genesisPub(), {State: StateActive, Root: root}, pending(StateAbsent),
			{State: StateAbsent, Pin: pin}} {
			require.False(t, guardHolds(g, f), "an undefined guard %d holds nowhere (state %q)", g, f.State)
		}
	}
}

// guardCase is one selectRow call; want 0 is transition_invalid.
type guardCase struct {
	name      string
	f         pub
	op, actor string
	p         presence
	want      int
}

func guardCases(t *testing.T) []guardCase {
	root, cand, pin := testPoint(t), testPoint(t), d48("pin")
	// h is a P(x) or D(x) head: a root unless x is absent, a pending root when P.
	h := func(state, prior string, susp []string, rr uint64) pub {
		f := pub{State: state, Prior: prior, Susp: susp, Root: root, RootRevoked: rr, Pin: pin}
		if prior == StateAbsent {
			f.Root = nil
		}
		if state == StatePending {
			f.PendRoot = cand
		}
		return f
	}
	pe := h(StatePending, StateSuspended, []string{ActorUser}, 0)
	pe.PendRoot = nil // Pe(suspended): a pending head with no pending root
	peAbsent := h(StatePending, StateAbsent, nil, 0)
	peAbsent.PendRoot = nil
	dAbsentRevokedPin := h(StateDisputed, StateAbsent, nil, 0)
	dAbsentRevokedPin.PinRevoked = 1
	pActive, pAbsent, dAbsent := h(StatePending, StateActive, nil, 0), h(StatePending, StateAbsent, nil, 0),
		h(StateDisputed, StateAbsent, nil, 0)
	dSusp := h(StateDisputed, StateSuspended, []string{ActorUser}, 0)
	cancel, revoke, replace := OpCancelPending, OpRevokeRoot, OpReplaceRoot
	return []guardCase{
		// Row 13: the root cancels only while root_revoked = 0. After revoke_root
		// (Sor) the root that cancelled can cancel no more, which is what lets the
		// owner's replace_root run (§2.9 containment escape, steps 1–3).
		{"13: P(active), root unrevoked", pActive, cancel, ActorUser, pCurrent, 13},
		{"13: Pe(suspended)", pe, cancel, ActorUser, pCurrent, 13},
		{"13: P(suspended) from Sor, root_revoked = 1", h(StatePending, StateSuspended, []string{ActorOperator}, 1),
			cancel, ActorUser, pCurrent, 0},
		{"13: P(absent), no root", pAbsent, cancel, ActorUser, pCurrent, 0},
		// Row 14: the PoP cancels only with a live pin.
		{"14: D(absent), live pin", dAbsent, cancel, ActorUser, pPoP, 14},
		{"14: Pe(absent), live pin", peAbsent, cancel, ActorUser, pPoP, 14},
		{"14: D(absent), pin revoked", dAbsentRevokedPin, cancel, ActorUser, pPoP, 0},
		{"14: P(active), prior not absent", pActive, cancel, ActorUser, pPoP, 0},
		// Rows 16 and 17: revoke_root from P or D is Sor unless the prior is
		// absent, which is Apr (row 16 "x ≠ absent"; row 17 "P(absent), D(absent)").
		{"16: P(active)", pActive, revoke, ActorOperator, pOperator, 16},
		{"16: D(suspended)", dSusp, revoke, ActorOperator, pOperator, 16},
		{"17 not 16: P(absent)", pAbsent, revoke, ActorOperator, pOperator, 17},
		{"17 not 16: D(absent)", dAbsent, revoke, ActorOperator, pOperator, 17},
		{"17: Ap", pub{State: StateAbsent, Pin: pin}, revoke, ActorOperator, pOperator, 17},
		// Row 12: a replace_root competitor needs x ∈ {active, suspended}.
		{"12: P(active)", pActive, replace, ActorUser, pCandidate | pEK, 12},
		{"12: D(suspended)", dSusp, replace, ActorUser, pCandidate | pEK, 12},
		{"12: P(absent)", pAbsent, replace, ActorUser, pCandidate | pEK, 0},
		{"12: D(absent)", dAbsent, replace, ActorUser, pCandidate | pEK, 0},
	}
}

// The guards as selectRow applies them: one admitted and one refused case for
// each guarded row a single-guard mutant reaches.
func TestSelectRowGuards(t *testing.T) {
	for _, tc := range guardCases(t) {
		r, reason := selectRow(tc.f, tc.op, tc.actor, tc.p)
		if tc.want == 0 {
			require.Equal(t, ReasonTransitionInvalid, reason, tc.name)
			require.Zero(t, r.N, tc.name)
			continue
		}
		require.Equal(t, ReasonOK, reason, tc.name)
		require.Equal(t, tc.want, r.N, tc.name)
	}
}

// Row 1 admits only an account outside the population (DoR:680), whose pin is
// never set (DoR:337 "set at a population account's first event"): a row-1
// result carries legacy_ek_digest "" even when the hypothesis holds a pin (a
// first-sight pin read from an unverified status, chain.go). checkPost's
// always-compare alone would admit a row-1 result that repeats that pin; the
// population check refuses it. The authority never issues a row-1 result with
// a pin, so this is never stricter than it (A6).
func TestCheckPostRow1CarriesNoPin(t *testing.T) {
	row1, _ := RowByN(1)
	first := postCases(t)[0]
	require.Equal(t, 1, first.n)
	require.Equal(t, ReasonOK, checkPost(row1, first.f, first.c))
	first.c.res.LegacyEKDigest = d48("first-sight")
	require.Equal(t, ReasonTransitionInvalid, checkPost(row1, first.f, first.c), "a row-1 result introducing a pin")
	// The hypothesis knows a pin and the result repeats it: only the
	// population check refuses this one.
	known := first
	known.f.Pin = d48("first-sight")
	known.c.res.LegacyEKDigest = d48("first-sight")
	require.Equal(t, ReasonTransitionInvalid, checkPost(row1, known.f, known.c), "a row-1 result repeating a known pin")
}
