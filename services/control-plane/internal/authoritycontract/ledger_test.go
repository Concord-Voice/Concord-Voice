package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The walk's hypotheses, driven directly: the derived activation (DR8), the
// slot completion (DR9 (c)) and their order (C4-4, decision A10), each held as
// the hypotheses own devices cannot tell apart (A6, A11, R3), and the
// device-status's resolution of them. The vectors drive the same code through
// VerifyDeviceLedger; these pin the cases one at a time, including the ones a
// vector cannot reach (a Pe activation before an entry: no row admits an entry
// on an erased head; a status contradicting the retained fix).

const tLedgerG = uint64(900_000)

type ledgerFixture struct {
	self, next DeviceEntry
	key, rkey  []byte
	s          setB
}

// newLedgerFixture is a set with self active, next pending, a bound key and a
// pending refresh whose replacement_nb_ms is 10 000.
func newLedgerFixture(t *testing.T) ledgerFixture {
	f := ledgerFixture{self: testDev(t, 1), next: testDev(t, 5), key: testPoint(t), rkey: testPoint(t)}
	f.s = setB{realm: newID(), acct: newID(), active: []DeviceEntry{f.self}, pending: &f.next, key: f.key,
		dig: digestOf("bound"), rkey: f.rkey, rdig: digestOf("refresh"), nb: 10_000}
	return f
}

// walk is a hypothesis over the fixture's set whose chain is one head, hp at
// seq 3, which the verified status at asOf shows activated (act) or as it is.
func (f ledgerFixture) walk(t *testing.T, hp pub, act bool, asOf uint64) *ledgerWalk {
	final := hp
	if act {
		final = activate(hp)
	}
	return &ledgerWalk{set: f.s.bytes(t), final: final, asOf: asOf, from: ChainHead{SeqIndex: make([]string, 3)}}
}

func pendingHead(t *testing.T, op string, anb uint64) pub {
	return pub{Seq: 3, State: StatePending, Prior: StateActive, PendRoot: testPoint(t), ANB: anb, Op: op, Entry: digestOf("p")}
}

// sets is the sets of a list of branches, decoded.
func sets(t *testing.T, bs []branch) []DeviceSet {
	out := make([]DeviceSet, len(bs))
	for i, b := range bs {
		out[i] = decodeSet(t, b.w.set)
	}
	return out
}

func TestVerifyDeviceLedgerBranchesActivation(t *testing.T) {
	f := newLedgerFixture(t)
	at := uint64(5_000) // before the slot's replacement_nb_ms

	p := pendingHead(t, OpReplaceRoot, at)
	bs := f.walk(t, p, true, at).branches(p, 3, at)
	require.Len(t, bs, 1, "the status shows it activated at the event: H_act only")
	got := decodeSet(t, bs[0].w.set)
	require.Equal(t, []string{f.next.Dev()}, got.Active, "DR8: the pending device is the only active one")
	require.Empty(t, got.Pending)
	require.Empty(t, got.ReplacementBindDigest, "LD-52: a new-root activation burns the pending slot")
	require.Equal(t, f.key, got.RecoveryKey, "a replace_root activation keeps the bound key")
	require.Equal(t, f.s.bytes(t), bs[0].pre, "pre is the set before the activation")

	p = pendingHead(t, OpRecover, at)
	got = decodeSet(t, f.walk(t, p, true, at).branches(p, 3, at)[0].w.set)
	require.Equal(t, []string{f.next.Dev()}, got.Active)
	require.Empty(t, got.RecoveryKey, "DR8: a Pr activation burns the bound key")
	require.Empty(t, got.RecoveryBindDigest)

	pe := pub{Seq: 3, State: StatePending, Prior: StateActive, ANB: at, Op: OpErase} // Pe: no pending root
	bs = f.walk(t, pe, true, at).branches(pe, 3, at)
	require.Len(t, bs, 1)
	require.Equal(t, f.s.bytes(t), bs[0].w.set, "DR8, DoR:670: a Pe activation keeps the set, its pending device and its slot")
	require.Equal(t, uint64(3), bs[0].w.actDone, "a Pe activation is still recorded once")

	p = pendingHead(t, OpReplaceRoot, at)
	bs = f.walk(t, p, true, at+9_000).branches(p, 3, at-1)
	require.Len(t, bs, 1, "R3: nothing activates before the anb")
	require.Equal(t, f.s.bytes(t), bs[0].w.set)
	require.False(t, bs[0].act)

	w := f.walk(t, p, true, at)
	w.actDone, w.set = 3, f.walk(t, p, true, at).branches(p, 3, at)[0].w.set
	bs = w.branches(p, 3, at+1)
	require.Len(t, bs, 1, "an activation applies once")
	require.False(t, bs[0].act)
	require.Equal(t, w.set, bs[0].w.set)
}

