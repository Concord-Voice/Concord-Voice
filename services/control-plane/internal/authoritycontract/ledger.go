package authoritycontract

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"time"
)

// VerifyDeviceLedger (§2.12; DR1–DR9, LD-32, D-240, D-257, D-272, C4-4).
//
// Invariant. A ledger extends `from` only when, in this order:
//
//   - the chain bundles verify exactly as VerifyChain verifies them, to a status
//     whose verdict lets the chain persist (stateReasons); that verdict is the
//     Reason returned on success;
//   - the device-status is RSK-signed under a retained delegation and carries the
//     realm, account, seq, head digest, as_of, valid_until and delegation of that
//     status (DR7; an older one is device_status_stale), and is within τ of now
//     whatever the Use (DoR:919, :1120; device_status_stale);
//   - the entries are contiguous in dseq and hash-linked from the persisted last
//     entry (device_gap, device_fork), and no entry's chain_seq is below the
//     previous entry's (DR7, D-272; device_rule);
//   - each published op whose intent #15 is set is followed by exactly one
//     attached entry at its seq, naming its entry digest, whose device part IS the
//     one #15 commits to (device_gap): the candidate or root signature over the
//     intent authorizes only that device part (D-240). Every other entry is a
//     private op naming the published head current at its commit (device_fork);
//   - each entry is consistent with itself and, attached, with its result
//     (§2.5's identity rules), uses only its op's and kind's positions (V2-04),
//     keeps R1's issued_at bounds against its cp-authz (device-result #14), has
//     every DR3 signature verify under a key the WALK holds — the actor's dsk from
//     the set before the entry, the root of the effective head at its chain_seq,
//     the bound recovery key — never a key the entry names for itself, and
//     ApplyDeviceOp admits it under a delegation whose RSK signed it, giving
//     exactly its device_set_digest (device_rule);
//   - the entry-less changes are applied where the walk derives them: DR8's
//     clearPending after rows 13, 14, 16, 17 and 21 and row 22's emptied slot;
//     the pending slot's completion (DR9 (c)); and the derived activation of a
//     raw pending head, which, when it installs a new root (P(x), Pr(x)), makes
//     the pending device the only active one, empties the slot and, for Pr,
//     burns the bound key. A Pe(x) activation installs no root and moves nothing
//     private, so row 18's erase keeps its pending device in the tombstone (DR8);
//   - the device-status's dseq, device_head_digest and device_set_digest equal
//     the walk's (device_behind, device_fork, device_status_mismatch). The set it
//     shows is the persisted set plus a new-root activation the status shows and
//     no result has recorded; the persisted set (device-head #7) stops before it.
//
// Timing is held as hypotheses, never computed: own devices cannot see when the
// authority completes a slot or activates a head (the accumulator, the grace's
// use, a hold that ended, the post-restore floor, the extension part of the
// anb), so authority acceptance ⊆ own-client acceptance (A6) only if the walk
// accepts every instant the authority may choose (DR9 (c), R3, decision A11).
// A hypothesis is one consistent history of those instants:
//
//   - the slot is pending before replacement_nb_ms. At every event at or after
//     it (an entry's issued_at, a result's issued_at, the status's as_of), a
//     hypothesis whose slot is still pending forks into "pending" and
//     "completed"; nothing bounds the pending one (G-41), and a completion holds
//     for every later event;
//   - a raw pending head reads as pending before its anb (R3). From the anb, a
//     hypothesis that has not activated it forks into "pending" and
//     "activated", within what fixed it: the from head's retained status (R4),
//     the next result's matched hypothesis (R3) or, for the last head, the
//     verified status (R4, R5). Each admits only H_pend when it shows the head
//     pending, and only H_act at or after the instant it shows it activated;
//   - when a completion and a new-root activation fall at one event, both
//     orders are held: completion first keeps the completed key, activation
//     first burns the still-pending slot (C4-4, LD-52). A tie completes first
//     (A10), which is the completion-first hypothesis.
//
// An entry keeps the hypotheses that admit it. One that none admits fails with
// the Reason they share, or device_rule when they differ (DR9 (c): it matches
// neither hypothesis), so an add_device or bind_recovery, which no H_pend
// admits, fixes H_act for every later entry (R3). A hypothesis an entry refused
// while another admitted it is kept aside, its later entries applied as
// ApplyDeviceOp admits them: a device-status that shows only such a hypothesis
// is device_rule, the refusing entry's (RT6-9, A11: a status showing the slot
// pending at T fixes it pending for every entry issued at or before T), and one
// that shows no hypothesis is device_status_mismatch. Verdicts that contradict
// each other (a status against the retained fix) leave no hypothesis:
// hypothesis_conflict. Every hypothesis the device-status shows must persist
// one device-head, or the call fails closed rather than pick one. A call holds
// at most maxLedgerHypotheses live hypotheses.
//
// Across calls, a hypothesis starts from what the persisted heads prove
// (carry): a new-root activation of the from head that the persisted set
// already holds is taken, never applied again, and the persisted slot keeps
// what the device-status co-signed with the from head's retained fix fixed
// about it (slotFloor).
//
// What breaks it: taking an actor, root or recovery key from the entry instead of
// from the walk; accepting an attached device part #15 does not commit to;
// letting a Pe activation promote or burn; completing a slot before
// replacement_nb_ms, or activating a head before its anb or against the
// verdict that fixed it; dropping a hypothesis the chain leaves open (that
// refuses an honest, extended ledger); completing a slot past a new-root
// activation that came first; applying again an activation the persisted set
// holds; completing the persisted slot at or before the as_of its fix showed
// it pending at, or before an activation that fix showed first; reading Δ or g
// from a lookup that hides an ambiguous key; and producing a head Parse…
// would refuse (E3-08).

// signedDeviceStatus is the `signed` type of a device-status (§2.6).
const signedDeviceStatus = "device-status"

type parsedDevice struct {
	raw        DeviceBundle
	di         *DeviceIntent // nil on the sheet form of cancel_recovery_bind
	dr         DeviceResult
	drBytes    []byte
	entry      []byte // SHA-384 of the device-result: the device entry digest
	authz      *CPAuthz
	bind       *RecoveryBind
	bindDigest []byte
}

// parseDevice is EN5 over every part of one entry (malformed). Position, kind
// and identity rules are device_rule and run in the walk (§2.12, V2-04).
func parseDevice(b []byte) (parsedDevice, Reason) {
	var p parsedDevice
	var err error
	if p.raw, err = DecodeDeviceBundle(b); err != nil {
		return p, ReasonMalformed
	}
	if p.dr, err = DecodeDeviceResult(p.raw.DeviceResult); err != nil {
		return p, ReasonMalformed
	}
	p.drBytes, p.entry = p.raw.DeviceResult, Digest(p.raw.DeviceResult)
	return p, p.parseNested()
}

func (p *parsedDevice) parseNested() Reason {
	if len(p.raw.DeviceIntent) > 0 {
		di, err := DecodeDeviceIntent(p.raw.DeviceIntent)
		if err != nil {
			return ReasonMalformed
		}
		p.di = &di
	}
	if len(p.raw.CPAuthz) > 0 {
		a, err := DecodeCPAuthz(p.raw.CPAuthz)
		if err != nil {
			return ReasonMalformed
		}
		p.authz = &a
	}
	if len(p.raw.RecoveryBind) > 0 {
		rb, err := DecodeRecoveryBind(p.raw.RecoveryBind)
		if err != nil {
			return ReasonMalformed
		}
		p.bind, p.bindDigest = &rb, Digest(p.raw.RecoveryBind)
	}
	return ReasonOK
}

