package authoritycontract

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func rowN(t *testing.T, n int) Row {
	t.Helper()
	r, ok := RowByN(n)
	require.True(t, ok, "row %d", n)
	return r
}

func TestTableHasRows1to22InOrder(t *testing.T) {
	rows := Rows()
	require.Len(t, rows, 22)
	for i, r := range rows {
		require.Equal(t, i+1, r.N)
		require.NotEmpty(t, r.From, "row %d From", r.N)
		require.NotEmpty(t, r.Floors, "row %d has an authority-only floor entry (DoR:678)", r.N)
	}
	for _, op := range []string{OpAddDevice, OpBindRecovery, OpCancelRecoveryBind} {
		r, ok := PrivateRow(op)
		require.True(t, ok, op)
		require.Zero(t, r.N)
		require.NotEmpty(t, r.Floors, op)
	}
	_, ok := PrivateRow(OpRotateEK)
	require.False(t, ok)
}

// LD-34: only the recovery key ends a pending recover, except row 22.
func TestRowsThatExcludePr(t *testing.T) {
	for _, n := range []int{4, 11, 12, 13, 14, 16, 17, 18, 19} {
		require.False(t, slices.Contains(rowN(t, n).From, FromPr), "row %d admits Pr", n)
	}
	for _, n := range []int{20, 21, 22} {
		require.True(t, slices.Contains(rowN(t, n).From, FromPr), "row %d must admit Pr", n)
	}
	require.False(t, slices.Contains(rowN(t, 20).From, FromE))
	require.True(t, slices.Contains(rowN(t, 19).From, FromPe), "operator erase admits Pe")
	require.False(t, slices.Contains(rowN(t, 18).From, FromPe), "user erase excludes Pe")
	require.False(t, slices.Contains(rowN(t, 11).From, FromSo), "row 11: not So")
}