// R3, R4 and R5 fix the head's activation; between its anb and the verdict that
// fixes it, both readings stand.
func TestVerifyDeviceLedgerBranchesActivationHypotheses(t *testing.T) {
	f := newLedgerFixture(t)
	p := pendingHead(t, OpReplaceRoot, 5_000)
	acts := func(w *ledgerWalk, x uint64) []bool {
		bs := w.branches(p, 3, x)
		out := make([]bool, len(bs))
		for i, b := range bs {
			out[i] = b.act
		}
		return out
	}
	require.Equal(t, []bool{false, true}, acts(f.walk(t, p, true, 9_000), 6_000),
		"from the anb to the status's as_of: H_pend and H_act (the authority may extend the activation)")
	require.Equal(t, []bool{true}, acts(f.walk(t, p, true, 9_000), 9_000), "R4: activated at as_of fixes H_act from it")
	require.Equal(t, []bool{false}, acts(f.walk(t, p, false, 9_000), 8_000), "R4, R5: a status showing it pending fixes H_pend")

	w := f.walk(t, p, false, 20_000)
	w.steps = []chainStep{{act: true, res: Result{IssuedAtMs: 9_000}}}
	require.Equal(t, []bool{false, true}, acts(w, 6_000), "R3: before the next result, which matched H_act, both")
	require.Equal(t, []bool{true}, acts(w, 9_000), "R3: at the next result, which matched H_act, H_act only")
	w.steps[0].act = false
	require.Equal(t, []bool{false}, acts(w, 8_000), "R3: the next result matched H_pend: never activated before it")

	w = f.walk(t, p, true, 9_000)
	w.fix = &Status{Seq: p.Seq, HeadEntryDigest: p.Entry, AsOfMs: 7_000} // the retained status shows it activated at 7 000
	require.Equal(t, []bool{false, true}, acts(w, 6_000))
	require.Equal(t, []bool{true}, acts(w, 7_500), "R4: the from head's retained fix admits H_act only from its as_of")
	w.final, w.asOf = p, 8_000 // a status showing it pending after the fix showed it activated
	require.Empty(t, w.branches(p, 3, 7_500), "verdicts that contradict each other leave no hypothesis")
	w.actDone = 3
	require.Empty(t, w.branches(p, 3, 7_500), "nor does an activation the chain does not let the event read")
}

func TestVerifyDeviceLedgerBranchesSlot(t *testing.T) {
	f := newLedgerFixture(t)
	active := pub{Seq: 3, State: StateActive}
	nb := f.s.nb

	w := f.walk(t, active, false, 0)
	require.Len(t, w.branches(active, 3, nb-1), 1, "DR9 (c): the slot is pending before replacement_nb_ms")
	for _, x := range []uint64{nb, nb + tLedgerG, nb + 100*tLedgerG} {
		got := sets(t, w.branches(active, 3, x))
		require.Len(t, got, 2, "from replacement_nb_ms, pending or completed, with no upper bound (G-41): %d", x)
		require.Equal(t, f.rkey, got[0].ReplacementKey, "the slot still pending")
		require.Equal(t, f.rkey, got[1].RecoveryKey, "the refresh completed")
		require.Empty(t, got[1].ReplacementBindDigest)
	}

	p := pendingHead(t, OpReplaceRoot, nb) // a tie: the anb at replacement_nb_ms
	got := sets(t, f.walk(t, p, true, nb).branches(p, 3, nb))
	require.Len(t, got, 2, "C4-4: both orders")
	require.Equal(t, f.key, got[0].RecoveryKey, "the activation first burns the still-pending slot")
	require.Empty(t, got[0].ReplacementBindDigest)
	require.Equal(t, f.rkey, got[1].RecoveryKey, "A10: the completion first stays bound")
	require.Equal(t, []string{f.next.Dev()}, got[1].Active)

	pe := pub{Seq: 3, State: StatePending, ANB: nb - 1, Op: OpErase}
	bs := f.walk(t, pe, true, nb+1).branches(pe, 3, nb+1)
	require.Len(t, bs, 2)
	require.Equal(t, f.s.bytes(t), bs[0].w.set, "a Pe activation never burns the slot")
	require.Equal(t, f.rkey, decodeSet(t, bs[1].w.set).RecoveryKey, "a Pe activation never pre-empts a completion")
}

