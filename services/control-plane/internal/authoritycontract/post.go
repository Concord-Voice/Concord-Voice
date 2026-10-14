package authoritycontract

import (
	"bytes"
	"slices"
)

// The row interpreter: §2.7's intent positions and §2.9's client-checkable
// columns, which is everything a co-member checks about a transition.
//
// Invariant. A co-member accepts result n+1 from the hypothesis f only when:
//
//   - exactly the rows of (intent op, cp-authz actor) are considered, and one
//     of them admits f's from-state code and guard and the bundle's signature
//     positions: required ⊆ present ⊆ required ∪ optional. A two-part
//     position (PoP and SPKI, ek-binding and its signature, the binding proof
//     and its signature) satisfies a requirement only when both parts are
//     non-empty, and a half-present one is never admitted at all;
//   - every intent position the op does not use is "" (IntentForOp), and the
//     head-bound positions equal f (checkIntentValues; decision A4);
//   - the result's post-state is exactly the row's To rule over f, its
//     recovery_gen is f's plus the row's Δrg, its pin is f's ("" on row 1,
//     whose account is outside the population), and its assurance is in the
//     row's allowed set (checkPost).
//
// The allowed assurance set is computed from f and from the segments read
// from the chain, never from the result. A segment the caller does not know
// ("" or a value outside §2.8's four labels) is read as unverified, as §2.8's
// monitor rule reads it and as DeriveAssurance does (knownSegment): the set
// then holds unverified and nothing stronger, and never "".
//
// What breaks it: counting half of a two-part position as present, so a row
// that requires a PoP is selected while the empty PoP signature is skipped by
// the verifier, and the row's label (continuity) is accepted unproven;
// passing an unknown segment straight to segmentRule, which returns "" and
// lets "" through on rotate_root, the LD-35 laundering; a default arm that
// admits an op §2.7 does not list, or a guard the table does not define; and
// computing an allowed set from the result's own fields.
//
// A6 (authority acceptance ⊆ client acceptance) holds because every column
// here is the weakest the DoR row states: rows 2, 3 and 11 admit P or D, row
// 11 and row 20 admit any label at or below their caps, and an unknown segment
// is reachable only from a head that does not record its own root (A9 records
// every installed root), which the authority, holding its own segment record,
// never is.

// presence is which signature-bearing bundle positions are non-empty.
type presence uint16

const (
	pCandidate presence = 1 << iota
	pCurrent
	pOperator
	pRecovery
	pPoP
	pEK
	pBindProof
	pHeld
	pHalf // a two-part position with one part missing: no row admits it
)

// pairPresence is a two-part position: full when both parts are non-empty,
// pHalf when exactly one is.
func pairPresence(a, b []byte, full presence) presence {
	switch {
	case len(a) > 0 && len(b) > 0:
		return full
	case len(a) > 0 || len(b) > 0:
		return pHalf
	}
	return 0
}

func presenceOf(b Bundle) presence {
	var p presence
	set := func(on bool, f presence) {
		if on {
			p |= f
		}
	}
	set(len(b.SigCandidate) > 0, pCandidate)
	set(len(b.SigCurrentRoot) > 0, pCurrent)
	set(len(b.SigOperator) > 0, pOperator)
	set(len(b.SigRecovery) > 0, pRecovery)
	set(len(b.HeldStatus) > 0, pHeld)
	p |= pairPresence(b.LegacyPopSig, b.LegacySPKI, pPoP)
	p |= pairPresence(b.EKBinding, b.EKBindingSig, pEK)
	p |= pairPresence(b.RecoveryBind, b.RecoveryBindSig, pBindProof)
	return p
}

// required maps a row's signature column onto bundle positions.
func required(s sigReq) presence {
	var p presence
	pairs := []struct {
		s sigReq
		p presence
	}{{sigCandidate, pCandidate}, {sigCurrent, pCurrent}, {sigOperator, pOperator}, {sigRecovery, pRecovery},
		{sigPendingRecovery, pRecovery}, {sigPoP, pPoP}, {sigEKByCandidate, pEK}, {sigEKByCurrent, pEK},
		{sigBindProof, pBindProof}, {sigHeldStatus, pHeld}}
	for _, x := range pairs {
		if s&x.s != 0 {
			p |= x.p
		}
	}
	return p
}

