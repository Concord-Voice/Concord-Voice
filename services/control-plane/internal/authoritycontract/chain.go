package authoritycontract

import (
	"bytes"
	"slices"
	"strings"
	"time"
)

// Bundle and chain verification (§2.12, §2.11 R1–R4, §2.9, §2.10 T7/T8/T12).
//
// Invariant. A bundle extends the persisted head only when, in this order:
// every part passes EN5 and the restore positions are consistent (malformed);
// it names the head's realm and account (realm_mismatch); its intent, cp-authz
// and result agree on request and digests (transition_invalid); its seq is
// the head's + 1 (chain_gap) and it links to the head's entry (head_mismatch);
// its RSK signature verifies under a delegation retained at the (gen, serial)
// it names whose window covers issued_at and whose scope holds its op (T7);
// and then, under at least one such delegation (T7/T8 are existential over
// the retained set), R1, T8, R2 and the hypothesis-independent signatures
// pass, and exactly one R3 hypothesis that R4's fix leaves standing admits
// it through one §2.9 row, its intent positions, its current-root signature,
// its ek-binding, a candidate root that hypothesis's root_index does not
// already record (on a row that installs it), and its post-state. Only then
// does the index move (headmove).
//
// The input heads come from ParseChainHead (or ParseDeviceHead) or from an
// earlier verification: canonical in the sense of indexLen. VerifyBundle and
// VerifyChain refuse any other head as malformed before reading a record, so
// no hand-built head can reach a record read that assumes the codec's form
// (recordActivation's seq_index split) or the EN3 order the index lookups
// search by (rootRecordOf, ekIndexHas).
//
// A re-installed root (E2EE review A-B1). root_index keeps ONE record per
// root, written when the chain first makes that root active (addRoot skips a
// digest it already holds), and that record's segment label is what every
// later reader takes: VerifiedStatus.Segment, row 5's segment rule, row 11's
// cap (D-182) and row 20's binding-segment cap (LD-35, D-254). A result that
// installed an old root again would therefore keep the old, possibly
// stronger, label in place of the one its own row allowed. So on every row
// whose To installs the candidate (1, 2, 3, 5, 11, 20; on rows 2, 3 and 11
// whatever the outcome), a candidate root already in the hypothesis's
// root_index is transition_invalid. A6 holds: the authority's key_bindings
// are global and permanent, a root reused by the same account is root_reused
// (DoR:683) at step 10, which binds the candidate whatever the outcome, and
// root_index holds only roots this account made active, so it is a subset of
// what key_bindings refuses. The rule mirrors the ek_index refusal
// (ekChecks).
//
// The seq-0 hypothesis carries the seq-0 pin only where AccountMemory is an
// input (V3-05: "the seq-0 pin is AccountMemory.pin only"; the genesis head
// has none). VerifyChain takes AccountMemory's first-sight pin, or, when the
// caller has none, the pin of the status it is seeing for the first time
// (D-149). VerifyBundle has no AccountMemory, so its genesis hypothesis is A0
// with no pin, and a pinned account's first result verifies only through
// VerifyChain. checkPost keeps the pin immutable from the seq-0 hypothesis on,
// "" included, so a chain cannot start from one pin and continue under
// another.
//
// What breaks it: reading a delegation's g_ms or Δ from lookup (it refuses an
// ambiguous key) instead of from the delegation whose RSK signed; accepting
// on two hypotheses; letting an unverified fix constrain or a verified one be
// ignored; learning a pin from VerifyBundle's fix, which would take an RSK
// claim the first-sight rule (D-149) never judged and let a pending head
// verify as continuity under it; producing a head Parse… would refuse
// (E3-08); reading a record of a head that was never measured canonical; and
// letting a root-installing row name a root root_index already records.

// parsedBundle is one published bundle with every nested message decoded.
type parsedBundle struct {
	raw                       Bundle
	intent                    Intent
	authz                     CPAuthz
	res                       Result
	intentDigest, authzDigest []byte
	resBytes, entry           []byte
	ekb                       *EKBinding
	ekbDigest                 []byte
	bind                      *RecoveryBind
	bindDigest                []byte
	held                      *Status
	heldSigned                *Signed
}