func TestAuthorityOnlyColumns(t *testing.T) {
	want := map[int][]FloorName{1: {FloorREG}, 2: {FloorS}, 3: {FloorRFull, FloorREmail}, 4: {FloorRFull, FloorREmail},
		5: {FloorS}, 6: {FloorS}, 7: {FloorH}, 8: {FloorOP}, 9: {FloorH}, 10: {FloorOP},
		11: {FloorRFull, FloorREmail, FloorRRk}, 12: {FloorRFull, FloorREmail, FloorRRk}, 13: {FloorNone}, 14: {FloorNone},
		15: {FloorOP}, 16: {FloorOP}, 17: {FloorOP}, 18: {FloorH, FloorEraseList}, 19: {FloorOP}, 20: {FloorNone},
		21: {FloorNone}, 22: {FloorNone}}
	for n, f := range want {
		require.Equal(t, f, rowN(t, n).Floors, "row %d floors", n)
	}
	require.Equal(t, []FloorName{FloorS}, rowN(t, 4).PoPFloors)
	for op, f := range map[string]FloorName{OpAddDevice: FloorS, OpBindRecovery: FloorH, OpCancelRecoveryBind: FloorNone} {
		r, _ := PrivateRow(op)
		require.Equal(t, []FloorName{f}, r.Floors, op)
	}
	for n := 1; n <= 22; n++ {
		r := rowN(t, n)
		held := HeldRefuse
		switch n {
		case 16, 17, 19:
			held = HeldExempt
		case 18:
			held = HeldCommitsPe
		case 22:
			held = HeldOnly
		}
		require.Equal(t, held, r.Held, "row %d held", n)
		fu := FUKeep
		switch n {
		case 1, 2, 5, 6, 9:
			fu = FURefuse
		case 3, 11, 12, 20:
			fu = FUForce
		}
		require.Equal(t, fu, r.ForceUnverified, "row %d force_unverified", n)
		nc := NCancelNone
		switch n {
		case 2, 3, 11:
			nc = NCancelDisputed
		case 18:
			nc = NCancelWrongState
		}
		require.Equal(t, nc, r.NCancel, "row %d N_cancel (not rows 20, 21: LD-34)", n)
		require.Equal(t, n == 13 || n == 14, r.SetsLastCancel, "row %d last_cancel_at", n)
		require.Equal(t, n >= 15 && n <= 17, r.ClearsCancelTimes, "row %d clears cancel timestamps", n)
	}
	for _, op := range []string{OpAddDevice, OpBindRecovery} {
		r, _ := PrivateRow(op)
		require.Equal(t, FURefuse, r.ForceUnverified, op)
	}
	r, _ := PrivateRow(OpCancelRecoveryBind)
	require.Equal(t, FUKeep, r.ForceUnverified, "both cancel forms stay admitted")
	// DoR:680: rows 2–4 need the account in the population, row 1 needs it not
	// to be, and no other row (or private op) consults the population.
	// DoR:678: only row 4 has a PoP-specific floor.
	for n := 1; n <= 22; n++ {
		r := rowN(t, n)
		pop := PopAny
		switch n {
		case 1:
			pop = PopNotIn
		case 2, 3, 4:
			pop = PopIn
		}
		require.Equal(t, pop, r.Population, "row %d population", n)
		if n == 4 {
			require.Equal(t, []FloorName{FloorS}, r.PoPFloors, "row 4: S with a PoP")
		} else {
			require.Empty(t, r.PoPFloors, "row %d has no PoP-specific floor", n)
		}
	}
	// The private ops carry only their authority-only columns (DoR:684, :685,
	// :682, :690): recorded `held` refusal, no N_cancel, no population rule and
	// no cancel timestamps.
	for _, op := range []string{OpAddDevice, OpBindRecovery, OpCancelRecoveryBind} {
		r, _ := PrivateRow(op)
		require.Equal(t, HeldRefuse, r.Held, "%s held", op)
		require.Equal(t, NCancelNone, r.NCancel, "%s N_cancel", op)
		require.Equal(t, PopAny, r.Population, "%s population", op)
		require.Empty(t, r.PoPFloors, "%s PoP floors", op)
		require.False(t, r.SetsLastCancel, "%s last_cancel_at", op)
		require.False(t, r.ClearsCancelTimes, "%s clears cancel timestamps", op)
		require.Zero(t, r.DeltaRG, "%s Δrg", op)
	}
}

func TestDeltaRecoveryGen(t *testing.T) {
	for n := 1; n <= 22; n++ {
		want := uint64(1)
		switch n {
		case 1, 5, 6, 7, 8, 9, 10, 19:
			want = 0
		}
		require.Equal(t, want, rowN(t, n).DeltaRG, "row %d Δrg", n)
	}
}

func TestClassify(t *testing.T) {
	root := []byte{4}
	cases := []struct {
		p    pub
		want FromCode
	}{
		{pub{State: StateAbsent}, FromA0},
		{pub{State: StateAbsent, Pin: []byte{1}}, FromAp},
		{pub{State: StateAbsent, Pin: []byte{1}, PinRevoked: 1}, FromApr},
		{pub{State: StateActive, Root: root}, FromAc},
		{pub{State: StateSuspended, Root: root, Susp: []string{ActorUser}}, FromSu},
		{pub{State: StateSuspended, Root: root, Susp: []string{ActorOperator, ActorUser}}, FromSo},
		{pub{State: StateSuspended, Root: root, Susp: []string{ActorOperator}, RootRevoked: 1}, FromSor},
		{pub{State: StatePending, Prior: StateActive, Root: root, PendRoot: root, Op: OpReplaceRoot}, FromP},
		{pub{State: StatePending, Prior: StateActive, Root: root, PendRoot: root, Op: OpRecover}, FromPr},
		{pub{State: StatePending, Prior: StateActive, Root: root, Op: OpErase}, FromPe},
		{pub{State: StateDisputed, Prior: StateAbsent}, FromD},
		{pub{State: StateErased, Root: root}, FromE},
	}
	for _, c := range cases {
		require.Equal(t, c.want, classify(c.p), "%+v", c.p)
	}
}