// guardHolds is the From-column guard g over f. gNone holds everywhere; a
// value the table does not define holds nowhere, so a row carrying one is
// never selected rather than selected unguarded (fail closed).
func guardHolds(g guard, f pub) bool {
	switch g {
	case gNone:
		return true
	case gUserNotInSet:
		return !has(f.Susp, ActorUser)
	case gUserInSetUnrevoked:
		return has(f.Susp, ActorUser) && f.RootRevoked == 0
	case gOpInSetUnrevoked:
		return has(f.Susp, ActorOperator) && f.RootRevoked == 0
	case gPriorAbsent:
		return f.Prior == StateAbsent
	case gPriorActiveOrSusp:
		return f.Prior == StateActive || f.Prior == StateSuspended
	case gRootUnrevoked:
		return len(f.Root) > 0 && f.RootRevoked == 0
	case gLivePinPriorAbsent:
		return f.Prior == StateAbsent && len(f.Pin) > 0 && f.PinRevoked == 0
	case gPriorNotAbsent:
		return f.Prior != StateAbsent
	case gAbsentPrior:
		return f.State == StateAbsent || f.Prior == StateAbsent
	}
	return false
}

// selectRow finds the unique row whose From, guard and signature pattern
// admit the bundle: required ⊆ present ⊆ required ∪ optional. No row is
// transition_invalid (a missing or extra signature, or a disallowed
// from-state), and so are two: the rows of one (op, actor) are disjoint by
// construction (TestSelectRowIsUniqueOverEveryFromAndPattern), so a second
// match is a table defect, refused rather than settled by table order.
func selectRow(f pub, op, actor string, p presence) (Row, Reason) {
	code := classify(f)
	var found Row
	n := 0
	for _, r := range rowsFor(op, actor) {
		if rowAdmits(r, code, f, p) {
			found, n = r, n+1
		}
	}
	if n != 1 {
		return Row{}, ReasonTransitionInvalid
	}
	return found, ReasonOK
}

// rowAdmits is one row's From code, guard and signature pattern.
func rowAdmits(r Row, code FromCode, f pub, p presence) bool {
	if !slices.Contains(r.From, code) || !guardHolds(r.guard, f) {
		return false
	}
	req, opt := required(r.sigs), required(r.optSigs)
	return p&req == req && p&^(req|opt) == 0
}

type need uint8

const (
	nEmpty need = iota // "—": MUST be ""
	nReq               // req
	nIff               // non-empty iff the flag
)

func posOK(n need, b []byte, flag bool) bool {
	switch n {
	case nReq:
		return len(b) > 0
	case nIff:
		return (len(b) > 0) == flag
	}
	return len(b) == 0
}

// intentNeeds is §2.7's table for one (op, actor): candidate_root,
// ek_binding_digest, legacy_ek_digest, predecessor_root_digest,
// device_intent_digest (#15) and recovery_bind_digest (#16). ok is false for
// an op §2.7 does not list, which no published intent may carry.
func intentNeeds(op, actor string) (cand, ek, legacy, pred, dev, bind need, ok bool) {
	switch op {
	case OpEnrollNew:
		return nReq, nReq, nEmpty, nEmpty, nReq, nEmpty, true
	case OpEnrollExisting:
		return nReq, nReq, nIff, nEmpty, nReq, nEmpty, true
	case OpRotateRoot, OpReplaceRoot:
		return nReq, nReq, nEmpty, nReq, nReq, nEmpty, true
	case OpRotateEK:
		return nEmpty, nReq, nEmpty, nReq, nReq, nEmpty, true
	case OpUnsuspend:
		dev = nEmpty
		if actor == ActorUser {
			dev = nReq
		}
		return nEmpty, nEmpty, nEmpty, nReq, dev, nEmpty, true
	case OpRecover:
		return nReq, nReq, nEmpty, nReq, nReq, nReq, true
	case OpCancelPending:
		return nEmpty, nEmpty, nIff, nIff, nEmpty, nEmpty, true
	case OpSuspend, OpRevokeRoot, OpErase:
		return nEmpty, nEmpty, nEmpty, nEmpty, nEmpty, nEmpty, true
	}
	return nEmpty, nEmpty, nEmpty, nEmpty, nEmpty, nEmpty, false
}