func parseDevices(bundles [][]byte) ([]parsedDevice, Reason) {
	out := make([]parsedDevice, 0, len(bundles))
	for _, b := range bundles {
		p, r := parseDevice(b)
		if r != ReasonOK {
			return nil, r
		}
		out = append(out, p)
	}
	return out, ReasonOK
}

// d is the digest the entry's signatures cover: D, or H(B) on the sheet form.
func (p parsedDevice) d() []byte {
	if p.di == nil {
		return p.bindDigest
	}
	return Digest(p.raw.DeviceIntent)
}

// requestID is the request the entry's device-intent, or its B on the sheet
// form, names (device-result #5).
func (p parsedDevice) requestID() string {
	if p.di == nil {
		return p.bind.RequestID
	}
	return p.di.RequestID
}

// expires is the entry's expiry: device-intent #8, or B #7 on the sheet form.
func (p parsedDevice) expires() uint64 {
	if p.di == nil {
		return p.bind.ExpiresAtMs
	}
	return p.di.ExpiresAtMs
}

// rootCancelExpiryIsIntents is recovery-bind #7 on the root form of
// cancel_recovery_bind: B's expires_at_ms is device-intent #8 (DoR §2.22,
// E3-10, V3-06), as CheckDeviceSubmit requires at the authority. The sheet
// form has no device-intent, and its B #7 is its only expiry.
func (p parsedDevice) rootCancelExpiryIsIntents() bool {
	return p.di == nil || p.bind == nil || p.di.Op != OpCancelRecoveryBind || p.bind.ExpiresAtMs == p.di.ExpiresAtMs
}

func (p parsedDevice) sameAccount(realm, account string) bool {
	ids := [][2]string{{p.dr.RealmID, p.dr.AccountID}}
	if p.di != nil {
		ids = append(ids, [2]string{p.di.RealmID, p.di.AccountID})
	}
	if p.authz != nil {
		ids = append(ids, [2]string{p.authz.RealmID, p.authz.AccountID})
	}
	if p.bind != nil {
		ids = append(ids, [2]string{p.bind.RealmID, p.bind.AccountID})
	}
	return !slices.ContainsFunc(ids, func(x [2]string) bool { return x[0] != realm || x[1] != account })
}

// consistent is §2.5's identity rules for one entry: device-result #5, #6, #8
// and #11 against its device-intent (or B on the sheet form), #7 against its
// cp-authz, and an attached entry against the intent and result it belongs to.
func (p parsedDevice) consistent(step *chainStep) Reason {
	dr := p.dr
	if p.di == nil && (dr.Op != OpCancelRecoveryBind || p.bind == nil) {
		return ReasonDeviceRule // only the sheet form of cancel_recovery_bind has no device-intent
	}
	if !bytes.Equal(dr.DeviceIntentDigest, p.d()) || dr.RequestID != p.requestID() ||
		(p.bind != nil && p.bind.RequestID != dr.RequestID) || !p.rootCancelExpiryIsIntents() {
		return ReasonDeviceRule
	}
	if p.di != nil && (p.di.Op != dr.Op || (step == nil && p.di.ExpectedSeq != dr.ChainSeq)) {
		return ReasonDeviceRule
	}
	if r := p.authzConsistent(); r != ReasonOK {
		return r
	}
	if step != nil && !attachedTo(p, *step) {
		return ReasonDeviceRule
	}
	return ReasonOK
}

// authzConsistent is device-result #7: a cp-authz rides only on a private op,
// is digested there, and names the entry's request.
func (p parsedDevice) authzConsistent() Reason {
	if (p.authz != nil) != (len(p.dr.CPAuthzDigest) > 0) || (p.authz != nil && attachedOp(p.dr.Op)) {
		return ReasonDeviceRule
	}
	if p.authz != nil && (!bytes.Equal(p.dr.CPAuthzDigest, Digest(p.raw.CPAuthz)) || p.authz.RequestID != p.dr.RequestID) {
		return ReasonDeviceRule
	}
	return ReasonOK
}

// attachedTo is §2.5's attached identity: device-intent #5, #6, #9 and #10 are
// the published intent's, and the device-result carries the result's op,
// issued_at and delegation (§3.2.4 step 11: the same db_now and delegation).
func attachedTo(p parsedDevice, s chainStep) bool {
	in, res := s.pb.intent, s.res
	return p.di.RequestID == in.RequestID && p.di.Op == in.Op && p.di.ExpectedSeq == in.ExpectedSeq &&
		bytes.Equal(p.di.ExpectedHeadDigest, in.ExpectedHeadDigest) && p.dr.Op == res.Op &&
		p.dr.IssuedAtMs == res.IssuedAtMs && p.dr.DelegationGen == res.DelegationGen &&
		p.dr.DelegationSerial == res.DelegationSerial
}

// forOp is V2-04 on own devices (device_rule, §2.12): a B on exactly the ops
// that carry one, in its kind's positions and named by device-intent #18, and
// the device-intent positions its op uses. An attached proposal's new keys must
// differ from the published intent's candidate root (#12).
func (p parsedDevice) forOp(step *chainStep) Reason {
	usesBind := p.dr.Op == OpBindRecovery || p.dr.Op == OpCancelRecoveryBind
	if usesBind != (p.bind != nil) {
		return ReasonDeviceRule
	}
	if p.bind != nil && (RecoveryBindForKind(*p.bind) != "" || (p.di != nil && !bytes.Equal(p.di.RecoveryBindDigest, p.bindDigest))) {
		return ReasonDeviceRule
	}
	if p.di == nil {
		return ReasonOK
	}
	var candidate []byte
	if step != nil {
		candidate = step.pb.intent.CandidateRoot
	}
	if DeviceIntentForOp(*p.di, candidate) != "" {
		return ReasonDeviceRule
	}
	return ReasonOK
}

// issuedBounds is device-result #14: a private entry keeps R1's bounds against
// its cp-authz — asserted − ε ≤ issued < min(expires, asserted +
// INTENT_WINDOW_MS) — and a cancel_recovery_bind the cancel bounds, issued <
// asserted + g + CANCEL_SLACK_MS with expires > asserted, g being the grace of
// the delegation that signed it. An attached entry's issued_at is its result's,
// which R1 already bounded; a private entry with no cp-authz fails DR3 later.
func (p parsedDevice) issuedBounds(d Delegation) Reason {
	if p.authz == nil || attachedOp(p.dr.Op) {
		return ReasonOK
	}
	a, issued, expires := p.authz.AssertedAtMs, p.dr.IssuedAtMs, p.expires()
	if issued+EpsilonMs < a {
		return ReasonIssuedBounds
	}
	if p.dr.Op == OpCancelRecoveryBind {
		hi, ok := addMs(a, d.GMs, CancelSlackMs)
		if !ok || issued >= hi || expires <= a {
			return ReasonIssuedBounds
		}
		return ReasonOK
	}
	hi, ok := addMs(a, IntentWindowMs)
	if !ok {
		hi = MaxSafeInt
	}
	if issued >= min(expires, hi) {
		return ReasonIssuedBounds
	}
	return ReasonOK
}

