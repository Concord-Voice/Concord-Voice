package authoritycontract

import (
	"bytes"
	"slices"
)

// §2.9's restore-cancel checks (row 22; LD-46, D-268, E3-09).
//
// Invariant. A co-member admits a row-22 result, which no root, PoP,
// recovery-key or CAK signature authorises, only on the evidence of the
// RSK-signed status the bundle carries, and only when (1) cp-authz binds that
// exact held_status by its SHA-384; (2) held_status names the delegation the
// result names (#25/#26), an RSK of a delegation retained at that (gen,
// serial) signed it, that delegation's scope holds `status` and its window
// covers the held status's as_of, and it was signed at the result's
// issued_at; (3) it names the result's realm and
// account (E3-09) and the from-head (seq and entry digest), passes R5 against
// that head, and shows it held; and (4) the client holds no fix that names
// the from-head and shows it activated. The actor, cp_authz_sig and
// held_status positions are parseBundle's (malformed); every failure here is
// transition_invalid except check 4, which is hypothesis_conflict.
//
// Check 2 is existential over the retained delegations at (gen, serial), as
// T6–T8 are (trustcheck.go): lookup refuses the ambiguous key a retired
// generation can hold, so reading "the delegation the result names" through
// it would refuse an honest held_status. Its delegation-name clause refuses a
// held_status signed under another delegation that lists the same RSK
// (DoR:1320); the authority signs both under one delegation in one
// transaction (§3.4 clear-held), so no honest restore is refused. Callers
// (VerifyBundle, VerifyChain) check haltReason first.
//
// Check 4 is not bounded by issued_at, unlike R4's own reading of a fix
// (fixAllows: H_act for issued ≥ T). An activation fix for the from-head
// refuses a restore-cancel issued at any instant (DoR:641, :644, :1320): a
// held_status never overrides it, so an RSK cannot cancel an activation the
// client saw by back-dating the restore. For issued ≥ T pickHypothesis's R4
// filter refuses the same H_pend first; for issued < T only check 4 does.
//
// What breaks it: accepting a held_status that shows held = 0 or names
// another head, account, realm or delegation; skipping the evidence_digest
// binding, which lets the CP pair any RSK status with the result; and letting
// a held_status override an activation fix the client already holds (E3-04,
// V3-09), at any issued_at.

func init() { rowChecks[22] = checkRestoreCancel }

// checkRestoreCancel is §2.9's restore-cancel checks 1–4.
func checkRestoreCancel(c *bundleCtx) Reason {
	pb := c.pb
	if pb.held == nil || pb.heldSigned == nil { // parseBundle sets both on actor restore
		return ReasonTransitionInvalid
	}
	if !bytes.Equal(pb.authz.EvidenceDigest, Digest(pb.raw.HeldStatus)) { // 1
		return ReasonTransitionInvalid
	}
	if !heldSignedUnderResultDelegation(c.t, pb) { // 2
		return ReasonTransitionInvalid
	}
	if !heldNamesFromHead(c.f, pb) { // 3
		return ReasonTransitionInvalid
	}
	if fixShowsActivated(c.fix, c.f) { // 4: a held_status never overrides a fix
		return ReasonHypothesisConflict
	}
	return ReasonOK
}

// heldSignedUnderResultDelegation is check 2: held_status names the result's
// (gen, serial), it is as of the result's issued_at, and ONE delegation
// retained there passes rskRule for it: lists the RSK that signed it, covers
// its as_of (DoR §2.10 T6's window, defence in depth: the result's own RSK
// rule already covers issued_at under some delegation at that name) and holds
// `status` in its scope.
func heldSignedUnderResultDelegation(t *Trust, pb parsedBundle) bool {
	st, res := pb.held, pb.res
	if st.DelegationGen != res.DelegationGen || st.DelegationSerial != res.DelegationSerial || st.AsOfMs != res.IssuedAtMs {
		return false
	}
	return slices.ContainsFunc(t.delegations(res.DelegationGen, res.DelegationSerial), func(d Delegation) bool {
		return rskRule(d, pb.heldSigned.Msg, pb.heldSigned.SigA, res.IssuedAtMs, ScopeStatus) == ReasonOK
	})
}

// heldNamesFromHead is check 3: held_status names the result's realm and
// account (E3-09; R5 copies both from the status, so it cannot see them), the
// from-head f (seq = the result's seq − 1, head_entry_digest = the result's
// prev_entry_digest), passes R5 against f and shows f held.
func heldNamesFromHead(f pub, pb parsedBundle) bool {
	st, res := pb.held, pb.res
	if st.RealmID != res.RealmID || st.AccountID != res.AccountID {
		return false
	}
	if st.Seq+1 != res.Seq || !bytes.Equal(st.HeadEntryDigest, res.PrevEntryDigest) {
		return false
	}
	return st.Held == 1 && checkR5(f, *st) == ReasonOK
}

// fixShowsActivated is check 4's condition: the retained fix names the
// from-head f (seq and head_entry_digest) and shows it activated, which only
// a raw pending head can be (R5's effective state; D(x) never activates). It
// reads f, the from-state of the hypothesis row 22 matched: row 22's From
// column is P, D and Pr, so that is always the raw from-head.
func fixShowsActivated(fix *Status, f pub) bool {
	if fix == nil || fix.Seq != f.Seq || !bytes.Equal(fix.HeadEntryDigest, f.Entry) {
		return false
	}
	return f.State == StatePending && !effectivePending(f, *fix)
}