// §2.9 derived activation (DoR:666–672).
func TestActivate(t *testing.T) {
	old, cand := []byte("old-root"), []byte("candidate")
	act := activate(pub{State: StatePending, Prior: StateActive, Root: old, EK: []byte("ek1"), PendRoot: cand,
		PendEK: []byte("ek2"), EKGen: 1, PendEKGen: 2, ANB: 99})
	require.Equal(t, StateActive, act.State)
	require.Equal(t, cand, act.Root)
	require.Equal(t, []byte("ek2"), act.EK)
	require.Equal(t, uint64(2), act.EKGen)
	require.Empty(t, act.PendRoot)
	require.Nil(t, act.PendEK, "the pending EK is consumed")
	require.Zero(t, act.PendEKGen, "the pending EK generation is consumed")
	require.Zero(t, act.ANB)
	require.Empty(t, act.Prior)

	fromSor := activate(pub{State: StatePending, Prior: StateSuspended, Root: old, RootRevoked: 1,
		Susp: []string{ActorOperator, ActorUser}, PendRoot: cand})
	require.Equal(t, StateSuspended, fromSor.State)
	require.Equal(t, []string{ActorUser}, fromSor.Susp, "operator removed only when it came from Sor")
	require.Zero(t, fromSor.RootRevoked)

	opOnly := activate(pub{State: StatePending, Prior: StateSuspended, Root: old, RootRevoked: 1,
		Susp: []string{ActorOperator}, PendRoot: cand})
	require.Equal(t, StateActive, opOnly.State)
	require.Empty(t, opOnly.Susp)

	fromSo := activate(pub{State: StatePending, Prior: StateSuspended, Root: old, Susp: []string{ActorOperator}, PendRoot: cand})
	require.Equal(t, StateSuspended, fromSo.State, "operator kept when root_revoked = 0")

	pe := activate(pub{State: StatePending, Prior: StateSuspended, Root: old, Susp: []string{ActorUser},
		EK: []byte("ek1"), EKGen: 4, RootRevoked: 1, ANB: 99})
	require.Equal(t, StateErased, pe.State)
	require.Equal(t, old, pe.Root, "the root is kept")
	require.Equal(t, []byte("ek1"), pe.EK, "an erase keeps the EK (DoR:670, :332)")
	require.Equal(t, uint64(4), pe.EKGen, "an erase keeps the EK generation")
	require.Equal(t, uint64(1), pe.RootRevoked, "an erase carries root_revoked unchanged")
	require.Empty(t, pe.Susp)
	require.Zero(t, pe.ANB)
	require.Empty(t, pe.Prior)
	require.Nil(t, pe.PendEK)
	require.Zero(t, pe.PendEKGen)

	pr := activate(pub{State: StatePending, Prior: StateActive, Root: old, PendRoot: cand, Op: OpRecover})
	require.Equal(t, StateActive, pr.State)
	require.Equal(t, cand, pr.Root)
}

func TestRowsForSelectsByOpAndActor(t *testing.T) {
	ns := func(rs []Row) []int {
		out := make([]int, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.N)
		}
		return out
	}
	require.Equal(t, []int{13, 14, 21}, ns(rowsFor(OpCancelPending, ActorUser)))
	require.Equal(t, []int{22}, ns(rowsFor(OpCancelPending, ActorRestore)))
	require.Equal(t, []int{15, 16, 17}, ns(rowsFor(OpRevokeRoot, ActorOperator)))
	require.Empty(t, rowsFor(OpRevokeRoot, ActorUser))
}

