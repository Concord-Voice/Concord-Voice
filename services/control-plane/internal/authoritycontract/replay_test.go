package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestR1IssuedBounds(t *testing.T) {
	a := CPAuthz{AssertedAtMs: 1_000_000}
	in := Intent{ExpiresAtMs: 1_000_000 + 600_000}
	nonCancel := func(issued uint64) Reason {
		return checkIssued(0, Result{Op: OpRotateEK, IssuedAtMs: issued}, in, a, 900_000)
	}
	require.Equal(t, ReasonOK, nonCancel(1_000_000-EpsilonMs), "asserted − ε")
	require.Equal(t, ReasonIssuedBounds, nonCancel(1_000_000-EpsilonMs-1), "the issued lower bound")
	require.Equal(t, ReasonOK, nonCancel(1_000_000+600_000-1))
	require.Equal(t, ReasonIssuedBounds, nonCancel(1_000_000+600_000), "< intent.expires_at")
	cancel := func(issued, expires uint64) Reason {
		return checkIssued(0, Result{Op: OpCancelPending, IssuedAtMs: issued}, Intent{ExpiresAtMs: expires}, a, 900_000)
	}
	require.Equal(t, ReasonOK, cancel(1_000_000+900_000+CancelSlackMs-1, 1_000_001))
	require.Equal(t, ReasonIssuedBounds, cancel(1_000_000+900_000+CancelSlackMs, 1_000_001))
	require.Equal(t, ReasonIssuedBounds, cancel(1_000_000, 1_000_000), "expires_at > asserted_at")
	require.Equal(t, ReasonIssuedBounds, checkIssued(1_000_100, Result{Op: OpRotateEK, IssuedAtMs: 1_000_050}, in, a, 900_000),
		"issued_{n+1} ≥ issued_n")
}

func TestR2ANB(t *testing.T) {
	d := Delegation{DeltaMs: 259_200_000, GMs: 900_000}
	a := CPAuthz{AssertedAtMs: 1_000}
	require.Equal(t, ReasonOK, checkANB(Result{State: StatePending, ActivationNotBeforeMs: 1_000 + 259_200_000 + 900_000}, a, d))
	require.Equal(t, ReasonANBMismatch, checkANB(Result{State: StatePending, ActivationNotBeforeMs: 1_000 + 259_200_000}, a, d))
	require.Equal(t, ReasonANBMismatch,
		checkANB(Result{State: StatePending, ActivationNotBeforeMs: 1_000 + 259_200_000 + 900_000 + 1}, a, d),
		"anb is the sum exactly, not a lower bound")
	require.Equal(t, ReasonOK, checkANB(Result{State: StateActive}, a, d))
	require.Equal(t, ReasonANBMismatch, checkANB(Result{State: StateDisputed, ActivationNotBeforeMs: 1}, a, d))
	big := CPAuthz{AssertedAtMs: MaxSafeInt - 1_000}
	require.Equal(t, ReasonANBMismatch, checkANB(Result{State: StatePending, ActivationNotBeforeMs: MaxSafeInt}, big, d),
		"an R2 sum above 2^53−1")
	require.Equal(t, ReasonANBMismatch,
		checkANB(Result{State: StatePending, ActivationNotBeforeMs: MaxSafeInt - 1_000 + 259_200_000 + 900_000}, big, d),
		"an R2 sum above 2^53−1 is refused even when anb equals it")
	edge := CPAuthz{AssertedAtMs: MaxSafeInt - 259_200_000 - 900_000}
	require.Equal(t, ReasonOK, checkANB(Result{State: StatePending, ActivationNotBeforeMs: MaxSafeInt}, edge, d),
		"a sum of exactly 2^53−1 is in range")
}