// verifyDeviceStatus is DR7's device-status checks against the verified status:
// an RSK of a delegation retained at the (gen, serial) it names signed it, and it
// names that status's realm, account, seq, head, as_of, valid_until and
// delegation (an older one is stale).
func verifyDeviceStatus(t *Trust, b []byte, st Status) (DeviceStatus, Reason) {
	sg, err := DecodeSigned(b)
	if err != nil || sg.Type != signedDeviceStatus {
		return DeviceStatus{}, ReasonMalformed
	}
	ds, err := DecodeDeviceStatus(sg.Msg)
	if err != nil {
		return ds, ReasonMalformed
	}
	if !slices.ContainsFunc(t.delegations(ds.DelegationGen, ds.DelegationSerial),
		func(d Delegation) bool { return rskVerifies(d, sg.Msg, sg.SigA) }) {
		return ds, ReasonBadSignature
	}
	switch {
	case ds.RealmID != st.RealmID || ds.AccountID != st.AccountID:
		return ds, ReasonRealmMismatch
	case ds.ChainSeq < st.Seq || ds.AsOfMs < st.AsOfMs:
		return ds, ReasonDeviceStatusStale
	case ds.ChainSeq != st.Seq || !bytes.Equal(ds.ChainHeadDigest, st.HeadEntryDigest) || ds.AsOfMs != st.AsOfMs ||
		ds.ValidUntilMs != st.ValidUntilMs || ds.DelegationGen != st.DelegationGen || ds.DelegationSerial != st.DelegationSerial:
		return ds, ReasonDeviceStatusMismatch
	}
	return ds, ReasonOK
}

// maxLedgerHypotheses bounds the live hypotheses of one call. They differ only
// in the open instants: whether the one pending slot has completed, whether the
// raw pending head has activated, and, for a new-root activation of a pending
// slot, which came first. An event forks a hypothesis into at most 4; an entry's
// device_set_digest then pins the set, so the hypotheses that admit it differ at
// most in a Pe activation, which moves nothing private (2); and a result's
// matched hypothesis fixes its predecessor's activation, so a step forks only
// the slot. A ledger the authority writes therefore never holds more than 4
// after deduplication. One past this bound is refused (device_rule) rather than
// trimmed: dropping a live hypothesis could refuse an honest ledger silently.
const maxLedgerHypotheses = 8

// ledgerWalk is one hypothesis: the set, its position in the ledger, and the
// pending head whose derived activation it has taken. The chain it reads
// (from, fix, steps, final, asOf) is shared by every hypothesis of a call.
type ledgerWalk struct {
	t          *Trust
	hw         HighWater
	from       ChainHead
	fix        *Status // R4: the from head's retained status; nil when it has none
	steps      []chainStep
	final      pub    // the effective head the verified status shows
	asOf       uint64 // the verified status's as_of_ms
	set        []byte
	dseq       uint64
	last       []byte // the last device entry digest
	lastDR     []byte // the last device-result's bytes
	floor      uint64 // DR7's ordering floor: the last entry's chain_seq (D-272)
	actDone    uint64 // the seq whose derived activation this hypothesis has taken
	slotFloor  slotFloor
	admitted   map[string]string
	self       string
	secrets    []string
	superseded bool // an entry under a superseded delegation (T7)
}

// newLedgerWalk loads the persisted device-head and the from head's retained
// fix. Both are this package's own output (E3-08), so an inconsistent device
// head or a fix that does not decode is malformed.
func newLedgerWalk(t *Trust, hw HighWater, from DeviceHead, steps []chainStep, final pub, asOf uint64) (*ledgerWalk, Reason) {
	if from.RealmID != from.Chain.RealmID || from.AccountID != from.Chain.AccountID ||
		(len(from.DeviceSet) == 0) != (len(from.LastDeviceResult) == 0) {
		return nil, ReasonMalformed
	}
	fix, err := fixStatus(from.Chain.Fix)
	if err != nil {
		return nil, ReasonMalformed
	}
	w := &ledgerWalk{t: t, hw: hw, from: from.Chain, fix: fix, steps: steps, final: final, asOf: asOf,
		set: EmptyDeviceSet(from.RealmID, from.AccountID), admitted: map[string]string{},
		self: from.SelfDeviceID, secrets: slices.Clone(from.Secrets)}
	if len(from.LastDeviceResult) > 0 {
		set, err := DecodeDeviceSet(from.DeviceSet)
		dr, err2 := DecodeDeviceResult(from.LastDeviceResult)
		if err != nil || err2 != nil || set.RealmID != from.RealmID || set.AccountID != from.AccountID {
			return nil, ReasonMalformed
		}
		w.set, w.dseq, w.last, w.lastDR, w.floor = from.DeviceSet, dr.Dseq, Digest(from.LastDeviceResult),
			from.LastDeviceResult, dr.ChainSeq
		if r := w.carry(set, dr); r != ReasonOK {
			return nil, r
		}
	}
	for _, rec := range from.AdmittedBy {
		if checkAdmittedRecord(rec) != nil { // a hand-built head: never read past a record the codec refuses
			return nil, ReasonMalformed
		}
		id, by, _ := strings.Cut(rec, ".")
		w.admitted[id] = by
	}
	return w, ReasonOK
}

// carry is what the persisted heads prove at call start, which the walk must
// not re-open as though those instants were still undecided (§2.12, D-269).
//
// The from head's new-root activation is in the persisted set exactly when the
// set's pending device is gone. The from head's attached entry put the
// candidate's device there (DR5; intent #15 is required on every proposing op,
// §2.5), the last entry is at that head's seq (DR7), no private op touches
// pending (DR1–DR9), and every published op that clears it (DR6, DR8) moves the
// head. So only that activation, taken at an entry of an earlier call (an entry
// that needed the head active), empties it; the hypothesis has taken it, and
// applying it again would empty a slot and, for Pr, burn a key the owner bound
// after it, on every call until the next result. A set that stops before the
// activation (device-head #7: a status showed it and no entry or result
// recorded it) still holds the device, and the walk applies it as before.
func (w *ledgerWalk) carry(set DeviceSet, last DeviceResult) Reason {
	raw, r := headPub(w.from) // verifyChainSteps verified it, so this cannot refuse; it still does
	if r != ReasonOK {
		return r
	}
	base := uint64(len(w.from.SeqIndex))
	taken := installsRoot(raw) && set.Pending == "" && last.ChainSeq == base
	if taken {
		w.actDone = base
	}
	w.slotFloor = slotFloorOf(set, raw, w.fix, taken)
	return ReasonOK
}

// slotFloor is what the persisted heads prove about the slot the persisted set
// holds pending, through the from head's retained fix (R4). That fix is a
// status a VerifyDeviceLedger call verified for this raw pending head (an
// activation fix is the earliest activation one, keepsFix), and DR7
// co-signed a device-status at its as_of, so the floor it gives is a lower
// bound: either that device-status
// showed this set, so the slot was pending at that as_of and a completion at an
// event at or before it matches neither hypothesis (RT6-9: after); or the fix
// shows a new-root activation this set stops before (device-head #7), and the
// device-status showed it come first and burn the slot (C4-4; the completion
// first would have shown the completed key), so the slot never completes
// before that activation, which empties it (never). A later status older than
// the fix (it keeps the newest) has no new entry from an honest authority and
// only re-shows this set. Nothing is proven without a fix naming the from head:
// a head that is not raw pending keeps none (retainFix), so RT6-9 is not
// carried into a later call there. The floor binds only the slot with this
// digest; a later bind fills a slot it does not name.
//
// It rests on the pairing D-269 states: the chain-head is the one
// VerifyDeviceLedger persisted with this device-head. A fix from a VerifyChain
// call, paired by hand, was co-signed with no device-status.
type slotFloor struct {
	digest []byte // the slot's replacement_bind_digest; nil when nothing is proven
	after  uint64 // the fix's as_of_ms
	never  bool
}