func TestVerifyDeviceLedgerOutcomes(t *testing.T) {
	f := newLedgerFixture(t)
	nb := f.s.nb

	p := pendingHead(t, OpReplaceRoot, nb-1)
	out := outcomes([]*ledgerWalk{f.walk(t, p, true, nb+1)}, p, 3, nb+1)
	require.Len(t, out, 2)
	require.Equal(t, f.s.bytes(t), out[0].persisted, "device-head #7: the activation came first, so nothing completed")
	got := decodeSet(t, out[0].shown)
	require.Equal(t, f.key, got.RecoveryKey, "the shown activation burns the slot")
	require.Equal(t, []string{f.next.Dev()}, got.Active)
	require.Equal(t, f.rkey, decodeSet(t, out[1].persisted).RecoveryKey, "the completion first is persisted")
	require.Equal(t, []string{f.self.Dev()}, decodeSet(t, out[1].persisted).Active, "and the activation is not")
	require.Equal(t, []string{f.next.Dev()}, decodeSet(t, out[1].shown).Active)

	out = outcomes([]*ledgerWalk{f.walk(t, p, false, nb+1)}, p, 3, nb+1)
	require.Len(t, out, 2, "the status shows it pending: the slot pending or completed, no activation")
	for _, o := range out {
		require.Equal(t, o.persisted, o.shown)
	}

	pe := pub{Seq: 3, State: StatePending, Prior: StateActive, ANB: 1, Op: OpErase}
	out = outcomes([]*ledgerWalk{f.walk(t, pe, true, nb-1)}, pe, 3, nb-1)
	require.Len(t, out, 1)
	require.Equal(t, f.s.bytes(t), out[0].persisted)
	require.Equal(t, out[0].persisted, out[0].shown, "a status showing a Pe activation shows the set as it is")
}