func TestR3Hypotheses(t *testing.T) {
	p := pub{State: StatePending, Prior: StateActive, Root: []byte("r"), PendRoot: []byte("c"), ANB: 500}
	require.Len(t, hypotheses(p, 499), 1, "issued < anb → {H_pend}")
	hs := hypotheses(p, 500)
	require.Len(t, hs, 2)
	require.False(t, hs[0].act)
	require.True(t, hs[1].act)
	require.Equal(t, StateActive, hs[1].p.State)
	require.Len(t, hypotheses(pub{State: StateActive}, 0), 1)
}

func TestPickMatch(t *testing.T) {
	i, r := pickMatch([]Reason{ReasonOK})
	require.Equal(t, 0, i)
	require.Equal(t, ReasonOK, r)
	_, r = pickMatch([]Reason{ReasonTransitionInvalid})
	require.Equal(t, ReasonTransitionInvalid, r, "one hypothesis reports its own failure (decision A3)")
	i, r = pickMatch([]Reason{ReasonTransitionInvalid, ReasonOK})
	require.Equal(t, 1, i)
	require.Equal(t, ReasonOK, r)
	_, r = pickMatch([]Reason{ReasonOK, ReasonOK})
	require.Equal(t, ReasonHypothesisConflict, r, "two matches")
	_, r = pickMatch([]Reason{ReasonTransitionInvalid, ReasonBadSignature})
	require.Equal(t, ReasonHypothesisConflict, r, "zero matches of two")
}

func TestR4Fix(t *testing.T) {
	prev := pub{Seq: 3, Entry: digestOf("e3"), State: StatePending, ANB: 500}
	pending := &Status{Seq: 3, HeadEntryDigest: digestOf("e3"), AsOfMs: 600, ActivationNotBeforeMs: 700}
	require.False(t, fixAllows(pending, prev, 600, true), "pending at T fixes H_pend for issued ≤ T")
	require.True(t, fixAllows(pending, prev, 600, false))
	require.True(t, fixAllows(pending, prev, 601, true))
	activated := &Status{Seq: 3, HeadEntryDigest: digestOf("e3"), AsOfMs: 800}
	require.False(t, fixAllows(activated, prev, 800, false), "activated at T fixes H_act for issued ≥ T")
	require.True(t, fixAllows(activated, prev, 799, false))
	other := &Status{Seq: 2, AsOfMs: 800}
	require.True(t, fixAllows(other, prev, 900, false), "a fix for another head fixes nothing")
	require.True(t, fixAllows(nil, prev, 900, false))
}

