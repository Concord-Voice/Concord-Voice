package authoritycontract

import (
	"bytes"
	"slices"
	"strings"
	"time"
)

// Status verification (§2.12, §2.11 R5, §2.13).
//
// Invariant. A status is verified in two parts, and only the second needs the
// caller's memory and clock:
//
//   - authenticStatus: it is an RSK-signed `status` under the HW delegation
//     (T6), names this realm and account, keeps valid_until ≤ as_of + V_MAX
//     (A6), names the verified head itself (chain_behind, fork), and every
//     derivable field equals the head's (R5). This is all a retained fix needs
//     (R4): a fix is a witness of what the head looked like at its as_of, not a
//     claim about now, so freshness and the state verdict do not apply to it.
//   - verifyStatus adds freshness (status_future, then τ for NewSecret or
//     valid_until for History) and §2.13's state verdict on the effective
//     state, which is where the first-sight pin (D-149) and the accepted pin
//     (LD-28) are compared.
//
// The seq-0 pin (V3-05, D-149). A genesis head carries no pin, and R5 cannot
// derive one: at seq 0 the status supplies it ("set or \"\""). So the status
// pin is compared with AccountMemory's first-sight pin here, in absentVerdict,
// and never anywhere else for an absent head: a changed pin is pin_changed,
// an accepted one ok_accepted_pin, an unseen one ok (the caller then stores
// VerifiedStatus.Pin as the first-sight pin). Once a result has been verified
// the pin is a derived field and R5 pins it to the chain (checkPost keeps it
// immutable from the seq-0 hypothesis on), so a later status cannot change it.
//
// Past genesis the head's pin is still compared with the first-sight pin, in
// every state (stateVerdict; RT-2, D-149, DoR:943, :2107): VerifyBundle's
// genesis is A0 with no pin, so a chain it verified from genesis may carry a
// pin other than the member's, and only this comparison stops a forged row-1
// chain of a pinned account from reaching a wrap. An honest chain never
// differs (checkPost; row 1 never runs on a population account).
//
// The realm a status names is the Trust's own as well as the head's (B-SF1,
// D-115): T6 checks the RSK, not which realm the status speaks for, so a
// caller pairing the wrong Trust with a head would otherwise take another
// realm's RSK as that head's authority.
//
// What breaks it: reading the pin of a status the state verdict has not
// judged; using a fix that skipped authenticStatus (fixAllows trusts R5 was
// run); comparing realm and account only inside R5 (checkR5 copies them from
// the status, so it cannot notice them); comparing the realm with the head
// alone; reading a delegation's τ or cutover from a lookup whose ok was
// ignored; and letting WrapKey read anything verifyStatus did not seal.

// headPub derives the head's published state from a persisted ChainHead.
// The genesis head is absent with no pin (V3-05): only VerifyChain, which
// takes AccountMemory, supplies a seq-0 pin separately; VerifyBundle's
// genesis stays A0.
func headPub(h ChainHead) (pub, Reason) {
	if len(h.HeadResult) == 0 {
		if len(h.SeqIndex) != 0 {
			return pub{}, ReasonMalformed
		}
		return genesisPub(), ReasonOK
	}
	r, err := DecodeResult(h.HeadResult)
	if err != nil || r.Seq != uint64(len(h.SeqIndex)) {
		return pub{}, ReasonMalformed
	}
	return pubOfResult(r, h.HeadResult, h.ActiveEKGeneration, h.PendingEKGeneration), ReasonOK
}

// segmentOf is the root_index assurance of a root (D-182, §2.22 check 5).
// Every root the chain installed is recorded with a non-empty segment (A9);
// "" means the root is not in the index. The lookup is rootRecordOf's binary
// search, so the head must be canonical (a verified or parsed head is).
func segmentOf(h ChainHead, root []byte) Assurance {
	if len(root) == 0 {
		return ""
	}
	rest, ok := rootRecordOf(h, Digest(root))
	if !ok {
		return ""
	}
	return Assurance(rest[strings.LastIndexByte(rest, '.')+1:])
}