func slotFloorOf(set DeviceSet, raw pub, fix *Status, taken bool) slotFloor {
	if len(set.ReplacementBindDigest) == 0 || !fixNames(fix, raw) {
		return slotFloor{}
	}
	return slotFloor{digest: set.ReplacementBindDigest, after: fix.AsOfMs,
		never: installsRoot(raw) && !taken && !effectivePending(raw, *fix)}
}

// fixNames reports a retained fix for the raw pending head p, read as fixAllows
// reads one: a fix for another head, or for a head that is not raw pending,
// proves nothing.
func fixNames(fix *Status, p pub) bool {
	return fix != nil && p.State == StatePending && fix.Seq == p.Seq && bytes.Equal(fix.HeadEntryDigest, p.Entry)
}

// holds reports that the floor keeps the slot with this digest pending at an
// event at x.
func (f slotFloor) holds(digest []byte, x uint64) bool {
	return bytes.Equal(f.digest, digest) && (f.never || x <= f.after)
}

func (w *ledgerWalk) clone() *ledgerWalk {
	c := *w
	c.admitted, c.secrets = maps.Clone(w.admitted), slices.Clone(w.secrets)
	return &c
}

// same reports an equal hypothesis: the set and the activation taken, and with
// full also the admitted and secrets records a live hypothesis persists.
func (w *ledgerWalk) same(o *ledgerWalk, full bool) bool {
	if w.actDone != o.actDone || !bytes.Equal(w.set, o.set) {
		return false
	}
	return !full || (maps.Equal(w.admitted, o.admitted) && slices.Equal(w.secrets, o.secrets))
}

// headAt is the verified published head at chain seq k (base = the from head's).
func (w *ledgerWalk) headAt(k uint64) (pub, ekSlots, bool) {
	base := uint64(len(w.from.SeqIndex))
	switch {
	case k == base:
		p, r := headPub(w.from)
		return p, slotsOf(w.from), r == ReasonOK
	case k < base || k-base > uint64(len(w.steps)):
		return pub{}, ekSlots{}, false
	}
	s := w.steps[k-base-1]
	return pubOfResult(s.res, s.pb.resBytes, s.ek.activeGen, s.ek.pendingGen), s.ek, true
}

// installsRoot reports a pending head whose derived activation installs a new
// root: P(x) or Pr(x). Only that activation touches the private set (DR8:
// active := {pending}, the slot emptied, a Pr's bound key burned; §3.2.4 step 7,
// DR9 (c) and (d)). A Pe(x) head has no pending root: it activates to E "with
// the root kept" (DoR:670) and keeps the set, its pending device and its slot
// (DR8: "erase keep[s] the set ... as part of the tombstone").
func installsRoot(p pub) bool { return p.State == StatePending && len(p.PendRoot) > 0 }

// verdict is what the chain fixed about the head at seq k: whether the next
// result matched H_act (R3) or, for the last head, whether the verified status
// shows it activated (R5), and the instant that fixed it.
func (w *ledgerWalk) verdict(k uint64) (act bool, at uint64) {
	if i := k - uint64(len(w.from.SeqIndex)); i < uint64(len(w.steps)) {
		return w.steps[i].act, w.steps[i].res.IssuedAtMs
	}
	return w.final.State != StatePending, w.asOf
}

// mayRead reports whether an event at x may read the raw pending head at seq k
// as activated (act) or as still pending: H_act only from its anb (R3), within
// the from head's retained fix (R4), and within the chain's verdict, which
// admits only H_pend when it shows the head pending and only H_act at or after
// the instant it shows it activated.
func (w *ledgerWalk) mayRead(raw pub, k, x uint64, act bool) bool {
	if act && x < raw.ANB {
		return false
	}
	if k == uint64(len(w.from.SeqIndex)) && !fixAllows(w.fix, raw, x, act) {
		return false
	}
	vAct, at := w.verdict(k)
	if !vAct {
		return !act
	}
	return act || x < at
}

// branch is one hypothesis at an event. act reports that it took the head's
// derived activation at that event, and pre is its set just before that
// activation (after a completion that came first): the set it persists when
// the event is the status (device-head #7).
type branch struct {
	w   *ledgerWalk
	pre []byte
	act bool
}

// branches is every hypothesis w leaves open at an event at x reading the raw
// head at seq k: the slot completed or not, the head activated or not, and a
// completion that falls with a new-root activation first or not (take). None
// when w took the activation at an instant the chain does not let x read.
func (w *ledgerWalk) branches(raw pub, k, x uint64) []branch {
	acts, completes := w.activationChoices(raw, k, x), w.completionChoices(x)
	out := make([]branch, 0, len(acts)*len(completes))
	for _, complete := range completes {
		for _, act := range acts {
			out = append(out, w.take(raw, k, complete, act))
		}
	}
	return out
}

// activationChoices: false takes no activation at x, true takes it now. A head
// that is not raw pending, or one w has already activated, offers only false.
func (w *ledgerWalk) activationChoices(raw pub, k, x uint64) []bool {
	switch {
	case raw.State != StatePending:
		return []bool{false}
	case w.actDone == k && w.mayRead(raw, k, x, true):
		return []bool{false}
	case w.actDone == k:
		return nil
	}
	var out []bool
	for _, act := range []bool{false, true} {
		if w.mayRead(raw, k, x, act) {
			out = append(out, act)
		}
	}
	return out
}

// completionChoices: a pending slot is still pending, or completed, at any x
// at or after replacement_nb_ms, with no upper bound (DR9 (c), G-41); before
// it, with no slot, or where an earlier call's device-status fixed it pending
// (slotFloor), the set is as it is.
func (w *ledgerWalk) completionChoices(x uint64) []bool {
	s, err := parseSet(w.set)
	if err != nil || len(s.ReplacementBindDigest) == 0 || x < s.ReplacementNbMs || w.slotFloor.holds(s.ReplacementBindDigest, x) {
		return []bool{false}
	}
	return []bool{false, true}
}

// take is one branch, on a copy of w: the completion, then the activation. So
// complete with a new-root activation is completion first (the completed key
// kept), and the activation alone burns a still-pending slot (C4-4, LD-52).
func (w *ledgerWalk) take(raw pub, k uint64, complete, act bool) branch {
	c := w.clone()
	if complete {
		c.set = NormalizeDeviceSet(c.set, false, false, true, false, false)
	}
	b := branch{w: c, pre: c.set, act: act}
	if act {
		c.actDone = k
		if installsRoot(raw) {
			c.set = NormalizeDeviceSet(c.set, true, raw.Op == OpRecover, false, false, false)
		}
	}
	return b
}

// forkAll is every branch of every hypothesis in ws at an event at x reading the
// raw head at seq k.
func forkAll(ws []*ledgerWalk, raw pub, k, x uint64) []*ledgerWalk {
	var out []*ledgerWalk
	for _, w := range ws {
		for _, b := range w.branches(raw, k, x) {
			out = append(out, b.w)
		}
	}
	return out
}