// resolve: the device-status against the live and refuted hypotheses (DR7,
// RT6-9, A11), and persistOne's refusal to pick between two device-heads.
func TestVerifyDeviceLedgerResolve(t *testing.T) {
	f := newLedgerFixture(t)
	active := pub{Seq: 3, State: StateActive}
	nb := f.s.nb
	completed := NormalizeDeviceSet(f.s.bytes(t), false, false, true, false, false)
	emptied := NormalizeDeviceSet(f.s.bytes(t), false, false, false, false, true)
	ds := func(set []byte) DeviceStatus {
		dig, err := DeviceSetDigest(set)
		require.NoError(t, err)
		return DeviceStatus{Dseq: 2, DeviceHeadDigest: digestOf("last"), DeviceSetDigest: dig}
	}
	mk := func(set []byte) *ledgerWalk {
		w := f.walk(t, active, false, 0)
		w.set, w.dseq, w.last = set, 2, digestOf("last")
		return w
	}
	l := &ledger{live: []*ledgerWalk{mk(f.s.bytes(t))}, refuted: []*ledgerWalk{mk(emptied)}}

	hits, r := l.resolve(ds(completed), active, 3, nb)
	require.Equal(t, ReasonOK, r, "the status shows the live hypothesis completed at as_of")
	require.Len(t, hits, 1)
	_, r = l.resolve(ds(completed), active, 3, nb-1)
	require.Equal(t, ReasonDeviceStatusMismatch, r, "completed before replacement_nb_ms matches no hypothesis")
	_, r = l.resolve(ds(emptied), active, 3, nb)
	require.Equal(t, ReasonDeviceRule, r, "A11: a status showing only a refuted hypothesis is the refusing entry's device_rule")
	behind := ds(f.s.bytes(t))
	behind.Dseq = 3
	_, r = l.resolve(behind, active, 3, nb)
	require.Equal(t, ReasonDeviceBehind, r)
	fork := ds(f.s.bytes(t))
	fork.DeviceHeadDigest = digestOf("other")
	_, r = l.resolve(fork, active, 3, nb)
	require.Equal(t, ReasonDeviceFork, r)

	p := pendingHead(t, OpReplaceRoot, 1)
	w := mk(f.s.bytes(t))
	// The status shows the head pending after the retained fix showed it activated.
	w.final, w.asOf = p, nb
	w.fix = &Status{Seq: p.Seq, HeadEntryDigest: p.Entry, AsOfMs: 2}
	_, r = (&ledger{live: []*ledgerWalk{w}}).resolve(ds(f.s.bytes(t)), p, 3, nb)
	require.Equal(t, ReasonHypothesisConflict, r, "R4: a status contradicting the retained fix")

	from := GenesisDeviceHead(f.s.realm, f.s.acct, "")
	dr := DeviceResult{RealmID: f.s.realm, AccountID: f.s.acct, RequestID: newID(), DeviceIntentDigest: digestOf("d"),
		Op: OpAddDevice, Dseq: 2, ChainSeq: 1, ChainEntryDigest: digestOf("e"), DeviceSetDigest: digestOf("s"),
		DelegationSerial: 1}.Encode()
	require.NotNil(t, dr)
	one, two := mk(nil), mk(nil)
	one.lastDR, two.lastDR = dr, dr
	same := []outcome{{w: one, persisted: completed, shown: completed}, {w: two, persisted: completed, shown: completed}}
	dh, shown, r := persistOne(same, from, from.Chain)
	require.Equal(t, ReasonOK, r)
	require.Equal(t, completed, shown)
	require.Equal(t, completed, dh.DeviceSet)
	split := []outcome{{w: one, persisted: completed, shown: completed}, {w: two, persisted: f.s.bytes(t), shown: completed}}
	_, _, r = persistOne(split, from, from.Chain)
	require.Equal(t, ReasonDeviceStatusMismatch, r, "two persisted heads for one status: fail closed, never pick one")
}

func TestVerifyDeviceLedgerKeepAndSharedReason(t *testing.T) {
	f := newLedgerFixture(t)
	many := make([]*ledgerWalk, 0, maxLedgerHypotheses+1)
	for i := range maxLedgerHypotheses + 1 {
		w := f.walk(t, pub{State: StateActive}, false, 0)
		w.actDone = uint64(i) // distinct hypotheses
		many = append(many, w)
	}
	l := &ledger{}
	require.Equal(t, ReasonOK, l.keep(many[:maxLedgerHypotheses], many))
	require.Len(t, l.refuted, maxLedgerHypotheses, "refuted hypotheses are trimmed, never refused")
	require.Equal(t, ReasonDeviceRule, l.keep(many, nil), "past the bound: refused, never trimmed")
	require.Equal(t, ReasonHypothesisConflict, l.keep(nil, many))
	require.Equal(t, ReasonOK, l.keep([]*ledgerWalk{many[0], many[0].clone()}, nil))
	require.Len(t, l.live, 1, "equal hypotheses are one")

	require.Equal(t, ReasonBadSignature, sharedReason([]Reason{ReasonBadSignature}), "one hypothesis: its own Reason")
	require.Equal(t, ReasonIssuedBounds, sharedReason([]Reason{ReasonIssuedBounds, ReasonIssuedBounds}))
	require.Equal(t, ReasonDeviceRule, sharedReason([]Reason{ReasonBadSignature, ReasonDeviceFork}),
		"DR9 (c): an entry that matches neither hypothesis")
}