// parseBundle is EN5 over every part, the recovery-bind per-kind rule inside
// a published bundle (V2-04), and the restore positions (LD-46, E3-09).
func parseBundle(b []byte) (parsedBundle, Reason) {
	var pb parsedBundle
	var err error
	if pb.raw, err = DecodeBundle(b); err != nil {
		return pb, ReasonMalformed
	}
	if pb.intent, err = DecodeIntent(pb.raw.Intent); err != nil {
		return pb, ReasonMalformed
	}
	if pb.authz, err = DecodeCPAuthz(pb.raw.CPAuthz); err != nil {
		return pb, ReasonMalformed
	}
	if pb.res, err = DecodeResult(pb.raw.Result); err != nil {
		return pb, ReasonMalformed
	}
	pb.intentDigest, pb.authzDigest = Digest(pb.raw.Intent), Digest(pb.raw.CPAuthz)
	pb.resBytes, pb.entry = pb.raw.Result, EntryDigest(pb.raw.Result)
	if r := pb.parseOptional(); r != ReasonOK {
		return pb, r
	}
	return pb, pb.parseRestore()
}

// parseOptional decodes the ek-binding and the recovery-bind when present.
func (pb *parsedBundle) parseOptional() Reason {
	if len(pb.raw.EKBinding) > 0 {
		e, err := DecodeEKBinding(pb.raw.EKBinding)
		if err != nil {
			return ReasonMalformed
		}
		pb.ekb, pb.ekbDigest = &e, Digest(pb.raw.EKBinding)
	}
	if len(pb.raw.RecoveryBind) > 0 {
		rb, err := DecodeRecoveryBind(pb.raw.RecoveryBind)
		if err != nil || RecoveryBindForKind(rb) != "" {
			return ReasonMalformed
		}
		pb.bind, pb.bindDigest = &rb, Digest(pb.raw.RecoveryBind)
	}
	return ReasonOK
}

// parseRestore is §2.9's restore positions: actor restore carries no CAK
// signature and a held_status, and no other actor carries one.
func (pb *parsedBundle) parseRestore() Reason {
	restore := pb.authz.Actor == ActorRestore
	if restore != (len(pb.raw.CPAuthzSig) == 0) || restore != (len(pb.raw.HeldStatus) > 0) {
		return ReasonMalformed
	}
	if !restore {
		return ReasonOK
	}
	sg, err := DecodeSigned(pb.raw.HeldStatus)
	if err != nil || sg.Type != ScopeStatus {
		return ReasonMalformed
	}
	st, err := DecodeStatus(sg.Msg)
	if err != nil {
		return ReasonMalformed
	}
	pb.held, pb.heldSigned = &st, &sg
	return ReasonOK
}

func (pb parsedBundle) sameAccount(h ChainHead) bool {
	for _, x := range [][2]string{{pb.intent.RealmID, pb.intent.AccountID}, {pb.authz.RealmID, pb.authz.AccountID},
		{pb.res.RealmID, pb.res.AccountID}} {
		if x[0] != h.RealmID || x[1] != h.AccountID {
			return false
		}
	}
	return true
}

func (pb parsedBundle) consistent() bool {
	in, a, r := pb.intent, pb.authz, pb.res
	return r.RequestID == in.RequestID && a.RequestID == in.RequestID && r.Op == in.Op &&
		bytes.Equal(r.IntentDigest, pb.intentDigest) && bytes.Equal(r.CPAuthzDigest, pb.authzDigest) &&
		bytes.Equal(a.IntentDigest, pb.intentDigest)
}

// bundleCtx is one bundle checked against one hypothesis.
type bundleCtx struct {
	t              *Trust
	head           ChainHead // the persisted head with this hypothesis's activation recorded
	pb             parsedBundle
	f              pub
	act            bool
	row            Row
	del            Delegation
	bindingSegment Assurance
	fix            *Status  // R4's retained status for the from-head (row 22 check 4)
	move           headMove // the edits that formed head from the persisted head (H_act's activation)
}

