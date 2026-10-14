package authoritycontract

import "bytes"

// The replay rules R1–R5 (§2.11). What they guarantee together:
//
//   - R1 and R2 are hypothesis-independent: they read only the result, its
//     intent, its cp-authz and the delegation it names, so they run once, before
//     R3, and a failure is the result's own Reason.
//   - R3 never lets a result choose between "the pending change has not
//     activated" (H_pend) and "it activated" (H_act): it must match exactly one,
//     and zero or two matches is hypothesis_conflict. The two are distinct
//     because H_act's root is always the candidate and the candidate is never
//     the prior root (bindings are burned). For a pending erase there is no
//     candidate: H_act is E, which differs from Pe by state (the root is
//     kept), so the two stay distinct.
//   - R4 only ever removes a hypothesis, never adds one: a retained verified
//     status that saw the head pending at T, or activated at T, rules out the
//     other reading for the issued_at range it witnessed.
//   - R5 pins every derivable field of a status to the verified head, so an RSK
//     can choose only as_of, valid_until, held and the extension part of anb
//     (§2.11 "What the RSK can still choose"), never the root, the EK or the
//     state.
//
// Breaking any one of them lets a server replay one chain as two histories:
// accepting both hypothesis matches, or ignoring a fix, lets the next result
// pick whichever from-state suits it.

// checkIssued is R1 (client): asserted_at − ε ≤ issued_at, the upper bound
// for the op, and issued never decreasing along the chain. gMs is the g_ms of
// the result's delegation. An upper bound that overflows 2^53 − 1 is refused
// on cancel_pending, where it is the whole bound, and is exact on every other
// op, where min(expires_at, ·) is expires_at whenever the sum overflows.
func checkIssued(prevIssued uint64, r Result, in Intent, a CPAuthz, gMs uint64) Reason {
	if r.IssuedAtMs < prevIssued || r.IssuedAtMs+EpsilonMs < a.AssertedAtMs {
		return ReasonIssuedBounds
	}
	if r.Op == OpCancelPending {
		hi, ok := addMs(a.AssertedAtMs, gMs, CancelSlackMs)
		if !ok || r.IssuedAtMs >= hi || in.ExpiresAtMs <= a.AssertedAtMs {
			return ReasonIssuedBounds
		}
		return ReasonOK
	}
	hi, ok := addMs(a.AssertedAtMs, IntentWindowMs)
	if !ok {
		hi = MaxSafeInt
	}
	if r.IssuedAtMs >= min(in.ExpiresAtMs, hi) {
		return ReasonIssuedBounds
	}
	return ReasonOK
}

// checkANB is R2: a pending result (a pending erase included) carries exactly
// asserted_at + delta_ms + g_ms of the delegation d it names; any other
// result carries 0. A sum above 2^53 − 1 is anb_mismatch, so Go and JS never
// compare a rounded value.
func checkANB(r Result, a CPAuthz, d Delegation) Reason {
	if r.State != StatePending {
		if r.ActivationNotBeforeMs != 0 {
			return ReasonANBMismatch
		}
		return ReasonOK
	}
	want, ok := addMs(a.AssertedAtMs, d.DeltaMs, d.GMs)
	if !ok || r.ActivationNotBeforeMs != want {
		return ReasonANBMismatch
	}
	return ReasonOK
}

// hyp is one R3 hypothesis for the from-state of the next result.
type hyp struct {
	p   pub
	act bool // H_act: the derived activation of a pending prev
}

// hypotheses is R3: {prev} unless prev is raw pending and issued ≥ its anb,
// then {H_pend, H_act} in that order. D(x) never activates, so it is {prev}.
func hypotheses(prev pub, issued uint64) []hyp {
	if prev.State != StatePending || issued < prev.ANB {
		return []hyp{{p: prev}}
	}
	return []hyp{{p: prev}, {p: activate(prev), act: true}}
}