func TestVerifyDeviceLedgerIssuedBounds(t *testing.T) {
	d := Delegation{GMs: tLedgerG}
	priv := func(op string, issued, asserted, expires uint64) parsedDevice {
		return parsedDevice{dr: DeviceResult{Op: op, IssuedAtMs: issued}, authz: &CPAuthz{AssertedAtMs: asserted},
			di: &DeviceIntent{ExpiresAtMs: expires}}
	}
	a := uint64(10_000_000)
	require.Equal(t, ReasonOK, priv(OpAddDevice, a-EpsilonMs, a, a+600_000).issuedBounds(d), "asserted − ε")
	require.Equal(t, ReasonIssuedBounds, priv(OpAddDevice, a-EpsilonMs-1, a, a+600_000).issuedBounds(d))
	require.Equal(t, ReasonIssuedBounds, priv(OpAddDevice, a+600_000, a, a+600_000).issuedBounds(d), "issued < expires")
	require.Equal(t, ReasonIssuedBounds, priv(OpBindRecovery, a+IntentWindowMs, a, a+IntentWindowMs+1).issuedBounds(d),
		"issued < asserted + INTENT_WINDOW_MS")
	require.Equal(t, ReasonOK, priv(OpCancelRecoveryBind, a+tLedgerG+CancelSlackMs-1, a, a+1).issuedBounds(d),
		"a cancel takes the cancel bounds")
	require.Equal(t, ReasonIssuedBounds, priv(OpCancelRecoveryBind, a+tLedgerG+CancelSlackMs, a, a+1).issuedBounds(d))
	require.Equal(t, ReasonIssuedBounds, priv(OpCancelRecoveryBind, a, a, a).issuedBounds(d), "a cancel needs expires > asserted")
	attached := parsedDevice{dr: DeviceResult{Op: OpRotateEK, IssuedAtMs: 1}}
	require.Equal(t, ReasonOK, attached.issuedBounds(d), "an attached entry's issued_at is its result's")
}

func TestDeviceViewQueries(t *testing.T) {
	a, b, c := testDev(t, 1), testDev(t, 2), testDev(t, 3)
	key := testPoint(t)
	s := setB{realm: newID(), acct: newID(), active: []DeviceEntry{a, b}, pending: &c, key: key, dig: digestOf("bound")}
	v := viewOf(s.bytes(t), 4, 99, []string{AdmittedRecord(a.ID, ""), AdmittedRecord(b.ID, a.ID), AdmittedRecord(c.ID, b.ID)})
	require.Equal(t, uint64(4), v.Dseq)
	require.Equal(t, uint64(99), v.AsOfMs)
	require.True(t, v.HasActiveDSK(Digest(a.DSK[:])))
	require.False(t, v.HasActiveDSK(Digest(c.DSK[:])), "a pending device is not active")
	require.Equal(t, []string{b.ID}, v.AdmittedBy(c.ID, false))
	require.Equal(t, []string{b.ID, a.ID}, v.AdmittedBy(c.ID, true), "RT4-3: to the device with no admitter")
	require.Empty(t, v.AdmittedBy(a.ID, true))
	require.Empty(t, v.AdmittedBy(newID(), true))
	cyc := viewOf(s.bytes(t), 4, 0, []string{AdmittedRecord(a.ID, b.ID), AdmittedRecord(b.ID, a.ID)})
	require.Equal(t, []string{b.ID}, cyc.AdmittedBy(a.ID, true), "a cycle ends the walk")
	got, dig, ok := v.BoundRecovery()
	require.True(t, ok)
	require.Equal(t, key, got[:])
	require.Equal(t, digestOf("bound"), dig)

	none := viewOf(nil, 0, 0, nil)
	_, _, ok = none.BoundRecovery()
	require.False(t, ok, "D-257: no bound key, no relay")
	require.False(t, none.HasActiveDSK(Digest(a.DSK[:])))

	gen := GenesisDeviceHead(newID(), newID(), "").View()
	require.Zero(t, gen.Dseq)
	require.Empty(t, gen.Active)
	dr := DeviceResult{RealmID: s.realm, AccountID: s.acct, RequestID: newID(), DeviceIntentDigest: digestOf("d"),
		Op: OpAddDevice, Dseq: 7, ChainSeq: 1, ChainEntryDigest: digestOf("e"), DeviceSetDigest: digestOf("s"),
		DelegationSerial: 1}.Encode()
	require.NotNil(t, dr)
	h := DeviceHead{RealmID: s.realm, AccountID: s.acct, LastDeviceResult: dr, DeviceSet: s.bytes(t),
		AdmittedBy: []string{AdmittedRecord(b.ID, a.ID)}}
	hv := h.View()
	require.Equal(t, uint64(7), hv.Dseq, "View reads the head dseq from last_device_result")
	require.Equal(t, []string{a.ID}, hv.AdmittedBy(b.ID, false))
	require.Len(t, hv.Active, 2)
	require.Zero(t, hv.AsOfMs)
}