// The client-checkable columns of DoR:609–632, transcribed a second time and
// independently of table.go, with the signature split of §2.7 (DoR:535–549):
// ek-binding signer, PoP, and row 21's key being the pending recover's.
func TestClientCheckableColumns(t *testing.T) {
	cand := sigCandidate | sigEKByCandidate
	anyButPePrEWant := []FromCode{FromA0, FromAp, FromApr, FromAc, FromSu, FromSo, FromSor, FromP, FromD}
	type cols struct {
		op, actor string
		from      []FromCode
		g         guard
		sigs, opt sigReq
		to        toRule
		a         assureRule
		anb       anbRule
	}
	want := map[int]cols{
		1:  {OpEnrollNew, ActorUser, []FromCode{FromA0}, gNone, cand, 0, toEnrollActive, aInitial, anbZero},
		2:  {OpEnrollExisting, ActorUser, []FromCode{FromAp}, gNone, cand | sigPoP, 0, toPendOrDisp, aRow2, anbIfPending},
		3:  {OpEnrollExisting, ActorUser, []FromCode{FromAp, FromApr}, gNone, cand, 0, toPendOrDisp, aRow3, anbIfPending},
		4:  {OpEnrollExisting, ActorUser, []FromCode{FromP, FromD}, gPriorAbsent, cand, sigPoP, toDispBurn, aEmpty, anbZero},
		5:  {OpRotateRoot, ActorUser, []FromCode{FromAc}, gNone, cand | sigCurrent, 0, toRotateRoot, aSegment, anbZero},
		6:  {OpRotateEK, ActorUser, []FromCode{FromAc}, gNone, sigCurrent | sigEKByCurrent, 0, toRotateEK, aEmpty, anbZero},
		7:  {OpSuspend, ActorUser, []FromCode{FromAc, FromSo, FromSor}, gUserNotInSet, 0, 0, toSuspendAdd, aEmpty, anbZero},
		8:  {OpSuspend, ActorOperator, []FromCode{FromAc, FromSu}, gNone, sigOperator, 0, toSuspendAdd, aEmpty, anbZero},
		9:  {OpUnsuspend, ActorUser, []FromCode{FromSu, FromSo}, gUserInSetUnrevoked, sigCurrent, 0, toUnsuspend, aEmpty, anbZero},
		10: {OpUnsuspend, ActorOperator, []FromCode{FromSo}, gOpInSetUnrevoked, sigOperator, 0, toUnsuspend, aEmpty, anbZero},
		11: {OpReplaceRoot, ActorUser, []FromCode{FromAc, FromSu, FromSor}, gNone, cand, 0, toPendOrDisp, aRow11, anbIfPending},
		12: {OpReplaceRoot, ActorUser, []FromCode{FromP, FromD}, gPriorActiveOrSusp, cand, 0, toDispBurn, aEmpty, anbZero},
		13: {OpCancelPending, ActorUser, []FromCode{FromP, FromD, FromPe}, gRootUnrevoked, sigCurrent, 0, toRestore, aEmpty, anbZero},
		14: {OpCancelPending, ActorUser, []FromCode{FromP, FromD, FromPe}, gLivePinPriorAbsent, sigPoP, 0, toAp, aEmpty, anbZero},
		15: {OpRevokeRoot, ActorOperator, []FromCode{FromAc, FromSu, FromSo}, gNone, sigOperator, 0, toSor, aEmpty, anbZero},
		16: {OpRevokeRoot, ActorOperator, []FromCode{FromP, FromD}, gPriorNotAbsent, sigOperator, 0, toSor, aEmpty, anbZero},
		17: {OpRevokeRoot, ActorOperator, []FromCode{FromAp, FromP, FromD}, gAbsentPrior, sigOperator, 0, toApr, aEmpty, anbZero},
		18: {OpErase, ActorUser, anyButPePrEWant, gNone, 0, 0, toPendErase, aEmpty, anbAlways},
		19: {OpErase, ActorOperator, append(slices.Clone(anyButPePrEWant), FromPe), gNone, sigOperator, 0, toErased, aEmpty, anbZero},
		20: {OpRecover, ActorUser, []FromCode{FromAc, FromSu, FromSo, FromSor, FromP, FromD, FromPe, FromPr}, gNone,
			cand | sigRecovery | sigBindProof, 0, toPr, aRow20, anbAlways},
		21: {OpCancelPending, ActorUser, []FromCode{FromPr}, gNone, sigPendingRecovery, 0, toRestore, aEmpty, anbZero},
		22: {OpCancelPending, ActorRestore, []FromCode{FromP, FromD, FromPr}, gNone, sigHeldStatus, 0, toRestore, aEmpty, anbZero},
	}
	require.Len(t, want, 22)
	for n, w := range want {
		r := rowN(t, n)
		require.Equal(t, w.op, r.Op, "row %d op", n)
		require.Equal(t, w.actor, r.Actor, "row %d actor", n)
		require.Equal(t, w.from, r.From, "row %d From", n)
		require.Equal(t, w.g, r.guard, "row %d guard", n)
		require.Equal(t, w.sigs, r.sigs, "row %d required signatures", n)
		require.Equal(t, w.opt, r.optSigs, "row %d optional signatures", n)
		require.Equal(t, w.to, r.to, "row %d To", n)
		require.Equal(t, w.a, r.assure, "row %d allowed assurance", n)
		require.Equal(t, w.anb, r.anb, "row %d anb", n)
	}
	for _, op := range []string{OpAddDevice, OpBindRecovery, OpCancelRecoveryBind} {
		r, _ := PrivateRow(op)
		require.Equal(t, ActorUser, r.Actor, op)
		require.Empty(t, r.From, "%s is not a row of the published table (D-238)", op)
		require.Zero(t, r.sigs, op)
		require.Zero(t, r.to, op)
	}
	require.Empty(t, rowsFor(OpAddDevice, ActorUser), "a private op never selects a published row")
}