// rowChecks are the row-specific client checks: §2.22's binding proof (row
// 20), the pending recovery key (row 21) and §2.9's restore-cancel checks
// (row 22). A row that needs one and has none registered fails closed.
var rowChecks = map[int]func(*bundleCtx) Reason{}

func needsRowCheck(n int) bool { return n == 20 || n == 21 || n == 22 }

// independentSigs are the signatures no hypothesis changes (decision A3).
// sig_recovery is verified under a bind B's key only: a B of another kind
// names no key to bind (an unbind's is ""), and §2.22 check 1 refuses it as
// transition_invalid before check 4 (DoR §3.1, "an unbind B presented as a
// recover's binding proof"), which bindproof.go runs under every hypothesis.
func independentSigs(t *Trust, pb parsedBundle) Reason {
	b, in := pb.raw, pb.intent
	if len(b.SigCandidate) > 0 && len(in.CandidateRoot) > 0 &&
		!VerifyP384(in.CandidateRoot, sigMsg(RoleCandidate, pb.intentDigest), b.SigCandidate) {
		return ReasonBadSignature
	}
	if r := popCheck(pb); r != ReasonOK {
		return r
	}
	if len(b.SigOperator) > 0 {
		if r := t.checkT12(pb.intentDigest, b.SigOperator, pb.res.DelegationGen, pb.res.DelegationSerial,
			pb.authz.AssertedAtMs); r != ReasonOK {
			return r
		}
	}
	if in.Op == OpRecover && pb.bind != nil && pb.bind.Kind == BindKindBind && len(b.SigRecovery) > 0 &&
		!VerifyP384(pb.bind.RecoveryKey, sigMsg(RoleRecoveryKey, pb.intentDigest), b.SigRecovery) {
		return ReasonBadSignature
	}
	return ReasonOK
}

// popCheck is the PoP (D-243, E2-5): its SPKI's digest is the intent's
// legacy_ek_digest (else transition_invalid), then the PSS signature over the
// rebuilt legacy-ek-pop message.
func popCheck(pb parsedBundle) Reason {
	b, in := pb.raw, pb.intent
	if len(b.LegacyPopSig) == 0 {
		return ReasonOK
	}
	if !bytes.Equal(Digest(b.LegacySPKI), in.LegacyEKDigest) {
		return ReasonTransitionInvalid
	}
	pop := LegacyEKPoP{RealmID: in.RealmID, AccountID: in.AccountID, IntentDigest: pb.intentDigest,
		LegacyEKDigest: in.LegacyEKDigest}.Encode()
	if !VerifyLegacyPSS(b.LegacySPKI, pop, b.LegacyPopSig) {
		return ReasonBadSignature
	}
	return ReasonOK
}

// ekChecks: the binding's signer, generation and digest; the EK never bound
// earlier in the chain and never the pin (D-243, E2-5).
func (c *bundleCtx) ekChecks() Reason {
	e := c.pb.ekb
	if e == nil {
		return ReasonOK
	}
	signer := c.pb.intent.CandidateRoot
	if c.row.sigs&sigEKByCurrent != 0 {
		signer = c.f.Root
	}
	if e.RealmID != c.head.RealmID || e.AccountID != c.head.AccountID {
		return ReasonRealmMismatch
	}
	if !bytes.Equal(e.RootDigest, digestOrNil(signer)) || e.EKGeneration != c.f.EKGen+1 ||
		!bytes.Equal(c.pb.ekbDigest, c.pb.intent.EKBindingDigest) {
		return ReasonTransitionInvalid
	}
	if !VerifyP384(signer, c.pb.raw.EKBinding, c.pb.raw.EKBindingSig) {
		return ReasonBadSignature
	}
	spki := Digest(e.EKPublicKey)
	if bytes.Equal(spki, c.f.Pin) || bytes.Equal(spki, c.pb.res.LegacyEKDigest) || ekIndexHas(c.head, spki) {
		return ReasonTransitionInvalid
	}
	return ReasonOK
}

// ekIndexHas is d ∈ ek_index by binary search over the base64 text, the order
// insertB48 keeps (EN3). The head is canonical (indexLen: strictly
// ascending), so it answers exactly what a scan would.
func ekIndexHas(h ChainHead, d []byte) bool {
	_, found := slices.BinarySearchFunc(h.EKIndex, b64s(d), func(x []byte, s string) int { return strings.Compare(b64s(x), s) })
	return found
}