// §2.5's identity rules for one entry, each broken alone: an entry whose parts
// disagree is device_rule whatever its signatures say.
func TestVerifyDeviceLedgerEntryConsistency(t *testing.T) {
	realm, acct, req := newID(), newID(), newID()
	base := func() parsedDevice {
		p := parsedDevice{raw: DeviceBundle{DeviceIntent: []byte("device-intent"), CPAuthz: []byte("cp-authz")}}
		p.di = &DeviceIntent{RealmID: realm, AccountID: acct, RequestID: req, Op: OpAddDevice, ExpectedSeq: 3}
		p.authz = &CPAuthz{RealmID: realm, AccountID: acct, RequestID: req}
		p.dr = DeviceResult{RealmID: realm, AccountID: acct, RequestID: req, Op: OpAddDevice, ChainSeq: 3,
			DeviceIntentDigest: Digest(p.raw.DeviceIntent), CPAuthzDigest: Digest(p.raw.CPAuthz)}
		return p
	}
	require.Equal(t, ReasonOK, base().consistent(nil))
	for name, mut := range map[string]func(*parsedDevice){
		"#6 is not D":                        func(p *parsedDevice) { p.dr.DeviceIntentDigest = digestOf("other") },
		"#5 is not the request":              func(p *parsedDevice) { p.dr.RequestID = newID() },
		"#8 is not the device-intent op":     func(p *parsedDevice) { p.dr.Op = OpBindRecovery },
		"#11 is not device-intent #9":        func(p *parsedDevice) { p.dr.ChainSeq = 2 },
		"#7 is not the cp-authz":             func(p *parsedDevice) { p.dr.CPAuthzDigest = digestOf("other") },
		"#7 without a cp-authz":              func(p *parsedDevice) { p.authz, p.raw.CPAuthz = nil, nil },
		"the cp-authz names another request": func(p *parsedDevice) { p.authz.RequestID = newID() },
		"no device-intent on a non-sheet op": func(p *parsedDevice) { p.di = nil },
		"B names another request":            func(p *parsedDevice) { p.bind = &RecoveryBind{RequestID: newID()} },
	} {
		p := base()
		mut(&p)
		require.Equal(t, ReasonDeviceRule, p.consistent(nil), name)
	}

	step := chainStep{res: Result{Op: OpRotateEK, IssuedAtMs: 7, DelegationSerial: 1},
		pb: parsedBundle{intent: Intent{RequestID: req, Op: OpRotateEK, ExpectedSeq: 3}}}
	attached := func() parsedDevice {
		p := base()
		p.dr.Op, p.di.Op, p.authz, p.raw.CPAuthz, p.dr.CPAuthzDigest = OpRotateEK, OpRotateEK, nil, nil, nil
		p.dr.IssuedAtMs, p.dr.DelegationSerial = 7, 1
		return p
	}
	require.Equal(t, ReasonOK, attached().consistent(&step))
	for name, mut := range map[string]func(*parsedDevice){
		"issued_at is not the result's":  func(p *parsedDevice) { p.dr.IssuedAtMs = 8 },
		"delegation is not the result's": func(p *parsedDevice) { p.dr.DelegationSerial = 2 },
		"#9 is not intent #8":            func(p *parsedDevice) { p.di.ExpectedSeq = 2 },
		"a cp-authz on an attached op": func(p *parsedDevice) {
			p.authz, p.raw.CPAuthz = &CPAuthz{RequestID: req}, []byte("cp-authz")
			p.dr.CPAuthzDigest = Digest(p.raw.CPAuthz)
		},
	} {
		p := attached()
		mut(&p)
		require.Equal(t, ReasonDeviceRule, p.consistent(&step), name)
	}
}