// IntentForOp is §2.7's position pattern (V-18): the authority records
// malformed_for_op; a co-member reports transition_invalid (decision A5).
// pop: a PoP accompanies the op; rootCancels: a cancel_pending the root signs.
// expected_head_digest is not a presence rule here: decision A4 compares it
// with the head's entry_digest (checkIntentValues), which is "" exactly at
// seq 0.
func IntentForOp(in Intent, actor string, pop, rootCancels bool) Refusal {
	cand, ek, legacy, pred, dev, bind, known := intentNeeds(in.Op, actor)
	ok := known && posOK(cand, in.CandidateRoot, false) && posOK(ek, in.EKBindingDigest, false) &&
		posOK(legacy, in.LegacyEKDigest, pop) && posOK(pred, in.PredecessorRootDigest, rootCancels) &&
		posOK(dev, in.DeviceIntentDigest, false) && posOK(bind, in.RecoveryBindDigest, false)
	if !ok {
		return RefusalMalformedForOp
	}
	return ""
}

// checkIntentValues compares the intent's head-bound positions with the
// hypothesis f (§2.7: expected_seq and expected_recovery_gen are always the
// head's; decision A4 for expected_head_digest; "= pin iff PoP"). A row
// selected for another op admits nothing.
func checkIntentValues(row Row, f pub, in Intent) Reason {
	if row.Op != in.Op {
		return ReasonTransitionInvalid
	}
	if in.ExpectedSeq != f.Seq || in.ExpectedRecoveryGen != f.RG || !bytes.Equal(in.ExpectedHeadDigest, f.Entry) {
		return ReasonTransitionInvalid
	}
	if len(in.PredecessorRootDigest) > 0 && !bytes.Equal(in.PredecessorRootDigest, digestOrNil(f.Root)) {
		return ReasonTransitionInvalid
	}
	if len(in.LegacyEKDigest) > 0 && !bytes.Equal(in.LegacyEKDigest, f.Pin) {
		return ReasonTransitionInvalid
	}
	return ReasonOK
}

// rowCtx is one bundle as the interpreter sees it.
type rowCtx struct {
	in             Intent
	res            Result
	outcomeP       bool      // the result is the P (not the D) outcome of rows 2, 3, 11
	segment        Assurance // the current segment's assurance (root_index record of f.Root)
	bindingSegment Assurance // row 20: the binding segment's (§2.22 check 5)
}

// want is the post-state a To rule permits.
type want struct {
	state, prior     string
	susp             []string
	root, ek         []byte
	rootRevoked      uint64
	pendRoot, pendEK []byte
	pinRevoked       uint64
}

func restoreX(f pub) want {
	susp := []string{}
	if f.Prior == StateSuspended {
		susp = f.Susp
	}
	return want{state: f.Prior, susp: susp, root: f.Root, ek: f.EK, rootRevoked: f.RootRevoked, pinRevoked: f.PinRevoked}
}

func setOfX(f pub) []string {
	if f.x() == StateSuspended {
		return f.Susp
	}
	return []string{}
}