// sigsOverlap reports whether one signature bundle satisfies both rows the way
// selectRow admits it: required ⊆ present ⊆ required ∪ optional. Such a bundle
// exists iff a.req ⊆ b.req∪b.opt and b.req ⊆ a.req∪a.opt.
func sigsOverlap(a, b Row) bool {
	ar, ao, br, bo := required(a.sigs), required(a.optSigs), required(b.sigs), required(b.optSigs)
	return ar&^(br|bo) == 0 && br&^(ar|ao) == 0
}

// Two rows of one (op, actor) that admit the same from-code must never both
// admit one bundle at one head, or selectRow would pick whichever comes first.
// The test computes the same overlap selectRow does: the signature patterns
// overlap and the guards cannot tell the heads apart. Equal guards never
// separate two rows; unequal guards are confirmed disjoint by evaluating them
// on sample heads. The expected profile of each pair is pinned, so a row whose
// guard or signature column drifts is caught even while the pair stays
// separated by the other column.
func TestRowsOfOneOpAndActorAreDisjoint(t *testing.T) {
	type key struct{ op, actor string }
	seen := map[key][]Row{}
	for _, r := range Rows() {
		k := key{r.Op, r.Actor}
		seen[k] = append(seen[k], r)
	}
	type profile struct{ sigsOverlap, guardsDiffer bool }
	got := map[[2]int]profile{}
	for k, rs := range seen {
		for i := range rs {
			for j := i + 1; j < len(rs); j++ {
				a, b := rs[i], rs[j]
				shared := false
				for _, c := range a.From {
					shared = shared || slices.Contains(b.From, c)
				}
				if !shared {
					continue
				}
				p := profile{sigsOverlap: sigsOverlap(a, b), guardsDiffer: a.guard != b.guard}
				got[[2]int{a.N, b.N}] = p
				require.False(t, p.sigsOverlap && !p.guardsDiffer,
					"%v: rows %d and %d share a from-code, admit one bundle, and have the same guard", k, a.N, b.N)
				if p.sigsOverlap {
					requireGuardsDisjoint(t, a, b)
				}
			}
		}
	}
	require.Equal(t, map[[2]int]profile{
		{2, 3}:   {sigsOverlap: false, guardsDiffer: false}, // told apart by the PoP pair alone
		{13, 14}: {sigsOverlap: false, guardsDiffer: true},  // sig_current_root vs PoP, and different guards
		{16, 17}: {sigsOverlap: true, guardsDiffer: true},   // the guards alone
	}, got, "the pairs of one (op, actor) that share a from-code")
}