// carriedHead is a persisted device-head over set whose chain head is a raw
// pending result of op at seq 3 (an erase has no pending root: Pe), whose last
// entry is at chainSeq, and whose retained fix is what fix signs for the
// head's entry digest (nil: none).
func carriedHead(t *testing.T, f ledgerFixture, op string, set []byte, chainSeq uint64,
	fix func(entry []byte) []byte) (DeviceHead, pub) {
	res := Result{RealmID: f.s.realm, AccountID: f.s.acct, RequestID: newID(), IntentDigest: digestOf("i"),
		CPAuthzDigest: digestOf("c"), Op: op, Seq: 3, State: StatePending, PriorState: StateActive,
		SuspendedBy: []string{}, ActiveRoot: testPoint(t), EKBindingDigest: digestOf("e"), PrevEntryDigest: digestOf("p"),
		RecoveryGen: 1, ActivationNotBeforeMs: 5_000, IssuedAtMs: 1_000, DelegationSerial: 1}
	if op != OpErase {
		res.PendingRoot, res.PendingEKBindingDigest = testPoint(t), digestOf("pe")
	}
	rb := res.Encode()
	require.NotNil(t, rb)
	ch := ChainHead{RealmID: f.s.realm, AccountID: f.s.acct, HeadResult: rb, SeqIndex: make([]string, 3)}
	if fix != nil {
		ch.Fix = fix(EntryDigest(rb))
	}
	raw, r := headPub(ch)
	require.Equal(t, ReasonOK, r)
	dr := DeviceResult{RealmID: f.s.realm, AccountID: f.s.acct, RequestID: newID(), DeviceIntentDigest: digestOf("d"),
		Op: OpBindRecovery, Dseq: 4, ChainSeq: chainSeq, ChainEntryDigest: raw.Entry, DeviceSetDigest: digestOf("s"),
		DelegationSerial: 1}.Encode()
	require.NotNil(t, dr)
	return DeviceHead{RealmID: f.s.realm, AccountID: f.s.acct, DeviceSet: set, LastDeviceResult: dr, Chain: ch}, raw
}

// fixAt signs a fix for the head that shows it at asOf pending until anb
// (anb > asOf) or activated (anb 0).
func fixAt(t *testing.T, asOf, anb uint64) func(entry []byte) []byte {
	return func(entry []byte) []byte {
		return newTestRealm(t).status(t, func(s *Status) {
			s.Seq, s.HeadEntryDigest, s.State, s.ActivationNotBeforeMs = 3, entry, StateActive, anb
			if anb > asOf {
				s.State, s.PriorState = StatePending, StateActive
			}
			s.AsOfMs, s.ValidUntilMs = asOf, asOf+259_200_000
		})
	}
}

func carriedWalk(t *testing.T, from DeviceHead, final pub, asOf uint64) *ledgerWalk {
	w, r := newLedgerWalk(nil, HighWater{}, from, nil, final, asOf)
	require.Equal(t, ReasonOK, r)
	return w
}

// D-B1: a later call takes, and never applies again, a new-root activation of
// the from head that an entry of an earlier call took: the persisted set's
// pending device is gone (DR5, DR8). A set that stops before the activation
// (device-head #7) still holds the device, and the activation applies as before.
func TestVerifyDeviceLedgerCarriesAPersistedActivation(t *testing.T) {
	f := newLedgerFixture(t)
	// The Pr activation burned the old key; the owner then bound f.rkey at once.
	after := setB{realm: f.s.realm, acct: f.s.acct, active: []DeviceEntry{f.next}, key: f.rkey,
		dig: digestOf("bound-after")}.bytes(t)
	from, raw := carriedHead(t, f, OpRecover, after, 3, nil)
	w := carriedWalk(t, from, activate(raw), 9_000)
	require.Equal(t, uint64(3), w.actDone, "no pending device: the activation is in the persisted set")
	out := outcomes([]*ledgerWalk{w}, raw, 3, 9_000)
	require.Len(t, out, 1)
	require.Equal(t, after, out[0].shown, "never applied again: the key bound after it is not burned")
	require.Equal(t, after, out[0].persisted)

	from, raw = carriedHead(t, f, OpRecover, f.s.bytes(t), 3, nil)
	w = carriedWalk(t, from, activate(raw), 9_000)
	require.Zero(t, w.actDone, "the device still pending: no entry took the activation")
	out = outcomes([]*ledgerWalk{w}, raw, 3, 9_000)
	require.Len(t, out, 1)
	got := decodeSet(t, out[0].shown)
	require.Equal(t, []string{f.next.Dev()}, got.Active, "the activation applies, as in one call")
	require.Empty(t, got.RecoveryKey, "and burns the key bound before it")
	require.Equal(t, f.s.bytes(t), out[0].persisted, "device-head #7: persisted before it again")

	for name, c := range map[string]struct {
		op  string
		seq uint64
	}{
		"a last entry below the head's seq proves nothing": {OpRecover, 2},
		"a Pe head installs no root":                       {OpErase, 3},
	} {
		from, raw = carriedHead(t, f, c.op, after, c.seq, nil)
		require.Zero(t, carriedWalk(t, from, activate(raw), 9_000).actDone, name)
	}
}