// dedupe drops a hypothesis equal to an earlier one (same; full for live ones).
func dedupe(ws []*ledgerWalk, full bool) []*ledgerWalk {
	var out []*ledgerWalk
	for _, w := range ws {
		if !slices.ContainsFunc(out, func(o *ledgerWalk) bool { return o.same(w, full) }) {
			out = append(out, w)
		}
	}
	return out
}

// ledger is one call's hypotheses. Every live one admits every entry so far; a
// refuted one is a hypothesis an entry refused while a live one admitted it,
// kept only so a device-status that shows it names that entry (A11).
type ledger struct {
	live, refuted []*ledgerWalk
}

// keep installs the next hypotheses. Live ones past maxLedgerHypotheses are
// device_rule (see its comment), and none left is hypothesis_conflict: the
// chain's own verdicts always leave one unless they contradict each other.
// Refuted ones are trimmed to the bound instead, since one dropped can only turn
// a refusal's device_rule into device_status_mismatch and never decides
// acceptance.
func (l *ledger) keep(live, refuted []*ledgerWalk) Reason {
	l.live, l.refuted = dedupe(live, true), dedupe(refuted, false)
	if len(l.refuted) > maxLedgerHypotheses {
		l.refuted = l.refuted[:maxLedgerHypotheses]
	}
	switch {
	case len(l.live) == 0:
		return ReasonHypothesisConflict
	case len(l.live) > maxLedgerHypotheses:
		return ReasonDeviceRule
	}
	return ReasonOK
}

// settle forks every hypothesis at an event at x reading the raw head at seq k.
func (l *ledger) settle(raw pub, k, x uint64) Reason {
	return l.keep(forkAll(l.live, raw, k, x), forkAll(l.refuted, raw, k, x))
}

// run merges the entries with the published steps (DR7): before step k → k+1,
// every private entry whose chain_seq is at most k, in dseq order (one below
// the floor is D-272's back-dating, refused in preEntry); then the step.
func (l *ledger) run(entries []parsedDevice) Reason {
	w := l.live[0]
	base := uint64(len(w.from.SeqIndex))
	end := base + uint64(len(w.steps))
	i := 0
	for k := base; ; k++ {
		for ; i < len(entries) && !attachedOp(entries[i].dr.Op) && entries[i].dr.ChainSeq <= k; i++ {
			if r := l.entry(entries[i], nil); r != ReasonOK {
				return r
			}
		}
		if k == end {
			break
		}
		var r Reason
		if i, r = l.step(entries, i, w.steps[k-base]); r != ReasonOK {
			return r
		}
	}
	if i != len(entries) { // an attached entry no published op claims, or a private entry ahead of the chain
		return ReasonDeviceRule
	}
	return ReasonOK
}

// step is one published result: every hypothesis forked at its issued_at on
// the head before it, whose activation its matched hypothesis fixes (R3); its
// attached entry (DR7's "exactly one"); then its entry-less change.
func (l *ledger) step(entries []parsedDevice, i int, s chainStep) (int, Reason) {
	k := s.res.Seq - 1
	raw, _, ok := l.live[0].headAt(k) // the head s was verified against: ok cannot be false; it still refuses
	if !ok {
		return i, ReasonMalformed
	}
	if r := l.settle(raw, k, s.res.IssuedAtMs); r != ReasonOK {
		return i, r
	}
	if len(s.pb.intent.DeviceIntentDigest) > 0 {
		if i >= len(entries) || !claims(entries[i], s) {
			return i, ReasonDeviceGap
		}
		if r := l.entry(entries[i], &s); r != ReasonOK {
			return i, r
		}
		i++
	}
	for _, w := range slices.Concat(l.live, l.refuted) {
		w.afterEvent(s)
	}
	return i, ReasonOK
}

// claims reports that p is step s's attached entry (DR7, D-240): an attached op
// at s's seq naming s's entry digest, whose device part is the D #15 commits to.
func claims(p parsedDevice, s chainStep) bool {
	return attachedOp(p.dr.Op) && p.dr.ChainSeq == s.res.Seq && bytes.Equal(p.dr.ChainEntryDigest, s.pb.entry) &&
		bytes.Equal(p.dr.DeviceIntentDigest, s.pb.intent.DeviceIntentDigest)
}

// afterEvent is DR8's entry-less change and row 22's emptied slot.
func (w *ledgerWalk) afterEvent(s chainStep) {
	switch s.row {
	case 13, 14, 16, 17, 21:
		w.set = NormalizeDeviceSet(w.set, false, false, false, true, false)
	case 22:
		w.set = NormalizeDeviceSet(w.set, false, false, false, true, true)
	}
}

// entry is one entry (step is its published step when attached; the step has
// already forked the hypotheses at its instant). The delegation-independent
// rules run once, on the position every live hypothesis shares; the rest runs
// under each hypothesis. The live hypotheses that admit it go on; one that none
// admits fails (sharedReason); those it refused while another admitted it join
// the refuted ones, which take it as ApplyDeviceOp admits it (skim).
func (l *ledger) entry(p parsedDevice, step *chainStep) Reason {
	dels, sup, r := l.admitEntry(p, step)
	if r != ReasonOK {
		return r
	}
	cand := dedupe(l.forks(l.live, p, step), true)
	if len(cand) == 0 {
		return ReasonHypothesisConflict
	}
	var live, refused []*ledgerWalk
	var why []Reason
	for _, w := range cand {
		if c, r := w.applyUnder(p, step, dels); r == ReasonOK {
			c.superseded = c.superseded || sup
			live = append(live, c)
		} else {
			refused, why = append(refused, w), append(why, r)
		}
	}
	if len(live) == 0 {
		return sharedReason(why)
	}
	refused = append(refused, l.forks(l.refuted, p, step)...)
	for _, w := range refused {
		w.skim(p, step, dels[0])
	}
	return l.keep(live, refused)
}

// admitEntry is the entry's place in the ledger and T7 (preEntry, checkT7),
// and the retained delegations at the (gen, serial) the device-result names
// whose RSK signed it, in msg-byte order (T7 is existential, AC9; Δ and g come
// from that delegation, never from lookup).
func (l *ledger) admitEntry(p parsedDevice, step *chainStep) ([]Delegation, bool, Reason) {
	w, dr := l.live[0], p.dr
	if r := w.preEntry(p, step); r != ReasonOK {
		return nil, false, r
	}
	sup, r := w.t.checkT7(w.hw, dr.DelegationGen, dr.DelegationSerial, dr.IssuedAtMs, dr.Op, p.drBytes, p.raw.DeviceResultSig)
	if r != ReasonOK {
		return nil, false, r
	}
	var dels []Delegation
	for _, d := range w.t.delegations(dr.DelegationGen, dr.DelegationSerial) {
		if rskRule(d, p.drBytes, p.raw.DeviceResultSig, dr.IssuedAtMs, dr.Op) == ReasonOK {
			dels = append(dels, d)
		}
	}
	if len(dels) == 0 {
		return nil, false, ReasonDelegationUnknown // unreachable after checkT7: some delegation's rskRule is OK
	}
	return dels, sup, ReasonOK
}