// VerifyStatus verifies a signed status against a verified head (§2.12): head
// must come from ParseChainHead or an earlier verification. A
// now that reads as 0 ms (the zero time, any time before the epoch, or the
// epoch itself) is malformed (Sec-N4): msOf reads every such time as 0, and
// judging freshness at 1970 would report a caller's unset clock as
// status_future, a transient.
func VerifyStatus(t *Trust, hw HighWater, head ChainHead, signed []byte, mem AccountMemory, now time.Time, use Use) (VerifiedStatus, HighWater, Reason) {
	nowMs := msOf(now)
	if nowMs == 0 {
		return VerifiedStatus{Reason: ReasonMalformed}, hw, ReasonMalformed
	}
	return verifyStatus(t, hw, head, signed, mem, nowMs, use)
}

// authentic is a status that passed authenticStatus, with what verifyStatus
// needs next.
type authentic struct {
	st Status
	hp pub
	d  Delegation
	hw HighWater
}

// authenticStatus is the part of §2.12's status verification that needs
// neither the clock nor the caller's memory (see the invariant above).
func authenticStatus(t *Trust, hw HighWater, head ChainHead, signed []byte) (authentic, Reason) {
	if r := t.haltReason(); r != ReasonOK {
		return authentic{}, r
	}
	st, hw2, r := signedStatus(t, hw, signed)
	if r != ReasonOK {
		return authentic{}, r
	}
	// T6 passed, so the delegation is current_gen's HW delegation, of which a
	// trust that is not halted retains exactly one; refuse rather than read a
	// zero τ or cutover from a lookup that found none.
	d, ok := t.lookup(st.DelegationGen, st.DelegationSerial)
	if !ok {
		return authentic{}, ReasonDelegationUnknown
	}
	if st.RealmID != t.realm || st.RealmID != head.RealmID || st.AccountID != head.AccountID { // B-SF1
		return authentic{}, ReasonRealmMismatch
	}
	if st.ValidUntilMs <= st.AsOfMs || st.ValidUntilMs-st.AsOfMs > VMaxMs {
		return authentic{}, ReasonMalformed
	}
	hp, r := headPub(head)
	if r != ReasonOK {
		return authentic{}, r
	}
	if st.Seq > hp.Seq {
		return authentic{}, ReasonChainBehind
	}
	if st.Seq < hp.Seq || !bytes.Equal(st.HeadEntryDigest, hp.Entry) {
		return authentic{}, ReasonFork
	}
	if r := checkR5(hp, st); r != ReasonOK {
		return authentic{}, r
	}
	return authentic{st: st, hp: hp, d: d, hw: hw2}, ReasonOK
}

// signedStatus decodes a signed status and checks its signature and
// delegation (T6), returning the effective HW. It reads nothing of the head:
// VerifyChain runs it before the chain (T7's order) and authenticStatus runs
// it first.
func signedStatus(t *Trust, hw HighWater, signed []byte) (Status, HighWater, Reason) {
	sg, err := DecodeSigned(signed)
	if err != nil || sg.Type != ScopeStatus { // a device-status here is malformed (P1-e)
		return Status{}, hw, ReasonMalformed
	}
	st, err := DecodeStatus(sg.Msg)
	if err != nil {
		return Status{}, hw, ReasonMalformed
	}
	hw2, r := t.checkT6(hw, sg.Msg, sg.SigA, st.DelegationGen, st.DelegationSerial, st.AsOfMs)
	return st, hw2, r
}