func TestR5(t *testing.T) {
	root, cand := testPoint(t), testPoint(t)
	// R5 compares canonical encodings, so every status here must be a valid
	// `status`: an s36 realm and account and delegation_serial ≥ 1.
	realm, acct := newID(), newID()
	head := pub{Seq: 4, Entry: digestOf("e4"), State: StatePending, Prior: StateActive, Susp: []string{}, Root: root,
		EK: digestOf("ek1"), PendRoot: cand, PendEK: digestOf("ek2"), Pin: nil, ANB: 1_000}
	base := Status{RealmID: realm, AccountID: acct, Seq: 4, HeadEntryDigest: digestOf("e4"), State: StatePending,
		PriorState: StateActive, SuspendedBy: []string{}, ActiveRootDigest: Digest(root), EKBindingDigest: digestOf("ek1"),
		PendingRootDigest: Digest(cand), PendingEKBindingDigest: digestOf("ek2"), ActivationNotBeforeMs: 1_200, AsOfMs: 900,
		DelegationSerial: 1}
	require.Equal(t, ReasonOK, checkR5(head, base))
	s := base
	s.ActivationNotBeforeMs = 999
	require.Equal(t, ReasonANBMismatch, checkR5(head, s), "status.anb ≥ result.anb")
	s = base
	s.ActivationNotBeforeMs = 1_000
	require.Equal(t, ReasonOK, checkR5(head, s), "status.anb = result.anb is allowed: only a smaller anb is refused")
	s = base
	s.Held, s.ActivationNotBeforeMs = 1, ANBNever
	require.Equal(t, ReasonOK, checkR5(head, s), "a held-pending status with ANB_NEVER")
	s.ActivationNotBeforeMs = 1_200
	require.Equal(t, ReasonANBMismatch, checkR5(head, s), "held iff ANB_NEVER")
	s = base
	s.EKBindingDigest = digestOf("ek0")
	require.Equal(t, ReasonFieldMismatch, checkR5(head, s), "an EK-rollback status")
	act := Status{RealmID: realm, AccountID: acct, Seq: 4, HeadEntryDigest: digestOf("e4"), State: StateActive,
		SuspendedBy: []string{}, ActiveRootDigest: Digest(cand), EKBindingDigest: digestOf("ek2"), AsOfMs: 1_000,
		DelegationSerial: 1}
	require.Equal(t, ReasonOK, checkR5(head, act), "effective = Activate(H)")
	act.AsOfMs = 999
	require.Equal(t, ReasonANBMismatch, checkR5(head, act), "as_of ≥ result.anb once activated")
	act.AsOfMs, act.ActivationNotBeforeMs = 1_000, 5
	require.Equal(t, ReasonANBMismatch, checkR5(head, act), "anb = 0 once activated")
	s = base
	s.DelegationGen, s.DelegationSerial = 3, 9
	require.Equal(t, ReasonOK, checkR5(head, s), "R5 exempts the delegation fields")
	genesis := Status{RealmID: realm, AccountID: acct, State: StateAbsent, SuspendedBy: []string{},
		LegacyEKDigest: digestOf("pin"), DelegationSerial: 1}
	require.Equal(t, ReasonOK, checkR5(genesisPub(), genesis), "seq 0: the pin set or \"\"")
	genesis.EKBindingDigest = digestOf("x")
	require.Equal(t, ReasonFieldMismatch, checkR5(genesisPub(), genesis))
}

// The cases below go past the plan's tables: each pins one clause of §2.11
// that the tables above leave reachable by a mutation.

func TestR1IssuedBoundsEdges(t *testing.T) {
	a := CPAuthz{AssertedAtMs: 1_000_000}
	far := Intent{ExpiresAtMs: 1_000_000 + 10*IntentWindowMs}
	rotate := func(prev, issued uint64, in Intent, a CPAuthz) Reason {
		return checkIssued(prev, Result{Op: OpRotateEK, IssuedAtMs: issued}, in, a, 900_000)
	}
	require.Equal(t, ReasonOK, rotate(0, 1_000_000+IntentWindowMs-1, far, a))
	require.Equal(t, ReasonIssuedBounds, rotate(0, 1_000_000+IntentWindowMs, far, a), "< asserted_at + INTENT_WINDOW")
	require.Equal(t, ReasonOK, rotate(1_000_100, 1_000_100, far, a), "issued_{n+1} = issued_n")
	cancel := func(issued uint64, in Intent, a CPAuthz) Reason {
		return checkIssued(0, Result{Op: OpCancelPending, IssuedAtMs: issued}, in, a, 900_000)
	}
	require.Equal(t, ReasonIssuedBounds, cancel(1_000_000-EpsilonMs-1, Intent{ExpiresAtMs: 1_000_001}, a), "cancel: asserted − ε")
	require.Equal(t, ReasonOK, cancel(1_000_000-EpsilonMs, Intent{ExpiresAtMs: 1_000_001}, a))
	require.Equal(t, ReasonOK, cancel(1_000_000+600_000, Intent{ExpiresAtMs: 1_000_001}, a),
		"cancel_pending is bounded by g_ms + CANCEL_SLACK, not by intent.expires_at")
	top := CPAuthz{AssertedAtMs: MaxSafeInt - 1_000}
	require.Equal(t, ReasonOK, rotate(0, MaxSafeInt-1, Intent{ExpiresAtMs: MaxSafeInt}, top),
		"an INTENT_WINDOW bound above 2^53−1 leaves expires_at as the bound")
	require.Equal(t, ReasonIssuedBounds, rotate(0, MaxSafeInt, Intent{ExpiresAtMs: MaxSafeInt}, top))
	require.Equal(t, ReasonIssuedBounds, cancel(MaxSafeInt-1_000, Intent{ExpiresAtMs: MaxSafeInt}, top),
		"a cancel bound above 2^53−1 is refused")
}