// installsCandidate is a row whose To installs the intent's candidate root:
// 1 (active), 2, 3 and 11 (pending on the P outcome), 5 (active) and 20 (Pr).
// Rows 4 and 12 burn theirs, and no other row carries one.
func installsCandidate(row Row) bool {
	switch row.to {
	case toEnrollActive, toPendOrDisp, toRotateRoot, toPr:
		return true
	}
	return false
}

// reinstallsRoot is A-B1 (see the invariant above): the row installs its
// candidate root and the hypothesis head's root_index already records it.
// Rows 2, 3 and 11 are refused on either outcome, as the authority refuses
// them: key_bindings is checked before the outcome matters.
func (c *bundleCtx) reinstallsRoot() bool {
	if !installsCandidate(c.row) || len(c.pb.intent.CandidateRoot) == 0 {
		return false
	}
	_, found := rootRecordOf(c.head, Digest(c.pb.intent.CandidateRoot))
	return found
}

// match checks the bundle against this hypothesis; ReasonOK is a match.
func (c *bundleCtx) match() Reason {
	p := presenceOf(c.pb.raw)
	row, r := selectRow(c.f, c.pb.intent.Op, c.pb.authz.Actor, p)
	if r != ReasonOK {
		return r
	}
	c.row = row
	if IntentForOp(c.pb.intent, c.pb.authz.Actor, p&pPoP != 0, p&pCurrent != 0) != "" {
		return ReasonTransitionInvalid
	}
	if r := checkIntentValues(row, c.f, c.pb.intent); r != ReasonOK {
		return r
	}
	if row.sigs&sigCurrent != 0 &&
		!VerifyP384(c.f.Root, sigMsg(RoleCurrentRoot, c.pb.intentDigest), c.pb.raw.SigCurrentRoot) {
		return ReasonBadSignature
	}
	if r := c.ekChecks(); r != ReasonOK {
		return r
	}
	if r := c.rowCheck(); r != ReasonOK {
		return r
	}
	if c.reinstallsRoot() {
		return ReasonTransitionInvalid
	}
	return checkPost(row, c.f, rowCtx{in: c.pb.intent, res: c.pb.res, outcomeP: c.pb.res.State == StatePending,
		segment: segmentOf(c.head, c.f.Root), bindingSegment: c.bindingSegment})
}

// rowCheck runs the row's registered check; a row that needs one and has
// none registered admits nothing.
func (c *bundleCtx) rowCheck() Reason {
	if !needsRowCheck(c.row.N) {
		return ReasonOK
	}
	chk := rowChecks[c.row.N]
	if chk == nil {
		return ReasonTransitionInvalid
	}
	return chk(c)
}

