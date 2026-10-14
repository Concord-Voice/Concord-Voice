package authoritycontract

import "bytes"

// This file is A2's half of the §2.6 envelope rules and every position rule
// whose failure is §3.3's malformed_for_op (§2.5, §2.7's private part, §2.22).
// A1's codec checks each position's type and cap on its own; the rules here
// relate positions to each other, to the op, and to nested messages.
//
// Two return shapes, two meanings. A Check* function returns an error that
// wraps ErrMalformed: the envelope is not one the authority accepts at all
// (400 malformed). A *ForOp, *ForKind or *Head function returns a Refusal,
// "" when admitted: the request is well formed, and the authority records the
// refusal (200 refused). Neither shape ever admits on a decode error.

func nonEmpty(b []byte) bool { return len(b) > 0 }

// CheckRelay is §2.6's relay rule: a cancel_pending intent and exactly one
// canceller form, sig_current_root or (legacy_pop_sig and legacy_spki). A
// recovery-key cancel never travels here: relay has no sig_recovery position.
func CheckRelay(b []byte) (Relay, error) {
	r, err := DecodeRelay(b)
	if err != nil {
		return r, err
	}
	in, err := DecodeIntent(r.Intent)
	if err != nil {
		return r, err
	}
	root := nonEmpty(r.SigCurrentRoot)
	pop := nonEmpty(r.LegacyPopSig) && nonEmpty(r.LegacySPKI)
	half := nonEmpty(r.LegacyPopSig) != nonEmpty(r.LegacySPKI)
	if in.Op != OpCancelPending || half || root == pop {
		return r, malformed("relay canceller form")
	}
	return r, nil
}

// CheckRelayRecover is §2.6's relay-recover rule beyond the form pattern A1's
// decoder already enforces (exactly one of R, C, B by which positions are
// non-empty): form R's intent op is recover, form C's is cancel_pending, and
// form B's recovery_bind is a cancel B. It is a separate function from
// CheckRelay, never a branch of it, so the relay route cannot be handed a
// relay-recover body and read back a zero Relay with a nil error.
func CheckRelayRecover(b []byte) (RelayRecover, error) {
	r, err := DecodeRelayRecover(b)
	if err != nil {
		return r, err
	}
	if !nonEmpty(r.Intent) { // form B: the sheet cancel of a pending refresh or unbind
		rb, err := DecodeRecoveryBind(r.RecoveryBind)
		if err != nil {
			return r, err
		}
		if rb.Kind != BindKindCancel {
			return r, malformed("relay-recover form B kind")
		}
		return r, nil
	}
	in, err := DecodeIntent(r.Intent)
	if err != nil {
		return r, err
	}
	want := OpCancelPending // form C
	if nonEmpty(r.RecoveryBind) {
		want = OpRecover // form R: A1 admits a recovery_bind beside an intent only in form R
	}
	if in.Op != want {
		return r, malformed("relay-recover intent op")
	}
	return r, nil
}

// CheckQueryReq is §2.6's query-req rule against the head's pin: a query
// signed by the pin carries a 512-byte PSS signature and the legacy SPKI whose
// SHA-384 is the pin; any other signer carries a 96-byte signature and no SPKI.
func CheckQueryReq(b, pin []byte) (QueryReq, Query, error) {
	qr, err := DecodeQueryReq(b)
	if err != nil {
		return qr, Query{}, err
	}
	q, err := DecodeQuery(qr.Query)
	if err != nil {
		return qr, q, err
	}
	byPin := len(pin) > 0 && bytes.Equal(q.SignerDigest, pin)
	if byPin {
		if len(qr.QuerySig) != 512 || len(qr.LegacySPKI) == 0 || !bytes.Equal(Digest(qr.LegacySPKI), pin) {
			return qr, q, malformed("query-req pin signer")
		}
		return qr, q, nil
	}
	if len(qr.QuerySig) != 96 || len(qr.LegacySPKI) != 0 {
		return qr, q, malformed("query-req key signer")
	}
	return qr, q, nil
}

// CheckCommitted is §2.6's pairing of device_result and its signature: both
// present (an op with a device part) or both "".
func CheckCommitted(b []byte) (Committed, error) {
	c, err := DecodeCommitted(b)
	if err == nil && nonEmpty(c.DeviceResult) != nonEmpty(c.DeviceResultSig) {
		err = malformed("committed device pairing")
	}
	return c, err
}

func bundleActor(b []byte) (Bundle, CPAuthz, Result, error) {
	bd, err := DecodeBundle(b)
	if err != nil {
		return bd, CPAuthz{}, Result{}, err
	}
	a, err := DecodeCPAuthz(bd.CPAuthz)
	if err != nil {
		return bd, a, Result{}, err
	}
	r, err := DecodeResult(bd.Result)
	return bd, a, r, err
}