func TestR2PendingErase(t *testing.T) {
	d := Delegation{DeltaMs: 259_200_000, GMs: 900_000}
	a := CPAuthz{AssertedAtMs: 1_000}
	// A pending erase has no pending root and still carries the full anb (result #22).
	require.Equal(t, ReasonOK,
		checkANB(Result{State: StatePending, Op: OpErase, ActivationNotBeforeMs: 1_000 + 259_200_000 + 900_000}, a, d))
	require.Equal(t, ReasonANBMismatch, checkANB(Result{State: StatePending, Op: OpErase}, a, d))
}

func TestR3HypothesesEdges(t *testing.T) {
	pe := pub{State: StatePending, Prior: StateActive, Root: []byte("r"), EK: digestOf("ek"), ANB: 500}
	hs := hypotheses(pe, 500)
	require.Len(t, hs, 2)
	require.Equal(t, StatePending, hs[0].p.State, "H_pend is prev")
	require.Equal(t, StateErased, hs[1].p.State, "Pe(x) → E")
	require.Equal(t, []byte("r"), hs[1].p.Root, "the root kept")
	one := hypotheses(pe, 499)
	require.False(t, one[0].act)
	require.Equal(t, StatePending, one[0].p.State)
	require.Len(t, hypotheses(pub{State: StateDisputed, Prior: StateActive}, 10), 1, "D(x) never activates")
}

func TestPickMatchEdges(t *testing.T) {
	i, r := pickMatch([]Reason{ReasonTransitionInvalid})
	require.Equal(t, -1, i, "no index for a failed hypothesis")
	require.Equal(t, ReasonTransitionInvalid, r)
	i, r = pickMatch(nil)
	require.Equal(t, -1, i)
	require.Equal(t, ReasonHypothesisConflict, r, "no hypothesis matches nothing")
	i, r = pickMatch([]Reason{ReasonOK, ReasonHypothesisConflict})
	require.Equal(t, 0, i, "H_pend alone matches")
	require.Equal(t, ReasonOK, r)
}

func TestR4FixEdges(t *testing.T) {
	prev := pub{Seq: 3, Entry: digestOf("e3"), State: StatePending, ANB: 500}
	held := &Status{Seq: 3, HeadEntryDigest: digestOf("e3"), AsOfMs: 900, Held: 1, ActivationNotBeforeMs: ANBNever}
	require.False(t, fixAllows(held, prev, 900, true), "held at T fixes H_pend for issued ≤ T")
	require.True(t, fixAllows(held, prev, 900, false))
	require.True(t, fixAllows(held, prev, 901, true))
	require.True(t, fixAllows(&Status{Seq: 3, HeadEntryDigest: digestOf("x"), AsOfMs: 900}, prev, 900, false),
		"same seq, another head")
	active := pub{Seq: 3, Entry: digestOf("e3"), State: StateActive}
	require.True(t, fixAllows(&Status{Seq: 3, HeadEntryDigest: digestOf("e3"), AsOfMs: 800}, active, 900, false),
		"a head that is not raw pending")
}