// pickMatch is R3's "exactly one". One hypothesis reports its own Reason
// (decision A3); of two, exactly one ok selects it, and zero or two oks are
// hypothesis_conflict. The index is −1 whenever the Reason is not ok, so a
// caller that ignores the Reason cannot select a failed hypothesis.
func pickMatch(rs []Reason) (int, Reason) {
	if len(rs) == 1 {
		if rs[0] != ReasonOK {
			return -1, rs[0]
		}
		return 0, ReasonOK
	}
	idx, n := -1, 0
	for i, r := range rs {
		if r == ReasonOK {
			idx, n = i, n+1
		}
	}
	if n != 1 {
		return -1, ReasonHypothesisConflict
	}
	return idx, ReasonOK
}

// effectivePending is R5's: raw pending ∧ (held ∨ as_of < status.anb).
func effectivePending(head pub, s Status) bool {
	return head.State == StatePending && (s.Held == 1 || s.AsOfMs < s.ActivationNotBeforeMs)
}

// effectiveOf is the effective state a status describes: Activate(head) when
// head is raw pending and the status shows it activated, otherwise head.
func effectiveOf(head pub, s Status) pub {
	if head.State == StatePending && !effectivePending(head, s) {
		return activate(head)
	}
	return head
}

// fixAllows is R4: a retained verified status for prev (same seq and
// head_entry_digest) that shows prev pending at T admits only H_pend for
// issued ≤ T, and one that shows it activated at T admits only H_act for
// issued ≥ T. It reads the fix exactly as R5 reads a status, and a fix for
// another head, or for a head that is not raw pending, constrains nothing.
func fixAllows(fix *Status, prev pub, issued uint64, act bool) bool {
	if fix == nil || prev.State != StatePending || fix.Seq != prev.Seq || !bytes.Equal(fix.HeadEntryDigest, prev.Entry) {
		return true
	}
	pending := effectivePending(prev, *fix)
	if pending && issued <= fix.AsOfMs {
		return !act
	}
	if !pending && issued >= fix.AsOfMs {
		return act
	}
	return true
}

// derivedFields is Derive(H, s) over the R5-compared fields; s supplies the
// exempt ones (as_of, valid_until, held, anb, gen, serial) and the realm and
// account, which pub does not carry: the caller compares those with the head
// (realm_mismatch) before R5.
func derivedFields(p pub, s Status) Status {
	e := Status{RealmID: s.RealmID, AccountID: s.AccountID, Seq: p.Seq, HeadEntryDigest: p.Entry, State: p.State,
		PriorState: p.Prior, SuspendedBy: p.Susp, ActiveRootDigest: digestOrNil(p.Root), RootRevoked: p.RootRevoked,
		EKBindingDigest: p.EK, PendingRootDigest: digestOrNil(p.PendRoot), PendingEKBindingDigest: p.PendEK,
		LegacyEKDigest: p.Pin, LegacyPinRevoked: p.PinRevoked,
		ActivationNotBeforeMs: s.ActivationNotBeforeMs, Held: s.Held, AsOfMs: s.AsOfMs, ValidUntilMs: s.ValidUntilMs,
		DelegationGen: s.DelegationGen, DelegationSerial: s.DelegationSerial}
	if p.Seq == 0 {
		e.LegacyEKDigest = s.LegacyEKDigest // D-149: the seq-0 pin is set or ""
	}
	if e.SuspendedBy == nil {
		e.SuspendedBy = []string{}
	}
	return e
}

// checkR5 is R5's anb rules and equality for a status naming head. Equality is
// over the canonical encodings, so every field the layout carries is compared
// unless derivedFields copies it from s, and a status that does not encode is
// field_mismatch rather than a pass.
func checkR5(head pub, s Status) Reason {
	if head.State == StatePending {
		if effectivePending(head, s) {
			if (s.Held == 1) != (s.ActivationNotBeforeMs == ANBNever) || s.ActivationNotBeforeMs < head.ANB {
				return ReasonANBMismatch
			}
		} else if s.ActivationNotBeforeMs != 0 || s.AsOfMs < head.ANB {
			return ReasonANBMismatch
		}
	} else if s.ActivationNotBeforeMs != 0 {
		return ReasonANBMismatch
	}
	want := derivedFields(effectiveOf(head, s), s).Encode()
	got := s
	if got.SuspendedBy == nil {
		got.SuspendedBy = []string{}
	}
	if want == nil || !bytes.Equal(want, got.Encode()) {
		return ReasonFieldMismatch
	}
	return ReasonOK
}