// D-S1: the device-status co-signed with the from head's retained fix fixed the
// persisted slot pending at the fix's as_of (RT6-9) or, when the fix showed a
// new-root activation the persisted set stops before, that activation first,
// burning the slot (C4-4).
func TestVerifyDeviceLedgerCarriesTheSlotFloor(t *testing.T) {
	f := newLedgerFixture(t)
	nb, at := f.s.nb, uint64(30_000) // the fix's as_of, past replacement_nb_ms

	from, raw := carriedHead(t, f, OpReplaceRoot, f.s.bytes(t), 3, fixAt(t, at, 40_000))
	w := carriedWalk(t, from, raw, 35_000)
	require.Equal(t, slotFloor{digest: f.s.rdig, after: at}, w.slotFloor, "the fix shows the head pending")
	require.Equal(t, []bool{false}, w.completionChoices(nb))
	require.Equal(t, []bool{false}, w.completionChoices(at), "RT6-9: pending for every event at or before its as_of")
	require.Equal(t, []bool{false, true}, w.completionChoices(at+1), "after it, either, with no upper bound (G-41)")
	other := f.s
	other.rdig = digestOf("a later bind")
	w.set = other.bytes(t)
	require.Equal(t, []bool{false, true}, w.completionChoices(at), "a slot the fix's device-status did not show is not held")

	from, raw = carriedHead(t, f, OpReplaceRoot, f.s.bytes(t), 3, fixAt(t, at, 0))
	w = carriedWalk(t, from, activate(raw), at+tLedgerG)
	require.True(t, w.slotFloor.never, "the fix shows the activation the persisted set stops before")
	require.Equal(t, []bool{false}, w.completionChoices(at+100*tLedgerG), "C4-4: never before the activation that came first")
	out := outcomes([]*ledgerWalk{w}, raw, 3, at+tLedgerG)
	require.Len(t, out, 1, "only the activation first")
	got := decodeSet(t, out[0].shown)
	require.Equal(t, f.key, got.RecoveryKey, "the activation burned the slot and kept the bound key")
	require.Empty(t, got.ReplacementBindDigest)
	require.Equal(t, f.s.bytes(t), out[0].persisted)

	after := setB{realm: f.s.realm, acct: f.s.acct, active: []DeviceEntry{f.next}, key: f.key, dig: f.s.dig,
		rkey: f.rkey, rdig: f.s.rdig, nb: nb}.bytes(t) // a slot filled after an entry took the activation
	from, raw = carriedHead(t, f, OpReplaceRoot, after, 3, fixAt(t, at, 0))
	require.Equal(t, slotFloor{digest: f.s.rdig, after: at}, carriedWalk(t, from, activate(raw), at+1).slotFloor,
		"the activation taken: the device-status showed this set, so only RT6-9")

	for name, fix := range map[string]func([]byte) []byte{
		"no fix":                 nil,
		"a fix for another head": func([]byte) []byte { return fixAt(t, at, 0)(digestOf("another head")) },
	} {
		from, raw = carriedHead(t, f, OpReplaceRoot, f.s.bytes(t), 3, fix)
		require.Equal(t, slotFloor{}, carriedWalk(t, from, raw, at).slotFloor, name)
	}
}