func verifyStatus(t *Trust, hw HighWater, head ChainHead, signed []byte, mem AccountMemory, nowMs uint64, use Use) (VerifiedStatus, HighWater, Reason) {
	a, r := authenticStatus(t, hw, head, signed)
	if r != ReasonOK {
		return VerifiedStatus{Reason: r}, hw, r
	}
	st, hp := a.st, a.hp
	if r := freshness(st, a.d, nowMs, use); r != ReasonOK {
		return VerifiedStatus{Reason: r}, hw, r
	}
	eff := effectiveOf(hp, st)
	// R4 on a status (N-2; DoR:738, :2106): once the retained fix shows the raw
	// pending head activated, a status that shows it pending again, older or
	// newer and not held, does not undo what the client saw: the fix decides,
	// and the head reads as activated. Read as pending, such a status wrapped a
	// continuity head to the legacy key again, with one RSK. It is not refused,
	// because DoR step 7 (:1445, :1464, :1689) lets an honest authority's
	// visible instant move past an activated status (a tick gap, a reconcile
	// floor), and an honest stale status is pending too (A6).
	if fix, err := fixStatus(head.Fix); err == nil && fixNames(fix, hp) && !effectivePending(hp, *fix) &&
		st.Held == 0 && effectivePending(hp, st) {
		eff = activate(hp)
	}
	vs := VerifiedStatus{Status: st, Pin: st.LegacyEKDigest, Segment: segmentOf(head, eff.Root)}
	if vs.Segment == "" && hp.State == StatePending && eff.State != StatePending {
		vs.Segment = Assurance(hp.Assurance) // an activated root is recorded only by the next result
	}
	if eff.State == StateActive {
		vs.ekSPKI = head.ActiveEKSPKI
		if hp.State == StatePending {
			vs.ekSPKI = head.PendingEKSPKI
		}
	}
	vs.Reason = stateVerdict(eff, st, mem, a.d, use, &vs)
	vs.seal(use)
	return vs, a.hw, vs.Reason
}

// seal records what WrapKey reads (R6, E-S1, open item 1): the Use the
// status was judged under and, for an ok or ok_accepted_pin verdict only, a
// copy of the wrap target. verifyStatus is its only production caller, so a
// struct literal carries no seal, and an edit to a returned value's exported
// fields moves no wrap.
func (v *VerifiedStatus) seal(use Use) {
	v.use = use
	if v.Reason != ReasonOK && v.Reason != ReasonOKAcceptedPin {
		return
	}
	v.wrap = &wrapTarget{ek: slices.Clone(v.ekSPKI), pin: slices.Clone(v.Pin),
		served: v.continuity || v.Status.State == StateAbsent}
}

// olderThanTau is τ freshness (DoR:919): as_of more than d's tau_ms before now.
// The chain status and the private ledger's device-status share it.
func olderThanTau(asOfMs uint64, d Delegation, nowMs uint64) bool {
	return asOfMs+d.TauMs < nowMs
}

func freshness(st Status, d Delegation, nowMs uint64, use Use) Reason {
	if st.AsOfMs > nowMs+EpsilonMs {
		return ReasonStatusFuture
	}
	switch use {
	case UseNewSecret:
		if olderThanTau(st.AsOfMs, d, nowMs) {
			return ReasonStatusStaleTau
		}
	case UseHistory:
		if nowMs > st.ValidUntilMs {
			return ReasonStatusExpired
		}
	default:
		return ReasonMalformed
	}
	return ReasonOK
}

// stateVerdict is §2.13 on the effective state. Every state is named, and
// one this switch does not know is malformed, never the absent path (B-N1).
// Past the absent state a head pin other than the first-sight pin is
// pin_changed whatever the state (RT-2; see the invariant above); the absent
// state keeps absentVerdict's own comparison, with LD-28's accepted pin. A
// changed pin is reported before held (NEW-2): held carries no notice
// (DoR:924), so holding would hide the substitution for as long as it lasts.
func stateVerdict(eff pub, st Status, mem AccountMemory, d Delegation, use Use, vs *VerifiedStatus) Reason {
	if eff.State != StateAbsent && len(mem.Pin) > 0 && !bytes.Equal(mem.Pin, eff.Pin) {
		return ReasonPinChanged
	}
	if st.Held == 1 {
		if eff.State == StateAbsent && absentVerdict(st, mem, d, use, &VerifiedStatus{}) == ReasonPinChanged {
			return ReasonPinChanged
		}
		return ReasonHeld
	}
	switch eff.State {
	case StateAbsent:
		return absentVerdict(st, mem, d, use, vs)
	case StateActive:
		return ReasonOK
	case StateSuspended:
		if eff.RootRevoked == 1 {
			return ReasonRootRevoked
		}
		return ReasonSuspended
	case StateDisputed:
		return ReasonDisputed
	case StateErased:
		return ReasonErased
	case StatePending:
		return pendingVerdict(eff, vs)
	}
	return ReasonMalformed
}