// fixStatus decodes a retained fix; "" is no fix.
func fixStatus(b []byte) (*Status, error) {
	if len(b) == 0 {
		return nil, nil
	}
	sg, err := DecodeSigned(b)
	if err != nil {
		return nil, err
	}
	if sg.Type != ScopeStatus {
		return nil, malformed("fix")
	}
	st, err := DecodeStatus(sg.Msg)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// chainStep is one verified result, kept for VerifyDeviceLedger. It keeps
// only the after-head's EK slots, never a head: each step's head can hold its
// own copy of an index (headmove.go), and a chain keeps every step, so a kept
// head per step held memory quadratic in the chain (CWE-400).
type chainStep struct {
	row  int // the §2.9 row the result matched
	res  Result
	pb   parsedBundle
	from pub
	act  bool
	ek   ekSlots
}

// ekSlots is a head's active and pending EK, the part of a head the ledger
// reads.
type ekSlots struct {
	active, pending       []byte
	activeGen, pendingGen uint64
}

func slotsOf(h ChainHead) ekSlots {
	return ekSlots{h.ActiveEKSPKI, h.PendingEKSPKI, h.ActiveEKGeneration, h.PendingEKGeneration}
}

// stepOut is one verified step and the head it moved to, which the walk
// carries to the next step and does not keep.
type stepOut struct {
	chainStep
	after    ChainHead
	afterIdx int // after's index length (headmove.go: indexLen), or -1 when not measured
}

// verifyBundleStep verifies one bundle against prev. seq0Pin is the pin of
// the seq-0 hypothesis (see the invariant above: VerifyChain's, from
// AccountMemory; nil from VerifyBundle); it is read only when prev is the
// genesis head. prevIdx is prev's index length when prev is canonical
// (indexLen), else -1; it decides how E3-08 is judged (headMove.fits).
func verifyBundleStep(t *Trust, hw HighWater, prev ChainHead, prevIdx int, raw []byte, seq0Pin []byte) (stepOut, bool, Reason) {
	pb, r := parseBundle(raw)
	if r != ReasonOK {
		return stepOut{}, false, r
	}
	pp, r := headPub(prev)
	if r != ReasonOK {
		return stepOut{}, false, r
	}
	if pp.Seq == 0 {
		pp.Pin = slices.Clone(seq0Pin)
	}
	if r := linkCheck(pb, prev, pp, t.realm); r != ReasonOK {
		return stepOut{}, false, r
	}
	sup, r := t.checkT7(hw, pb.res.DelegationGen, pb.res.DelegationSerial, pb.res.IssuedAtMs, pb.res.Op, pb.resBytes, pb.raw.ResultSig)
	if r != ReasonOK {
		return stepOut{}, false, r
	}
	st, r := underSigningDelegation(t, prev, prevIdx, pp, pb)
	if r != ReasonOK {
		return stepOut{}, false, r
	}
	return st, sup, ReasonOK
}

// linkCheck is realm/account, request and digest identity, seq and link.
// Every part names the head's realm and account, and that realm is the
// Trust's (B-SF1, D-115): the RSK and CAK checks that follow say which keys
// signed, not which realm the bundle speaks for.
func linkCheck(pb parsedBundle, prev ChainHead, pp pub, realm string) Reason {
	switch {
	case !pb.sameAccount(prev) || prev.RealmID != realm:
		return ReasonRealmMismatch
	case !pb.consistent():
		return ReasonTransitionInvalid
	case pb.res.Seq != pp.Seq+1:
		return ReasonChainGap
	case !bytes.Equal(pb.res.PrevEntryDigest, pp.Entry):
		return ReasonHeadMismatch
	}
	return ReasonOK
}

// underSigningDelegation runs R1–R4 and the row checks under each retained
// delegation at the result's (gen, serial) whose RSK signed it (T7 and T8 are
// existential, AC9), accepting the first under which the bundle verifies. The
// Reason reported when none does is that of the first such delegation in
// msg-byte order, so it does not depend on arrival order. More than one
// exists only in a retired generation (T4).
func underSigningDelegation(t *Trust, prev ChainHead, prevIdx int, pp pub, pb parsedBundle) (stepOut, Reason) {
	first := ReasonDelegationUnknown // unreachable after checkT7: some delegation's rskRule is OK
	tried := false
	for _, d := range t.delegations(pb.res.DelegationGen, pb.res.DelegationSerial) {
		if rskRule(d, pb.resBytes, pb.raw.ResultSig, pb.res.IssuedAtMs, pb.res.Op) != ReasonOK {
			continue
		}
		r := pre(t, pb, pp, d)
		var st stepOut
		if r == ReasonOK {
			st, r = pickHypothesis(t, prev, prevIdx, pp, pb, d)
		}
		if r == ReasonOK {
			return st, ReasonOK
		}
		if !tried {
			first, tried = r, true
		}
	}
	return stepOut{}, first
}

// pre is R1, T8, R2 and the hypothesis-independent signatures.
func pre(t *Trust, pb parsedBundle, pp pub, del Delegation) Reason {
	if r := checkIssued(pp.Issued, pb.res, pb.intent, pb.authz, del.GMs); r != ReasonOK {
		return r
	}
	if pb.authz.Actor != ActorRestore {
		if r := t.checkT8(pb.raw.CPAuthz, pb.raw.CPAuthzSig, pb.res.DelegationGen, pb.res.DelegationSerial,
			pb.authz.AssertedAtMs); r != ReasonOK {
			return r
		}
	}
	if r := checkANB(pb.res, pb.authz, del); r != ReasonOK {
		return r
	}
	return independentSigs(t, pb)
}

// pickHypothesis is R3 under R4: every hypothesis the fix leaves standing is
// matched, and exactly one must match (decision A3 for a single one). The fix
// is prev's retained one, which VerifyChain stored or VerifyBundle verified.
// No hypothesis copies prev: H_pend reads prev itself, H_act only the
// records its activation replaced or inserted (formHypothesis), and the
// after-head extends the matched one in place of a copy (headmove.go,
// "Moving a head in time linear in the chain").
func pickHypothesis(t *Trust, prev ChainHead, prevIdx int, pp pub, pb parsedBundle, del Delegation) (stepOut, Reason) {
	fix, err := fixStatus(prev.Fix)
	if err != nil {
		return stepOut{}, ReasonMalformed
	}
	var prevRes Result
	if pp.Seq > 0 {
		if prevRes, err = DecodeResult(prev.HeadResult); err != nil {
			return stepOut{}, ReasonMalformed
		}
	}
	hs := hypotheses(pp, pb.res.IssuedAtMs)
	rs := make([]Reason, len(hs))
	cs := make([]*bundleCtx, len(hs))
	for i, h := range hs {
		if !fixAllows(fix, pp, pb.res.IssuedAtMs, h.act) { // before formHypothesis: a ruled-out H_act copies nothing
			rs[i] = ReasonHypothesisConflict
			continue
		}
		head, move := formHypothesis(prev, prevRes, h)
		cs[i] = &bundleCtx{t: t, head: head, pb: pb, f: h.p, act: h.act, del: del, fix: fix, move: move}
		rs[i] = cs[i].match()
	}
	i, r := pickMatch(rs)
	if r != ReasonOK {
		return stepOut{}, r
	}
	c := cs[i]
	after, move := c.head, c.move
	move.appendResult(&after, pb, segmentOf(c.head, c.f.Root))
	afterIdx, ok := move.fits(after, prevIdx)
	if !ok { // E3-08: never produce a head Parse… refuses
		return stepOut{}, ReasonMalformed
	}
	return stepOut{chainStep: chainStep{row: c.row.N, res: pb.res, pb: pb, from: c.f, act: c.act, ek: slotsOf(after)},
		after: after, afterIdx: afterIdx}, ReasonOK
}

// VerifyBundle verifies one bundle against the persisted head (§2.12). prev
// must come from ParseChainHead or an earlier verification: a head whose
// index records the codec refuses, or whose a<…> positions are out of EN3
// order, is malformed (indexLen), and no record of it is read. A non-nil fix
// is offered to prev's stored fix (V3-05) once it passes the status checks
// that need neither the clock nor AccountMemory (authenticStatus: T6, realm
// and account, valid_until, naming prev, R5), and replaces it only where R4's
// retention rule lets it (keepsFix; RT-1): it never replaces an activation fix
// except by an earlier activation, so a caller that hands over the newest
// status it holds cannot drop the evidence row 22's check 4 and R4 read. A fix
// that fails the checks is refused with its own Reason. A fix has only its R4
// meaning: it can fix a pending head's hypothesis, never supply a pin. At
// genesis the hypothesis is A0 with no pin (V3-05: "the seq-0 pin is
// AccountMemory.pin only", and VerifyBundle takes no AccountMemory), so a
// pinned account's first result is refused here and verifies only through
// VerifyChain; a forged row 1 that verifies here meets the first-sight pin at
// VerifyStatus (RT-2). A superseded entry is
// superseded_uncovered here: only VerifyChain, with a covering status,
// accepts one (T7). Measuring prev (the guard above) is what lets its one
// step judge E3-08 from the bytes the step adds (headMove.fits).
//
// On every failure, superseded_uncovered included, it returns prev
// unchanged, fix included: a fix offered beside a bundle that fails is not
// retained, even though it passed its own checks. A caller therefore passes
// the newest status it holds on every call, or stores that status on its own
// through VerifyChain with no bundles. It never relies on one call having
// kept it.
func VerifyBundle(t *Trust, hw HighWater, prev ChainHead, bundle []byte, fix *Fix) (ChainHead, Reason) {
	if r := t.haltReason(); r != ReasonOK {
		return prev, r
	}
	in := clipIndexes(prev)
	idx, ok := indexLen(in)
	if !ok {
		return prev, ReasonMalformed
	}
	if fix != nil {
		a, r := authenticStatus(t, hw, prev, fix.Status)
		if r != ReasonOK {
			return prev, r
		}
		if !keepsFix(prev.Fix, a.st, a.hp) {
			in.Fix = slices.Clone(fix.Status) // the head outlives the caller's buffer (CWE-375)
		}
	}
	st, sup, r := verifyBundleStep(t, hw, in, idx, bundle, nil)
	if r != ReasonOK {
		return prev, r
	}
	if sup {
		return prev, ReasonSupersededUncovered
	}
	return st.after, ReasonOK
}

// stateReasons are the verdicts of a status that verified: the chain they
// describe may be persisted.
var stateReasons = map[Reason]bool{ReasonOK: true, ReasonOKAcceptedPin: true, ReasonSuspended: true,
	ReasonDisputed: true, ReasonHeld: true, ReasonErased: true, ReasonPendingErase: true, ReasonRootRevoked: true,
	ReasonPendingFromActive: true, ReasonPendingFromSuspended: true, ReasonPendingFromAbsent: true,
	ReasonPinRevoked: true, ReasonPinChanged: true, ReasonAbsentUnpinned: true, ReasonAbsentAfterCutover: true,
	ReasonAbsentAfterEnrolled: true}

// ChainInput is what the server serves for one VerifyChain call: the bundles
// after `from`, then the signed status naming the resulting head. Named fields
// keep the two byte arguments from being swapped silently.
type ChainInput struct {
	Bundles [][]byte
	Status  []byte
}

// VerifyChain verifies in.Bundles from `from`, then in.Status, the status
// naming the resulting head (§2.12). On any Reason outside stateReasons it returns
// `from` unchanged. `from` must come from ParseChainHead or an earlier
// verification: a head that is not canonical (indexLen) is malformed, and no
// record of it is read. A now that reads as 0 ms is malformed (Sec-N4; see
// VerifyStatus).
func VerifyChain(t *Trust, hw HighWater, from ChainHead, in ChainInput, mem AccountMemory,
	now time.Time, use Use) (ChainHead, VerifiedStatus, HighWater, Reason) {
	head, _, vs, hw2, r := t.verifyChainSteps(hw, from, in.Bundles, in.Status, mem, msOf(now), use)
	return head, vs, hw2, r
}

// chainSeq0Pin is the seq-0 hypothesis's pin for a chain verified from
// genesis: AccountMemory's first-sight pin (V3-05), or, for a caller that has
// none, the pin of the status this call is seeing for the first time (D-149:
// clients store the first pin they see; the caller stores VerifiedStatus.Pin).
// st has passed T6 (signedStatus), so the pin read is one an RSK signed.
func chainSeq0Pin(from ChainHead, bundles [][]byte, st Status, mem AccountMemory) []byte {
	if len(bundles) == 0 || len(from.HeadResult) != 0 || len(mem.Pin) > 0 {
		return mem.Pin
	}
	return st.LegacyEKDigest
}

// verifyChainSteps is VerifyChain in T7's order (DoR §2.10: "status
// signature, then chain, then head equality"): the status decodes and passes
// T6 first (signedStatus), then every bundle, then verifyStatus, which repeats
// T6 and judges head equality, R5, freshness and the state verdict.
//
// Its work is linear in the chain (CWE-400): `from` is measured once
// (indexLen, which also refuses a head that is not canonical), each step
// judges E3-08 from the bytes its own edits add
// (headMove.fits), and the head it returns is encoded once, at the end. No
// cap on len(bundles): first contact replays from seq 1 (D-120).
func (t *Trust) verifyChainSteps(hw HighWater, from ChainHead, bundles [][]byte, status []byte, mem AccountMemory,
	nowMs uint64, use Use) (ChainHead, []chainStep, VerifiedStatus, HighWater, Reason) {
	fail := func(r Reason) (ChainHead, []chainStep, VerifiedStatus, HighWater, Reason) {
		return from, nil, VerifiedStatus{Reason: r}, hw, r
	}
	if r := t.haltReason(); r != ReasonOK {
		return fail(r)
	}
	if nowMs == 0 { // msOf's reading of the zero time or any time before the epoch (Sec-N4)
		return fail(ReasonMalformed)
	}
	seen, _, r := signedStatus(t, hw, status)
	if r != ReasonOK {
		return fail(r)
	}
	seq0Pin := chainSeq0Pin(from, bundles, seen, mem)
	head, superseded := clipIndexes(from), false
	idx, ok := indexLen(head)
	if !ok { // not from Parse…: refuse before any step reads a record
		return fail(ReasonMalformed)
	}
	steps := make([]chainStep, 0, len(bundles))
	for _, b := range bundles {
		st, sup, r := verifyBundleStep(t, hw, head, idx, b, seq0Pin)
		if r != ReasonOK {
			return fail(r)
		}
		superseded = superseded || sup
		steps = append(steps, st.chainStep)
		head, idx = st.after, st.afterIdx
	}
	vs, hw2, r := verifyStatus(t, hw, head, status, mem, nowMs, use)
	if !stateReasons[r] {
		return fail(r)
	}
	if superseded && nowMs > vs.Status.ValidUntilMs { // T7: covered only under the history rule
		return fail(ReasonSupersededUncovered)
	}
	head = retainFix(head, status, vs.Status)
	if head.Bytes() == nil {
		return fail(ReasonMalformed)
	}
	return head, steps, vs, hw2, r
}

// retainFix is R4's retained status: kept only for a raw pending head, and
// replaced by a newly verified status only where keepsFix allows.
func retainFix(h ChainHead, signed []byte, st Status) ChainHead {
	hp, r := headPub(h)
	if r != ReasonOK || hp.State != StatePending {
		h.Fix = nil
		return h
	}
	if !keepsFix(h.Fix, st, hp) {
		h.Fix = slices.Clone(signed)
	}
	return h
}

// keepsFix is R4's retention rule (RT-1, RT-1b; DoR:642, :738, :2106): whether
// the retained fix old (stored bytes, "" for none) stays when st, a verified
// status naming the raw pending head hp, arrives. One slot holds one fix
// (D-269), so the rule keeps what the client must never lose:
//
//   - an activation fix is replaced only by an earlier activation: R4 fixes
//     H_act for issued >= T, so the earliest activation fixes the widest
//     range, and row 22's check 4 refuses a restore-cancel on it ("a
//     held_status never overrides a fix"). Neither a held or pending status
//     nor a later activation replaces it: either would let one RSK revert a
//     change the client saw activate (a restore-cancel, or a cancel
//     back-dated into the gap a later activation leaves), and the client
//     would wrap to the older EK again (DoR:2106);
//   - an activation replaces a pending fix, and what that fix fixed (H_pend
//     for issued <= T) is lost (RT-1c, a DoR question: full R4 needs a
//     pending and an activation fix at once, and the chain-head has one slot);
//   - of two pending fixes, held or not, the newer stays: it fixes H_pend for
//     the wider range (decision A8).
//
// A stored fix that does not decode or names another head fixes nothing
// (fixAllows), so any verified status replaces it; on a head that is not raw
// pending no fix is kept at all. What breaks it: replacing by as_of alone,
// whatever the two statuses show (decision A8 as first built).
func keepsFix(old []byte, st Status, hp pub) bool {
	if hp.State != StatePending {
		return false
	}
	o, err := fixStatus(old)
	if err != nil || o == nil || o.Seq != hp.Seq || !bytes.Equal(o.HeadEntryDigest, hp.Entry) {
		return false
	}
	oldAct, newAct := !effectivePending(hp, *o), !effectivePending(hp, st)
	switch {
	case oldAct:
		return !newAct || st.AsOfMs >= o.AsOfMs
	case newAct:
		return false
	}
	return o.AsOfMs >= st.AsOfMs
}