// forks is the hypotheses an entry is judged under, as copies: a private entry
// forks them at its issued_at on the head its chain_seq names; an attached
// entry takes them as its step left them, and so does an entry naming no
// verified head, which apply refuses (device_fork).
func (l *ledger) forks(ws []*ledgerWalk, p parsedDevice, step *chainStep) []*ledgerWalk {
	if step == nil && len(ws) > 0 {
		if raw, _, ok := ws[0].headAt(p.dr.ChainSeq); ok {
			return forkAll(ws, raw, p.dr.ChainSeq, p.dr.IssuedAtMs)
		}
	}
	out := make([]*ledgerWalk, len(ws))
	for i, w := range ws {
		out[i] = w.clone()
	}
	return out
}

// sharedReason is an entry's Reason when no hypothesis admits it: the one
// Reason all of them give, otherwise device_rule (DR9 (c): an entry that
// matches neither hypothesis). With one hypothesis it is that one's own.
func sharedReason(why []Reason) Reason {
	for _, r := range why[1:] {
		if r != why[0] {
			return ReasonDeviceRule
		}
	}
	return why[0]
}

// preEntry is the entry's place in the ledger and its own consistency:
// realm and account, dseq contiguity, the hash link, DR7's order (D-272),
// §2.5's identity rules and V2-04's positions, and that an op with an actor
// device was built on this private head.
func (w *ledgerWalk) preEntry(p parsedDevice, step *chainStep) Reason {
	dr := p.dr
	switch {
	case !p.sameAccount(w.from.RealmID, w.from.AccountID):
		return ReasonRealmMismatch
	case dr.Dseq != w.dseq+1:
		return ReasonDeviceGap
	case !bytes.Equal(dr.PrevDeviceEntryDigest, w.last):
		return ReasonDeviceFork
	case dr.ChainSeq < w.floor:
		return ReasonDeviceRule // DR7: never below an earlier entry's chain_seq (RT5-5, D-272)
	}
	if r := p.consistent(step); r != ReasonOK {
		return r
	}
	if r := p.forOp(step); r != ReasonOK {
		return r
	}
	// An op with an actor device names the private head it was built on
	// (DoR:413-414), and the authority refuses any other as stale_device_seq
	// (DoR:1449 (1)). Own devices refuse it too: otherwise an RSK holder could
	// replay an owner-signed device-intent at a later position, and a replayed
	// bind_recovery would re-bind a key its owner had already replaced.
	if p.di != nil && actorOpName(p.di.Op) &&
		(p.di.ExpectedDseq != w.dseq || !bytes.Equal(p.di.ExpectedDeviceHead, w.last)) {
		return ReasonDeviceRule
	}
	return ReasonOK
}

// applyUnder is the entry on a copy of w under the first delegation in dels
// under which it applies. The Reason when none does is the first one's, in
// msg-byte order, so it does not depend on arrival order.
func (w *ledgerWalk) applyUnder(p parsedDevice, step *chainStep, dels []Delegation) (*ledgerWalk, Reason) {
	first := ReasonOK
	for _, d := range dels {
		c := w.clone()
		r := c.apply(p, step, d)
		if r == ReasonOK {
			return c, ReasonOK
		}
		if first == ReasonOK {
			first = r
		}
	}
	return nil, first
}

// apply is the entry under delegation d: R1's bounds, the head it acted on,
// DR3's signatures, the shared rules and the recomputed set digest.
func (w *ledgerWalk) apply(p parsedDevice, step *chainStep, d Delegation) Reason {
	if r := p.issuedBounds(d); r != ReasonOK {
		return r
	}
	eff, ek, r := w.actedOn(p, step)
	if r != ReasonOK {
		return r
	}
	in := w.input(p, eff, step)
	if r := w.checkSigs(p, eff, in); r != ReasonOK {
		return r
	}
	set, ref := ApplyDeviceOp(w.set, in, p.dr.IssuedAtMs, d.DeltaMs)
	if ref != "" {
		return ReasonDeviceRule
	}
	if dig, err := DeviceSetDigest(set); err != nil || !bytes.Equal(dig, p.dr.DeviceSetDigest) {
		return ReasonDeviceRule
	}
	w.set, w.dseq, w.last, w.lastDR, w.floor = set, p.dr.Dseq, p.entry, p.drBytes, p.dr.ChainSeq
	if in.New != nil {
		w.admitted[in.New.ID] = in.Actor
	}
	w.recordSecrets(p, set, eff, ek, step)
	return ReasonOK
}

// skim applies p to a refuted hypothesis as ApplyDeviceOp admits it, so a
// device-status can be compared with the set that hypothesis shows (A11). It
// checks no signature and no recorded set digest, and an entry ApplyDeviceOp
// refuses leaves the set as it was.
func (w *ledgerWalk) skim(p parsedDevice, step *chainStep, d Delegation) {
	eff, _, r := w.actedOn(p, step)
	if r != ReasonOK {
		return
	}
	if set, ref := ApplyDeviceOp(w.set, w.input(p, eff, step), p.dr.IssuedAtMs, d.DeltaMs); ref == "" {
		w.set = set
	}
}

// actedOn is the published head an entry's op acted on and the EK active after
// it: an attached entry's from-state and its result's EK, or for a private
// entry the head its chain_entry_digest names (DR7), read as activated when
// this hypothesis has taken that head's derived activation.
func (w *ledgerWalk) actedOn(p parsedDevice, step *chainStep) (pub, []byte, Reason) {
	if step != nil {
		return step.from, step.ek.active, ReasonOK
	}
	dr := p.dr
	raw, ch, ok := w.headAt(dr.ChainSeq)
	if !ok {
		return pub{}, nil, ReasonDeviceFork
	}
	eff, ek := raw, ch.active
	if raw.State == StatePending && w.actDone == dr.ChainSeq {
		eff, ek = activate(raw), ch.pending
	}
	if !bytes.Equal(dr.ChainEntryDigest, eff.Entry) {
		return pub{}, nil, ReasonDeviceFork
	}
	return eff, ek, ReasonOK
}

// outcome is one hypothesis at the status's as_of: the set the device-status
// must show and the set it persists, which stops before a new-root activation
// the status shows and no result has recorded (device-head #7).
type outcome struct {
	w                *ledgerWalk
	persisted, shown []byte
}

// outcomes forks ws at as_of on the last head, whose activation the status
// fixes (R5).
func outcomes(ws []*ledgerWalk, raw pub, end, asOf uint64) []outcome {
	var out []outcome
	for _, w := range ws {
		for _, b := range w.branches(raw, end, asOf) {
			o := outcome{w: b.w, persisted: b.w.set, shown: b.w.set}
			if b.act {
				o.persisted = b.pre
			}
			out = append(out, o)
		}
	}
	return out
}

// resolve is DR7's last clause over the hypotheses: the device-status names the
// walk's last entry (device_behind, device_fork) and shows the set of a live
// hypothesis at its as_of. A set only a refuted hypothesis shows is the
// refusing entry's device_rule (RT6-9, A11); one none shows is
// device_status_mismatch. It returns the live outcomes the device-status shows.
func (l *ledger) resolve(ds DeviceStatus, raw pub, end, asOf uint64) ([]outcome, Reason) {
	w := l.live[0]
	switch {
	case ds.Dseq > w.dseq:
		return nil, ReasonDeviceBehind
	case ds.Dseq < w.dseq || !bytes.Equal(ds.DeviceHeadDigest, w.last):
		return nil, ReasonDeviceFork
	}
	live := outcomes(l.live, raw, end, asOf)
	if len(live) == 0 {
		return nil, ReasonHypothesisConflict // the status's verdict contradicts the retained fix
	}
	shows := func(o outcome) bool {
		dig, err := DeviceSetDigest(o.shown)
		return err == nil && bytes.Equal(dig, ds.DeviceSetDigest)
	}
	hits := slices.DeleteFunc(live, func(o outcome) bool { return !shows(o) })
	switch {
	case len(hits) > 0:
		return hits, ReasonOK
	case slices.ContainsFunc(outcomes(l.refuted, raw, end, asOf), shows):
		return nil, ReasonDeviceRule
	}
	return nil, ReasonDeviceStatusMismatch
}