func deviceBundleResult(b []byte) (DeviceResult, error) {
	db, err := DecodeDeviceBundle(b)
	if err != nil {
		return DeviceResult{}, err
	}
	return DecodeDeviceResult(db.DeviceResult)
}

// CheckImportItem is §2.6's import-item rule (E3-02, V3-04, D-288). An item is
// either published (a bundle) or a private-op item (bundle "", its
// device_bundle an add_device, bind_recovery or cancel_recovery_bind entry),
// so at least one part is present. A published item carries its attached
// entry exactly when its op has a device part (intent #15 set), and an
// attached entry never travels without its bundle. cp_evidence is "" exactly
// on a restore-cancel, whose cp-authz actor is restore.
func CheckImportItem(b []byte) (ImportItem, error) {
	it, err := DecodeImportItem(b)
	if err != nil {
		return it, err
	}
	if !nonEmpty(it.Bundle) && !nonEmpty(it.DeviceBundle) {
		return it, malformed("import-item empty")
	}
	restore, err := importParts(it)
	if err != nil {
		return it, err
	}
	if restore == nonEmpty(it.CPEvidence) {
		return it, malformed("import-item cp_evidence")
	}
	return it, nil
}

// importParts checks an item's two parts against each other and reports
// whether its bundle is a restore-cancel.
func importParts(it ImportItem) (bool, error) {
	if nonEmpty(it.DeviceBundle) {
		dr, err := deviceBundleResult(it.DeviceBundle)
		if err != nil {
			return false, err
		}
		if attachedOp(dr.Op) != nonEmpty(it.Bundle) {
			return false, malformed("import-item private-op pattern")
		}
	}
	if !nonEmpty(it.Bundle) {
		return false, nil
	}
	bd, a, _, err := bundleActor(it.Bundle)
	if err != nil {
		return false, err
	}
	in, err := DecodeIntent(bd.Intent)
	if err != nil {
		return false, err
	}
	if nonEmpty(in.DeviceIntentDigest) != nonEmpty(it.DeviceBundle) {
		return false, malformed("import-item attached entry")
	}
	return a.Actor == ActorRestore, nil
}

type importKey struct {
	account        string
	chainSeq, dseq uint64
}

func (a importKey) less(b importKey) bool {
	if a.account != b.account {
		return a.account < b.account
	}
	if a.chainSeq != b.chainSeq {
		return a.chainSeq < b.chainSeq
	}
	return a.dseq < b.dseq
}

// itemKey is §2.6's (account_id, chain_seq, dseq): a published item's result
// seq and its attached entry's dseq (0 with none); a private-op item's
// device-result's.
func itemKey(it ImportItem) (importKey, error) {
	var k importKey
	if nonEmpty(it.DeviceBundle) {
		dr, err := deviceBundleResult(it.DeviceBundle)
		if err != nil {
			return k, err
		}
		k = importKey{dr.AccountID, dr.ChainSeq, dr.Dseq}
	}
	if nonEmpty(it.Bundle) {
		_, _, r, err := bundleActor(it.Bundle)
		if err != nil {
			return k, err
		}
		k.account, k.chainSeq = r.AccountID, r.Seq
	}
	return k, nil
}

// CheckImportBatch is §2.6's import-batch rule: every item passes
// CheckImportItem, in strictly ascending (account_id, chain_seq, dseq) order.
func CheckImportBatch(b []byte) (ImportBatch, error) {
	ib, err := DecodeImportBatch(b)
	if err != nil {
		return ib, err
	}
	var prev importKey
	for i, raw := range ib.Items {
		it, err := CheckImportItem(raw)
		if err != nil {
			return ib, err
		}
		k, err := itemKey(it)
		if err != nil {
			return ib, err
		}
		if i > 0 && !prev.less(k) {
			return ib, malformed("import-batch order")
		}
		prev = k
	}
	return ib, nil
}

// CheckHoldReq is §2.6's hold-req ordering: one bundle per account, in
// strictly ascending account_id order. Whether each bundle verifies is
// VerifyBundle's (RT6-7, D-287).
func CheckHoldReq(b []byte) (HoldReq, error) {
	h, err := DecodeHoldReq(b)
	if err != nil {
		return h, err
	}
	prev := ""
	for i, raw := range h.Bundles {
		_, _, r, err := bundleActor(raw)
		if err != nil {
			return h, err
		}
		if i > 0 && r.AccountID <= prev {
			return h, malformed("hold-req order")
		}
		prev = r.AccountID
	}
	return h, nil
}