func TestR5Edges(t *testing.T) {
	root, cand := testPoint(t), testPoint(t)
	realm, acct := newID(), newID()
	at := func(s Status) Status { s.RealmID, s.AccountID, s.DelegationSerial = realm, acct, 1; return s }
	head := pub{Seq: 4, Entry: digestOf("e4"), State: StatePending, Prior: StateActive, Susp: []string{}, Root: root,
		EK: digestOf("ek1"), PendRoot: cand, PendEK: digestOf("ek2"), ANB: 1_000}
	pend := at(Status{Seq: 4, HeadEntryDigest: digestOf("e4"), State: StatePending, PriorState: StateActive,
		ActiveRootDigest: Digest(root), EKBindingDigest: digestOf("ek1"), PendingRootDigest: Digest(cand),
		PendingEKBindingDigest: digestOf("ek2"), ActivationNotBeforeMs: 1_200, AsOfMs: 900})
	require.Equal(t, ReasonOK, checkR5(head, pend), "a nil suspended_by reads as []")

	s := pend
	s.ActivationNotBeforeMs = ANBNever
	require.Equal(t, ReasonANBMismatch, checkR5(head, s), "ANB_NEVER without held")
	s = pend
	s.AsOfMs = 1_200
	require.Equal(t, ReasonANBMismatch, checkR5(head, s), "as_of = status.anb reads activated, so anb must be 0")
	s = pend
	s.State, s.PriorState, s.ActiveRootDigest, s.EKBindingDigest = StateActive, "", Digest(cand), digestOf("ek2")
	s.PendingRootDigest, s.PendingEKBindingDigest = nil, nil
	require.Equal(t, ReasonFieldMismatch, checkR5(head, s), "an activated body under an effective-pending anb")
	s = pend
	s.Seq = 3
	require.Equal(t, ReasonFieldMismatch, checkR5(head, s), "another seq")
	s = pend
	s.HeadEntryDigest = digestOf("e3")
	require.Equal(t, ReasonFieldMismatch, checkR5(head, s), "another head_entry_digest")
	s = pend
	s.RealmID = ""
	require.Equal(t, ReasonFieldMismatch, checkR5(head, s), "a status that does not encode")

	ac := pub{Seq: 2, Entry: digestOf("e2"), State: StateActive, Susp: []string{}, Root: root, EK: digestOf("ek1"),
		Pin: digestOf("pin")}
	acSt := at(Status{Seq: 2, HeadEntryDigest: digestOf("e2"), State: StateActive, ActiveRootDigest: Digest(root),
		EKBindingDigest: digestOf("ek1"), LegacyEKDigest: digestOf("pin"), AsOfMs: 50})
	require.Equal(t, ReasonOK, checkR5(ac, acSt))
	s = acSt
	s.Held = 1
	require.Equal(t, ReasonOK, checkR5(ac, s), "held is exempt from R5")
	s = acSt
	s.ActivationNotBeforeMs = 7
	require.Equal(t, ReasonANBMismatch, checkR5(ac, s), "anb = 0 on a head that is not pending")
	s = acSt
	s.LegacyEKDigest = digestOf("other")
	require.Equal(t, ReasonFieldMismatch, checkR5(ac, s), "after seq 0 the pin is the result's")
	s = acSt
	s.LegacyPinRevoked = 1
	require.Equal(t, ReasonFieldMismatch, checkR5(ac, s))

	genesis := at(Status{State: StateAbsent})
	require.Equal(t, ReasonOK, checkR5(genesisPub(), genesis), "seq 0 with no pin")
	genesis.LegacyPinRevoked = 1
	require.Equal(t, ReasonFieldMismatch, checkR5(genesisPub(), genesis), "seq 0: every other field 0")
}