// persistOne is the device-head every outcome the device-status shows
// persists, and the set they show. Two that would persist different heads fail
// closed (device_status_mismatch: the device-status has not fixed the ledger)
// rather than pick one; none of the authority's ledgers reaches it, since the
// shown set differs whenever the persisted one does.
func persistOne(hits []outcome, from DeviceHead, head ChainHead) (DeviceHead, []byte, Reason) {
	var out DeviceHead
	var first []byte
	for i, o := range hits {
		o.w.set = o.persisted
		dh := o.w.deviceHead(from, head)
		d, c := dh.Bytes()
		switch {
		case d == nil || c == nil: // E3-08
			return DeviceHead{}, nil, ReasonMalformed
		case i == 0:
			out, first = dh, d
		case !bytes.Equal(d, first):
			return DeviceHead{}, nil, ReasonDeviceStatusMismatch
		}
	}
	return out, hits[0].shown, ReasonOK
}

// DeviceLedgerInput is what the server serves for one VerifyDeviceLedger
// call. Named fields keep the four byte arguments from being swapped silently.
type DeviceLedgerInput struct {
	ChainBundles, DeviceBundles [][]byte
	Status, DeviceStatus        []byte
}

// VerifyDeviceLedger re-verifies in.ChainBundles from the ChainHead in from,
// then walks in.DeviceBundles (§2.12; see the invariant above). On success it returns
// the status's state verdict (any of stateReasons); on any failure, from
// unchanged and the failing Reason. from must come from ParseDeviceHead or an
// earlier verification: a record the codec refuses is malformed, and from's
// ChainHead must be the one that verification persisted with it (D-269).
func VerifyDeviceLedger(t *Trust, hw HighWater, from DeviceHead, in DeviceLedgerInput, mem AccountMemory, now time.Time,
	use Use) (ChainHead, DeviceHead, DeviceView, HighWater, Reason) {
	fail := func(r Reason) (ChainHead, DeviceHead, DeviceView, HighWater, Reason) {
		return from.Chain, from, DeviceView{}, hw, r
	}
	nowMs := msOf(now)
	head, steps, vs, hw2, verdict := t.verifyChainSteps(hw, from.Chain, in.ChainBundles, in.Status, mem, nowMs, use)
	if !stateReasons[verdict] {
		return fail(verdict)
	}
	ds, r := verifyDeviceStatus(t, in.DeviceStatus, vs.Status)
	if r != ReasonOK {
		return fail(r)
	}
	// A device-status older than τ is stale for every Use (DoR:919, :1120,
	// :1778). UseHistory relaxes the chain status to V, never the private
	// ledger: otherwise the CP could withhold a removal from an own device for
	// up to V. !ok cannot happen: verifyDeviceStatus requires the status's
	// (gen, serial), which authenticStatus resolved to exactly one delegation;
	// it still refuses.
	if d, ok := t.lookup(ds.DelegationGen, ds.DelegationSerial); !ok || olderThanTau(ds.AsOfMs, d, nowMs) {
		return fail(ReasonDeviceStatusStale)
	}
	hp, r := headPub(head) // verifyChainSteps produced it, so this cannot refuse; it still does
	if r != ReasonOK {
		return fail(r)
	}
	w, r := newLedgerWalk(t, hw, from, steps, effectiveOf(hp, vs.Status), vs.Status.AsOfMs)
	if r != ReasonOK {
		return fail(r)
	}
	entries, r := parseDevices(in.DeviceBundles)
	if r != ReasonOK {
		return fail(r)
	}
	l := &ledger{live: []*ledgerWalk{w}}
	if r := l.run(entries); r != ReasonOK {
		return fail(r)
	}
	if l.live[0].superseded && nowMs > vs.Status.ValidUntilMs { // T7: covered only under the history rule
		return fail(ReasonSupersededUncovered)
	}
	hits, r := l.resolve(ds, hp, hp.Seq, vs.Status.AsOfMs)
	if r != ReasonOK {
		return fail(r)
	}
	out, shown, r := persistOne(hits, from, head)
	if r != ReasonOK {
		return fail(r)
	}
	return head, out, viewOf(shown, l.live[0].dseq, ds.AsOfMs, out.AdmittedBy), hw2, verdict
}

// AuthenticateDeviceBundle is AuthenticateBundle's counterpart for a lone
// private-op device entry (§3.4 import; B3-D16, B3-D17, Go only, for the
// issuing authority): a halted or nil Trust admits nothing; EN5 over every
// part (parseDevice, malformed); every part naming t's realm and the device
// result's account (realm_mismatch); §2.5's identity rules for the entry with
// no step (consistent: the device result names its device-intent or B, its
// request and op, its cp-authz, and the device-intent's expected_seq is the
// result's chain_seq; device_rule); and T7's RSK signature, window and scope
// under a delegation t retains at the device result's (gen, serial). Each
// check is the one VerifyDeviceLedger runs on the entry (preEntry's identity
// clauses, then admitEntry's checkT7), so an entry the ledger admits
// authenticates here.
//
// An attached entry (an op whose device part rides a published result) is
// device_rule here: its identity is the published result's (attachedTo), which
// a lone entry does not carry, so it authenticates only beside that result.
//
// The invariant: an OK result is bytes an RSK of t's realm signed, within its
// delegation's window and scope, over exactly the device-intent (or B) and
// cp-authz beside it. It reads no device head, so it judges no dseq, no link,
// no set and no private signature (DR3): it is evidence that the authority's
// private head once reached res.Dseq, never that the entry extends a head the
// caller holds. superseded is T7's clause, reported and never refused. A
// refusal returns the zero DeviceResult.
//
// What breaks it: returning the decoded result before T7 or the identity check
// has passed; accepting an attached entry alone; or a client calling it in
// place of VerifyDeviceLedger, which needs the device-status and the chain
// (TestAuthorityOnlyExportsStayInTheAuthority).
func AuthenticateDeviceBundle(t *Trust, hw HighWater, bundle []byte) (DeviceResult, bool, Reason) {
	if r := t.haltReason(); r != ReasonOK {
		return DeviceResult{}, false, r
	}
	p, r := parseDevice(bundle)
	if r != ReasonOK {
		return DeviceResult{}, false, r
	}
	switch {
	case !p.sameAccount(t.realm, p.dr.AccountID):
		return DeviceResult{}, false, ReasonRealmMismatch
	case attachedOp(p.dr.Op):
		return DeviceResult{}, false, ReasonDeviceRule
	}
	if r := p.consistent(nil); r != ReasonOK {
		return DeviceResult{}, false, r
	}
	sup, r := t.checkT7(hw, p.dr.DelegationGen, p.dr.DelegationSerial, p.dr.IssuedAtMs, p.dr.Op, p.drBytes, p.raw.DeviceResultSig)
	if r != ReasonOK {
		return DeviceResult{}, false, r
	}
	return p.dr, sup, ReasonOK
}