// requireGuardsDisjoint checks that no sample head satisfies both guards, over
// the state, prior state and root_revoked of every head a shared from-code
// classifies.
func requireGuardsDisjoint(t *testing.T, a, b Row) {
	t.Helper()
	for _, c := range a.From {
		if !slices.Contains(b.From, c) {
			continue
		}
		for _, state := range []string{StateAbsent, StateActive, StateSuspended, StatePending, StateDisputed} {
			for _, prior := range []string{"", StateAbsent, StateActive, StateSuspended} {
				for _, revoked := range []uint64{0, 1} {
					h := pub{State: state, Prior: prior, Root: []byte("root"), RootRevoked: revoked, Pin: []byte{1},
						PendRoot: []byte("cand"), Susp: []string{ActorUser, ActorOperator}}
					if classify(h) != c {
						continue
					}
					require.False(t, guardHolds(a.guard, h) && guardHolds(b.guard, h),
						"rows %d and %d both admit %+v", a.N, b.N, h)
				}
			}
		}
	}
}

// The table is package state: no caller can change the protocol through the
// rows it is handed (fail closed by construction).
func TestReturnedRowsDoNotAliasTheTable(t *testing.T) {
	r := rowN(t, 20)
	r.From[0], r.Floors[0] = FromE, FloorOP
	rows := Rows()
	rows[19].From[1], rows[19].Floors[0] = FromE, FloorOP
	rows[3].PoPFloors[0] = FloorNone
	for _, x := range rowsFor(OpRecover, ActorUser) {
		x.From[2] = FromE
	}
	p, _ := PrivateRow(OpBindRecovery)
	p.Floors[0] = FloorNone

	again := rowN(t, 20)
	require.Equal(t, []FromCode{FromAc, FromSu, FromSo, FromSor, FromP, FromD, FromPe, FromPr}, again.From)
	require.Equal(t, []FloorName{FloorNone}, again.Floors)
	require.Equal(t, []FloorName{FloorS}, rowN(t, 4).PoPFloors)
	p, _ = PrivateRow(OpBindRecovery)
	require.Equal(t, []FloorName{FloorH}, p.Floors)
	for _, n := range []int{18, 19} {
		require.NotContains(t, rowN(t, n).From, FromE, "row %d", n)
	}
}

func TestRowByNBounds(t *testing.T) {
	for _, n := range []int{-1, 0, 23} {
		_, ok := RowByN(n)
		require.False(t, ok, "row %d", n)
	}
}

// D(x) never activates (DoR:672); activation is defined on pending heads only.
func TestActivateLeavesANonPendingHeadUnchanged(t *testing.T) {
	root := []byte("root")
	for _, p := range []pub{
		{State: StateDisputed, Prior: StateActive, Root: root, ANB: 0},
		{State: StateDisputed, Prior: StateAbsent, Pin: []byte{1}},
		{State: StateActive, Root: root},
		{State: StateSuspended, Root: root, Susp: []string{ActorUser}},
		{State: StateAbsent},
		{State: StateErased, Root: root},
	} {
		require.Equal(t, p, activate(p), "%+v", p)
	}
}

// activate never writes through to its argument's suspended_by set.
func TestActivateDoesNotAliasItsInput(t *testing.T) {
	in := pub{State: StatePending, Prior: StateSuspended, RootRevoked: 1, Susp: []string{ActorOperator, ActorUser},
		PendRoot: []byte("candidate")}
	out := activate(in)
	require.Equal(t, []string{ActorUser}, out.Susp)
	out.Susp[0] = ActorOperator
	require.Equal(t, []string{ActorOperator, ActorUser}, in.Susp)
}