func TestR5DerivedActivation(t *testing.T) {
	root, cand := testPoint(t), testPoint(t)
	realm, acct := newID(), newID()
	at := func(s Status) Status { s.RealmID, s.AccountID, s.DelegationSerial = realm, acct, 1; return s }
	t.Run("Pe(x) → E keeps the root", func(t *testing.T) {
		pe := pub{Seq: 5, Entry: digestOf("e5"), State: StatePending, Prior: StateSuspended, Susp: []string{ActorUser},
			Root: root, EK: digestOf("ek1"), ANB: 1_000}
		e := at(Status{Seq: 5, HeadEntryDigest: digestOf("e5"), State: StateErased, SuspendedBy: []string{},
			ActiveRootDigest: Digest(root), EKBindingDigest: digestOf("ek1"), AsOfMs: 1_000})
		require.Equal(t, ReasonOK, checkR5(pe, e))
		s := e
		s.SuspendedBy = []string{ActorUser}
		require.Equal(t, ReasonFieldMismatch, checkR5(pe, s), "an erased status has suspended_by []")
		s = e
		s.ActiveRootDigest = nil
		require.Equal(t, ReasonFieldMismatch, checkR5(pe, s), "the root kept")
	})
	t.Run("P(absent) activation keeps the pin and its revocation", func(t *testing.T) {
		// DoR:337, :338: the pin is set at a population account's first event and
		// never changes; a derived activation installs the candidate only.
		for _, revoked := range []uint64{0, 1} {
			p := pub{Seq: 8, Entry: digestOf("e8"), State: StatePending, Prior: StateAbsent, Susp: []string{},
				PendRoot: cand, PendEK: digestOf("ek2"), Pin: digestOf("pin"), PinRevoked: revoked, ANB: 1_000}
			act := at(Status{Seq: 8, HeadEntryDigest: digestOf("e8"), State: StateActive, SuspendedBy: []string{},
				ActiveRootDigest: Digest(cand), EKBindingDigest: digestOf("ek2"), LegacyEKDigest: digestOf("pin"),
				LegacyPinRevoked: revoked, AsOfMs: 1_000})
			require.Equal(t, ReasonOK, checkR5(p, act), "revoked=%d", revoked)
			require.Equal(t, digestOf("pin"), activate(p).Pin)
			require.Equal(t, revoked, activate(p).PinRevoked)
			s := act
			s.LegacyEKDigest, s.LegacyPinRevoked = nil, 0
			require.Equal(t, ReasonFieldMismatch, checkR5(p, s), "the pin is kept (revoked=%d)", revoked)
			s = act
			s.LegacyPinRevoked = 1 - revoked
			require.Equal(t, ReasonFieldMismatch, checkR5(p, s), "legacy_pin_revoked is unchanged (revoked=%d)", revoked)
		}
	})
	t.Run("P(suspended) from Sor drops operator", func(t *testing.T) {
		p := pub{Seq: 6, Entry: digestOf("e6"), State: StatePending, Prior: StateSuspended,
			Susp: []string{ActorOperator, ActorUser}, Root: root, RootRevoked: 1, EK: digestOf("ek1"), PendRoot: cand,
			PendEK: digestOf("ek2"), ANB: 1_000}
		act := at(Status{Seq: 6, HeadEntryDigest: digestOf("e6"), State: StateSuspended, SuspendedBy: []string{ActorUser},
			ActiveRootDigest: Digest(cand), EKBindingDigest: digestOf("ek2"), AsOfMs: 1_000})
		require.Equal(t, ReasonOK, checkR5(p, act))
		s := act
		s.SuspendedBy = []string{ActorOperator, ActorUser}
		require.Equal(t, ReasonFieldMismatch, checkR5(p, s), "operator removed when it came from Sor")
		s = act
		s.RootRevoked = 1
		require.Equal(t, ReasonFieldMismatch, checkR5(p, s), "root_revoked := 0")
	})
	t.Run("P(suspended) from So keeps operator", func(t *testing.T) {
		p := pub{Seq: 7, Entry: digestOf("e7"), State: StatePending, Prior: StateSuspended,
			Susp: []string{ActorOperator}, Root: root, EK: digestOf("ek1"), PendRoot: cand, PendEK: digestOf("ek2"), ANB: 1_000}
		act := at(Status{Seq: 7, HeadEntryDigest: digestOf("e7"), State: StateSuspended,
			SuspendedBy: []string{ActorOperator}, ActiveRootDigest: Digest(cand), EKBindingDigest: digestOf("ek2"), AsOfMs: 2_000})
		require.Equal(t, ReasonOK, checkR5(p, act))
		s := act
		s.State, s.SuspendedBy = StateActive, []string{}
		require.Equal(t, ReasonFieldMismatch, checkR5(p, s))
	})
}