func expected(row Row, f pub, c rowCtx) want {
	base := want{state: f.State, susp: f.Susp, root: f.Root, ek: f.EK, rootRevoked: f.RootRevoked, pinRevoked: f.PinRevoked}
	switch row.to {
	case toEnrollActive:
		return want{state: StateActive, susp: []string{}, root: c.in.CandidateRoot, ek: c.in.EKBindingDigest, pinRevoked: f.PinRevoked}
	case toPendOrDisp:
		w := base
		w.prior, w.susp = f.State, setOfX(f)
		if c.outcomeP {
			w.state, w.pendRoot, w.pendEK = StatePending, c.in.CandidateRoot, c.in.EKBindingDigest
		} else {
			w.state = StateDisputed
		}
		return w
	case toDispBurn:
		w := base
		w.state, w.prior, w.susp = StateDisputed, f.Prior, setOfX(f)
		return w
	case toRotateRoot:
		return want{state: StateActive, susp: []string{}, root: c.in.CandidateRoot, ek: c.in.EKBindingDigest, pinRevoked: f.PinRevoked}
	case toRotateEK:
		return want{state: StateActive, susp: []string{}, root: f.Root, ek: c.in.EKBindingDigest, pinRevoked: f.PinRevoked}
	case toSuspendAdd:
		w := base
		w.state, w.susp = StateSuspended, withSet(f.Susp, row.Actor)
		return w
	case toUnsuspend:
		w := base
		w.susp = withoutSet(f.Susp, row.Actor)
		w.state = StateSuspended
		if len(w.susp) == 0 {
			w.state = StateActive
		}
		return w
	case toRestore:
		return restoreX(f)
	case toAp:
		return want{state: StateAbsent, susp: []string{}, pinRevoked: 0}
	case toSor:
		w := base
		w.state, w.susp, w.rootRevoked = StateSuspended, withSet(setOfX(f), ActorOperator), 1
		return w
	case toApr:
		return want{state: StateAbsent, susp: []string{}, pinRevoked: 1}
	case toPendErase:
		w := base
		w.state, w.prior, w.susp = StatePending, f.x(), setOfX(f)
		return w
	case toErased:
		w := base
		w.state, w.susp = StateErased, []string{}
		return w
	case toPr:
		w := base
		w.state, w.prior, w.susp = StatePending, f.x(), setOfX(f)
		w.pendRoot, w.pendEK = c.in.CandidateRoot, c.in.EKBindingDigest
		return w
	}
	return want{}
}

// allowedAssurance is the Allowed-assurance column for this outcome. Every
// segment it reads passes through knownSegment first: a segment the caller
// does not know is unverified (§2.8 monitor rule), never "" and never a
// label it did not record.
func allowedAssurance(row Row, f pub, c rowCtx) []string {
	rec, unv := AssuranceRecovery, AssuranceUnverified
	if !c.outcomeP && (row.to == toPendOrDisp) {
		return []string{""}
	}
	switch row.assure {
	case aInitial:
		return []string{AssuranceInitial}
	case aRow2:
		return []string{AssuranceContinuity}
	case aRow3:
		if f.PinRevoked == 1 {
			return []string{unv}
		}
		return []string{rec, unv}
	case aSegment:
		return []string{string(segmentRule(knownSegment(c.segment)))}
	case aRow11:
		if classify(f) == FromSor {
			return []string{unv}
		}
		return capped([]string{rec, unv}, knownSegment(c.segment))
	case aRow20:
		return capped([]string{rec, unv}, capAt(Assurance(rec), knownSegment(c.bindingSegment)))
	}
	return []string{""}
}

func capped(vals []string, limit Assurance) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if rank(Assurance(v)) <= rank(limit) {
			out = append(out, v)
		}
	}
	return out
}

// checkPost compares the result's post-state, Δrg and assurance with the
// row (§2.9 client-checkable columns). R2 (anb) and the signatures are the
// caller's. Any mismatch is transition_invalid.
func checkPost(row Row, f pub, c rowCtx) Reason {
	w, r := expected(row, f, c), c.res
	prior := ""
	if w.state == StatePending || w.state == StateDisputed {
		prior = w.prior
	}
	ok := r.State == w.state && r.PriorState == prior && sameSet(r.SuspendedBy, w.susp) &&
		bytes.Equal(r.ActiveRoot, w.root) && r.RootRevoked == w.rootRevoked && bytes.Equal(r.EKBindingDigest, w.ek) &&
		bytes.Equal(r.PendingRoot, w.pendRoot) && bytes.Equal(r.PendingEKBindingDigest, w.pendEK) &&
		r.LegacyPinRevoked == w.pinRevoked && r.RecoveryGen == f.RG+row.DeltaRG &&
		slices.Contains(allowedAssurance(row, f, c), r.Assurance) &&
		bytes.Equal(r.LegacyEKDigest, f.Pin) // the pin never changes, "" included: f carries the seq-0 pin (V3-05)
	// Row 1 admits only an account outside the population (DoR:680), and the
	// pin is set only at a population account's first event (DoR:337), so a
	// row-1 result carries no pin whatever the hypothesis knows. The
	// authority never issues one with a pin, so this is never stricter than
	// it (A6).
	if row.Population == PopNotIn {
		ok = ok && len(r.LegacyEKDigest) == 0
	}
	if !ok {
		return ReasonTransitionInvalid
	}
	return ReasonOK
}