// deviceHead is the persisted device-head; device_set is "" until the first entry.
func (w *ledgerWalk) deviceHead(from DeviceHead, head ChainHead) DeviceHead {
	out := DeviceHead{RealmID: from.RealmID, AccountID: from.AccountID, SelfDeviceID: from.SelfDeviceID,
		LastDeviceResult: w.lastDR, AdmittedBy: w.admittedRecords(), Secrets: w.secrets, Chain: head}
	if w.dseq > 0 {
		out.DeviceSet = w.set
	}
	return out
}

// admittedRecords are the admitted_by records of the devices in the persisted
// set's active and pending.
func (w *ledgerWalk) admittedRecords() []string {
	s, err := parseSet(w.set)
	if err != nil {
		return nil
	}
	live := map[string]bool{}
	for _, e := range s.act {
		live[e.ID] = true
	}
	if s.pend != nil {
		live[s.pend.ID] = true
	}
	var out []string
	for id, by := range w.admitted {
		if live[id] {
			out = append(out, AdmittedRecord(id, by))
		}
	}
	slices.Sort(out)
	return out
}

// input builds DeviceOpInput from the entry (D-270). The new device's maturity
// is ApplyDeviceOp's to set (DR2, DR5); a recover's B rides in its published
// bundle, not in the device-bundle.
func (w *ledgerWalk) input(p parsedDevice, eff pub, step *chainStep) DeviceOpInput {
	in := DeviceOpInput{Op: p.dr.Op, Bind: p.raw.RecoveryBind, PriorSig: len(p.raw.SigRecoveryPrior) > 0,
		RootForm: p.dr.Op == OpCancelRecoveryBind && p.di != nil, Head: headInputOf(eff)}
	if p.di != nil {
		in.Actor, in.Removed, in.BindWait = p.di.ActorDeviceID, p.di.RemovedDeviceIDs, p.di.BindWait
		in.New = newDeviceOf(*p.di)
	}
	if step != nil {
		in.ProposingAnbMs, in.Outcome = step.res.ActivationNotBeforeMs, outcomeOf(step.res.State)
		if p.dr.Op == OpRecover {
			in.Bind = step.pb.raw.RecoveryBind
		}
	}
	return in
}

func headInputOf(p pub) HeadInput {
	return HeadInput{State: p.State, Seq: p.Seq, EntryDigest: p.Entry, RootDigest: digestOrNil(p.Root)}
}

// newDeviceOf is device-intent #14–#16 (forOp has required all or none); DR1
// checks that #14 is DeviceID(#15).
func newDeviceOf(di DeviceIntent) *DeviceEntry {
	if di.NewDeviceID == "" {
		return nil
	}
	e := DeviceEntry{ID: di.NewDeviceID}
	copy(e.DSK[:], di.NewDeviceDSK)
	copy(e.DKX[:], di.NewDeviceDKX)
	return &e
}

// outcomeOf is DR5/DR6's P or D outcome of a proposing result.
func outcomeOf(state string) string {
	switch state {
	case StatePending:
		return "P"
	case StateDisputed:
		return "D"
	}
	return ""
}

// devSig is one DR3 signature: sig over sigMsg(role, digest) under key.
type devSig struct {
	sig, key, digest []byte
	role             string
}

// check: a missing signature, or a key the walk does not hold, is device_rule;
// a present signature that fails is bad_signature.
func (q devSig) check() Reason {
	if len(q.sig) == 0 || len(q.key) == 0 {
		return ReasonDeviceRule
	}
	if !VerifyP384(q.key, sigMsg(q.role, q.digest), q.sig) {
		return ReasonBadSignature
	}
	return ReasonOK
}

// devSigs is DR3 for the entry. Every key comes from the walk: the actor's dsk
// from the set before the entry, the root of the effective head at its
// chain_seq (device-intent #9 on the root-form cancel, which consistent pins to
// chain_seq), the bound recovery key; only sig_new_device and sig_recovery on a
// bind verify under the key the entry introduces, which is what they prove.
func devSigs(p parsedDevice, eff pub, in DeviceOpInput, s devSet) []devSig {
	D, HB, b := p.d(), p.bindDigest, p.raw
	var qs []devSig
	if in.Actor != "" {
		qs = append(qs, devSig{b.SigActor, activeDSK(s, in.Actor), D, RoleActorDevice})
	}
	if in.New != nil {
		qs = append(qs, devSig{b.SigNewDevice, in.New.DSK[:], D, RoleNewDevice})
	}
	switch p.dr.Op {
	case OpAddDevice:
		qs = append(qs, devSig{b.SigRoot, eff.Root, D, RoleCurrentRoot})
	case OpBindRecovery:
		qs = append(qs, devSig{b.SigRoot, eff.Root, HB, RoleCurrentRoot})
		if p.bind.Kind != BindKindUnbind {
			qs = append(qs, devSig{b.SigRecovery, p.bind.RecoveryKey, HB, RoleRecoveryKey})
		}
		if in.PriorSig {
			qs = append(qs, devSig{b.SigRecoveryPrior, s.RecoveryKey, HB, RoleRecoveryKey})
		}
	case OpCancelRecoveryBind:
		if in.RootForm {
			qs = append(qs, devSig{b.SigRoot, eff.Root, HB, RoleCurrentRoot})
		} else {
			qs = append(qs, devSig{b.SigRecovery, s.RecoveryKey, HB, RoleRecoveryKey})
		}
	}
	return qs
}

func activeDSK(s devSet, id string) []byte {
	if i := s.activeIdx(id); i >= 0 {
		return s.act[i].DSK[:]
	}
	return nil
}

// checkSigs is DR3: the private signatures, then a private op's cp-authz over
// D (H(B) on the sheet form, LD-34) under a CAK T8 admits.
func (w *ledgerWalk) checkSigs(p parsedDevice, eff pub, in DeviceOpInput) Reason {
	s, err := parseSet(w.set)
	if err != nil {
		return ReasonDeviceRule
	}
	for _, q := range devSigs(p, eff, in, s) {
		if r := q.check(); r != ReasonOK {
			return r
		}
	}
	if attachedOp(p.dr.Op) {
		return ReasonOK
	}
	if p.authz == nil || !bytes.Equal(p.authz.IntentDigest, p.d()) {
		return ReasonDeviceRule
	}
	return w.t.checkT8(p.raw.CPAuthz, p.raw.CPAuthzSig, p.dr.DelegationGen, p.dr.DelegationSerial, p.authz.AssertedAtMs)
}

// recordSecrets keeps self's secrets entries (device-secrets check 3): an
// add_device admitting self, with the effective root and EK it was admitted
// under, or a rotate_root/rotate_ek after which self is active, with the
// result's root (rotate_root) and the EK the result made active.
func (w *ledgerWalk) recordSecrets(p parsedDevice, set []byte, eff pub, ek []byte, step *chainStep) {
	if w.self == "" {
		return
	}
	var root []byte
	switch op := p.dr.Op; {
	case op == OpAddDevice && p.di.NewDeviceID == w.self:
		root = eff.Root
	case step != nil && (op == OpRotateRoot || op == OpRotateEK) && selfActive(set, w.self):
		if op == OpRotateRoot {
			root = step.res.ActiveRoot
		}
	default:
		return
	}
	w.secrets = append(w.secrets, SecretsRecord(p.entry, p.dr.Op, root, ek))
}

func selfActive(set []byte, self string) bool {
	s, err := parseSet(set)
	return err == nil && s.activeIdx(self) >= 0
}