// pendingVerdict is §2.13 on an effective pending head. Only "a continuity
// change pending from absent" is ok (DoR:930), and each of its conditions is
// checked rather than reached by elimination (B-N2): the prior state is
// absent, the pin is set and not revoked, and the change is continuity, which
// R6 wraps to under the pin's SPKI. A prior state no pending result can carry
// is malformed. The first-sight pin is stateVerdict's.
func pendingVerdict(eff pub, vs *VerifiedStatus) Reason {
	switch {
	case len(eff.PendRoot) == 0:
		return ReasonPendingErase
	case eff.Prior == StateActive:
		return ReasonPendingFromActive
	case eff.Prior == StateSuspended:
		return ReasonPendingFromSuspended
	case eff.Prior != StateAbsent:
		return ReasonMalformed
	case eff.Assurance != AssuranceContinuity || len(eff.Pin) == 0 || eff.PinRevoked == 1:
		return ReasonPendingFromAbsent
	}
	vs.continuity = true
	return ReasonOK
}

// absentVerdict is §2.13's absent path: D-149's first-sight pin and LD-28.
func absentVerdict(st Status, mem AccountMemory, d Delegation, use Use, vs *VerifiedStatus) Reason {
	pin := st.LegacyEKDigest
	switch {
	case mem.EverEnrolled:
		return ReasonAbsentAfterEnrolled
	case len(pin) == 0:
		return ReasonAbsentUnpinned
	case st.LegacyPinRevoked == 1:
		return ReasonPinRevoked
	case d.CutoverAtMs != 0 && st.AsOfMs >= d.CutoverAtMs:
		return ReasonAbsentAfterCutover
	case len(mem.Pin) == 0 || bytes.Equal(mem.Pin, pin):
		return ReasonOK
	case use == UseNewSecret && bytes.Equal(mem.AcceptedPin, pin):
		vs.AcceptedPin = true
		return ReasonOKAcceptedPin
	}
	return ReasonPinChanged
}

// WrapKey is R6: only these bytes may be wrapped to. An effective active
// state wraps to the verified chain's active EK; a continuity change pending
// from absent, or an absent account before cutover (first-sight or accepted
// pin), wraps to a served SPKI that is a valid EK (§2.4) whose SHA-384 is the
// status pin. Every other verdict wraps to nothing.
//
// When the retained fix shows the raw pending head activated and the status
// still shows it pending (R4 on a status, N-2), the effective state is active
// and the target is the activated EK: the status's pending_ek_binding_digest,
// not its ek_binding_digest as R6 reads literally. Keep it so in a port; the
// literal reading wraps to the superseded EK.
//
// It reads only what verifyStatus sealed (E-S1, open item 1): a value
// VerifyStatus or VerifyChain returned, for a status judged with
// UseNewSecret (§2.13: a wrap needs freshness within τ; a UseHistory status
// is fresh only within V). A struct literal, a History status, and the
// exported fields of a returned value, edited or not, reach no wrap; the
// bytes it returns are a copy. VerifiedStatus does not export the active EK;
// a ChainHead's EK fields do, so a caller wraps only to these bytes, never to
// the head's (DoR:938). The seal does not expire (no clock is read here): the caller re-verifies a
// status before each wrap, and asks the acknowledgement §2.13 requires for an
// unverified Segment (DoR:936).
func (v VerifiedStatus) WrapKey(servedSPKI []byte) ([]byte, bool) {
	w := v.wrap
	if w == nil || v.use != UseNewSecret {
		return nil, false
	}
	if len(w.ek) > 0 {
		return slices.Clone(w.ek), true
	}
	if !w.served || len(w.pin) == 0 || !bytes.Equal(Digest(servedSPKI), w.pin) {
		return nil, false
	}
	if _, err := ValidateEKSPKI(servedSPKI); err != nil {
		return nil, false
	}
	return slices.Clone(servedSPKI), true
}