func TestPubOfResultAndGenesis(t *testing.T) {
	require.Equal(t, pub{State: StateAbsent}, genesisPub())
	require.Equal(t, FromA0, classify(genesisPub()))

	r := Result{Op: OpReplaceRoot, Seq: 7, State: StatePending, PriorState: StateSuspended,
		SuspendedBy: []string{ActorUser}, ActiveRoot: []byte("root"), RootRevoked: 1, EKBindingDigest: []byte("ek"),
		PendingRoot: []byte("cand"), PendingEKBindingDigest: []byte("pek"), LegacyEKDigest: []byte("pin"),
		LegacyPinRevoked: 1, RecoveryGen: 4, ActivationNotBeforeMs: 900, Assurance: AssuranceUnverified, IssuedAtMs: 800}
	rb := []byte("result bytes")
	got := pubOfResult(r, rb, 3, 5)
	require.Equal(t, pub{Seq: 7, Entry: EntryDigest(rb), State: StatePending, Prior: StateSuspended,
		Susp: []string{ActorUser}, Root: []byte("root"), RootRevoked: 1, EK: []byte("ek"), PendRoot: []byte("cand"),
		PendEK: []byte("pek"), Pin: []byte("pin"), PinRevoked: 1, RG: 4, ANB: 900, Assurance: AssuranceUnverified,
		Issued: 800, Op: OpReplaceRoot, EKGen: 3, PendEKGen: 5}, got)
	require.Equal(t, StateSuspended, got.x(), "x of P(x) is the prior state")
	require.Equal(t, FromP, classify(got))
}

func TestXOfANonPendingHeadIsItsState(t *testing.T) {
	for _, s := range []string{StateAbsent, StateActive, StateSuspended, StateErased} {
		require.Equal(t, s, pub{State: s, Prior: StateAbsent}.x(), s)
	}
	require.Equal(t, StateAbsent, pub{State: StateDisputed, Prior: StateAbsent}.x())
}

func TestSetHelpers(t *testing.T) {
	base := []string{ActorUser}
	added := withSet(base, ActorOperator)
	require.Equal(t, []string{ActorOperator, ActorUser}, added, "sorted, as the codec's a arrays are")
	require.Equal(t, []string{ActorUser}, base, "the input is not modified")
	require.Equal(t, []string{ActorUser}, withSet(base, ActorUser), "no duplicate")
	require.Equal(t, []string{ActorOperator}, withSet(nil, ActorOperator))

	removed := withoutSet(added, ActorOperator)
	require.Equal(t, []string{ActorUser}, removed)
	require.Equal(t, []string{ActorOperator, ActorUser}, added, "the input is not modified")
	require.NotNil(t, withoutSet([]string{ActorUser}, ActorUser), "[] not nil: suspended_by is [] when not suspended")
	require.Empty(t, withoutSet(nil, ActorUser))

	require.True(t, sameSet(nil, []string{}))
	require.True(t, sameSet([]string{ActorOperator, ActorUser}, []string{ActorOperator, ActorUser}))
	require.False(t, sameSet([]string{ActorUser}, []string{ActorOperator}))
	require.False(t, sameSet([]string{ActorUser}, []string{ActorOperator, ActorUser}))
	require.True(t, has([]string{ActorOperator, ActorUser}, ActorUser))
	require.False(t, has(nil, ActorUser))
}

func TestDigestOrNil(t *testing.T) {
	require.Nil(t, digestOrNil(nil))
	require.Nil(t, digestOrNil([]byte{}))
	require.Equal(t, Digest([]byte("root")), digestOrNil([]byte("root")))
	require.Len(t, digestOrNil([]byte("root")), 48)
}