// CheckDeviceSubmit is §2.6's device-submit rule: the non-empty positions
// follow §2.5's per-op private part for add_device, bind_recovery (bind:
// sig_recovery; unbind: none; sig_recovery_prior optional on both) or
// cancel_recovery_bind (the root form with a device-intent, the sheet form
// with none), and B.request_id is device-intent #5 for bind, unbind and the
// root-form cancel (§2.22 #5, E3-10), and on the root-form cancel B's
// expires_at_ms is device-intent #8 (§2.22 #7, E3-10, V3-06).
// Any other pattern is 400 malformed. B.kind beyond what selects the pattern
// is DR9's (wrong_state), and B's positions are RecoveryBindForKind's.
func CheckDeviceSubmit(b []byte) (DeviceSubmit, error) {
	d, err := DecodeDeviceSubmit(b)
	if err != nil {
		return d, err
	}
	if !submitPatternHolds(d) {
		return d, malformed("device-submit pattern")
	}
	return d, nil
}

// submitShape is which signature and bind positions a device-submit fills;
// sig_recovery_prior is judged apart, because the bind forms may omit it.
type submitShape struct{ root, actor, newDevice, bind, recovery bool }

func shapeOf(d DeviceSubmit) submitShape {
	return submitShape{root: nonEmpty(d.SigRoot), actor: nonEmpty(d.SigActor), newDevice: nonEmpty(d.SigNewDevice),
		bind: nonEmpty(d.RecoveryBind), recovery: nonEmpty(d.SigRecovery)}
}

func submitPatternHolds(d DeviceSubmit) bool {
	got, prior := shapeOf(d), nonEmpty(d.SigRecoveryPrior)
	if !nonEmpty(d.DeviceIntent) { // the sheet form of cancel_recovery_bind
		rb, err := DecodeRecoveryBind(d.RecoveryBind)
		return err == nil && rb.Kind == BindKindCancel && !prior && got == submitShape{bind: true, recovery: true}
	}
	di, err := DecodeDeviceIntent(d.DeviceIntent)
	if err != nil {
		return false
	}
	switch di.Op {
	case OpAddDevice:
		return !prior && got == submitShape{root: true, actor: true, newDevice: true}
	case OpBindRecovery: // B.request_id = device-intent #5 for bind and unbind (DoR §2.22 #5, E3-10)
		rb, err := DecodeRecoveryBind(d.RecoveryBind)
		return err == nil && rb.RequestID == di.RequestID &&
			got == submitShape{root: true, actor: true, bind: true, recovery: rb.Kind == BindKindBind}
	case OpCancelRecoveryBind: // the root form: B #5 and #7 are device-intent #5 and #8 (DoR §2.22, E3-10, V3-06)
		rb, err := DecodeRecoveryBind(d.RecoveryBind)
		return err == nil && !prior && rb.RequestID == di.RequestID && rb.ExpiresAtMs == di.ExpiresAtMs &&
			got == submitShape{root: true, actor: true, bind: true}
	}
	return false
}

// devicePart is one row of §2.5's per-op private table: which device-intent
// positions the op uses. A position the op does not use MUST be "" ([] at
// #17, 0 at #8 and #11), otherwise malformed_for_op (V-18).
type devicePart struct {
	newDevice bool // #14–#16 all set; otherwise all ""
	actor     bool // #13 set, #11 ≥ 1 and #12 set; otherwise "", 0 and "" (V2-01)
	removed   bool // #17 may hold [0..9]; otherwise []
	bind      bool // #18 set; otherwise ""
	bindWait  bool // #19 is DR9's per-form rule; otherwise ""
	private   bool // a private op: #8 > 0 (as intent #7). Attached: #8 = 0
}

// devicePartOf is §2.5's per-op private table (DoR:427–437). cancel_recovery_bind
// is its root form: the sheet form carries no device-intent (LD-43).
func devicePartOf(op string) (devicePart, bool) {
	switch op {
	case OpEnrollNew, OpEnrollExisting, OpReplaceRoot, OpRecover:
		return devicePart{newDevice: true}, true
	case OpRotateRoot, OpRotateEK:
		return devicePart{actor: true, removed: true}, true
	case OpUnsuspend: // the user form; the operator form has no device-intent
		return devicePart{actor: true}, true
	case OpAddDevice:
		return devicePart{newDevice: true, actor: true, private: true}, true
	case OpBindRecovery:
		return devicePart{actor: true, bind: true, bindWait: true, private: true}, true
	case OpCancelRecoveryBind:
		return devicePart{actor: true, bind: true, private: true}, true
	}
	return devicePart{}, false
}

// attachedOp reports a device-intent op whose device part is attached to a
// published intent (§2.5): every device-intent op but the three private ones.
func attachedOp(op string) bool {
	p, ok := devicePartOf(op)
	return ok && !p.private
}

// actorOpName reports an op with an actor device: device-intent #13 required.
func actorOpName(op string) bool {
	p, ok := devicePartOf(op)
	return ok && p.actor
}

// DeviceIntentForOp is §2.5's per-op private table plus §3.2.4 step 4's
// pairwise-distinct keys: "" or malformed_for_op. candidateRoot is the
// published intent's #12 for an attached op and nil for a private one. Two
// device-intent rules are deliberately not here: #14 = DeviceID(#15) is DR1
// (device_unknown, ApplyDeviceOp), and #19's value per form is DR9.
func DeviceIntentForOp(di DeviceIntent, candidateRoot []byte) Refusal {
	p, ok := devicePartOf(di.Op)
	if !ok || !p.positionsHold(di) || !p.keysDistinct(di, candidateRoot) {
		return RefusalMalformedForOp
	}
	return ""
}

func (p devicePart) positionsHold(di DeviceIntent) bool {
	newAny := di.NewDeviceID != "" || nonEmpty(di.NewDeviceDSK) || nonEmpty(di.NewDeviceDKX)
	newAll := di.NewDeviceID != "" && nonEmpty(di.NewDeviceDSK) && nonEmpty(di.NewDeviceDKX)
	return newAll == p.newDevice && newAny == newAll &&
		(di.ActorDeviceID != "") == p.actor &&
		(di.ExpectedDseq >= 1) == p.actor &&
		(di.ExpectedDseq == 0) == !nonEmpty(di.ExpectedDeviceHead) &&
		(len(di.RemovedDeviceIDs) == 0 || p.removed) &&
		nonEmpty(di.RecoveryBindDigest) == p.bind &&
		(di.BindWait == "" || p.bindWait) &&
		(di.ExpiresAtMs != 0) == p.private
}

// keysDistinct is §3.2.4 step 4 (§2.4 key separation): the new dsk, the new
// dkx and the candidate root pairwise distinct. Every attached new-device op
// names a candidate root (§2.7), so a missing one fails closed rather than
// skipping the comparison; add_device has none.
func (p devicePart) keysDistinct(di DeviceIntent, candidateRoot []byte) bool {
	if !p.newDevice {
		return true
	}
	if bytes.Equal(di.NewDeviceDSK, di.NewDeviceDKX) {
		return false
	}
	if nonEmpty(candidateRoot) {
		return !bytes.Equal(di.NewDeviceDSK, candidateRoot) && !bytes.Equal(di.NewDeviceDKX, candidateRoot)
	}
	return p.private
}

// RecoveryBindForKind is §2.22's per-kind position rule (V2-04, LD-49, LD-53):
// "" or malformed_for_op at the authority; a published bundle's B that fails
// it is malformed (A5). bind: #7 = 0, #8 set, #9 "", #10 ≥ 1, #11–#13 set.
// unbind: #7 = 0, #8 "", #9 "", #10 ≥ 1, #11 and #12 set, #13 "". cancel:
// #7 > 0, #8 set, #9 any, #10 = 0, #11–#13 "".
func RecoveryBindForKind(b RecoveryBind) Refusal {
	var ok bool
	switch b.Kind {
	case BindKindBind:
		ok = b.ExpiresAtMs == 0 && nonEmpty(b.RecoveryKey) && !nonEmpty(b.TargetDigest) && b.ChainSeq >= 1 &&
			nonEmpty(b.ChainEntryDigest) && nonEmpty(b.BindingRootDigest) && nonEmpty(b.EscrowPublicKey)
	case BindKindUnbind:
		ok = b.ExpiresAtMs == 0 && !nonEmpty(b.RecoveryKey) && !nonEmpty(b.TargetDigest) && b.ChainSeq >= 1 &&
			nonEmpty(b.ChainEntryDigest) && nonEmpty(b.BindingRootDigest) && !nonEmpty(b.EscrowPublicKey)
	case BindKindCancel:
		ok = b.ExpiresAtMs > 0 && nonEmpty(b.RecoveryKey) && b.ChainSeq == 0 && !nonEmpty(b.ChainEntryDigest) &&
			!nonEmpty(b.BindingRootDigest) && !nonEmpty(b.EscrowPublicKey)
	}
	if !ok {
		return RefusalMalformedForOp
	}
	return ""
}

// CheckDeviceIntentHead is device-intent #9/#10 against the effective
// published head (E3-10, V3-06): stale_seq, then stale_head. At seq 0 both the
// head's entry digest and #10 are "" (A4).
func CheckDeviceIntentHead(di DeviceIntent, h HeadInput) Refusal {
	if di.ExpectedSeq != h.Seq {
		return RefusalStaleSeq
	}
	if !bytes.Equal(di.ExpectedHeadDigest, h.EntryDigest) {
		return RefusalStaleHead
	}
	return ""
}
