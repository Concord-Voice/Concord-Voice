package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func cancelIntent(t *testing.T) []byte {
	in := sampleIntent(t)
	in.Op, in.CandidateRoot, in.EKBindingDigest, in.DeviceIntentDigest, in.RecoveryBindDigest = OpCancelPending, nil, nil, nil, nil
	b := in.Encode()
	require.NotNil(t, b)
	return b
}

func TestCheckRelay(t *testing.T) {
	ci := cancelIntent(t)
	_, err := CheckRelay(Relay{Intent: ci, SigCurrentRoot: rnd(96)}.Encode())
	require.NoError(t, err)
	_, err = CheckRelay(Relay{Intent: ci, LegacyPopSig: rnd(512), LegacySPKI: testEKSPKI(t)}.Encode())
	require.NoError(t, err)
	for name, r := range map[string]Relay{
		"two cancellers":   {Intent: ci, SigCurrentRoot: rnd(96), LegacyPopSig: rnd(512), LegacySPKI: testEKSPKI(t)},
		"none":             {Intent: ci},
		"PoP without spki": {Intent: ci, LegacyPopSig: rnd(512)},
		"not a cancel":     {Intent: sampleIntent(t).Encode(), SigCurrentRoot: rnd(96)},
	} {
		_, err := CheckRelay(r.Encode())
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

// DoR §2.6 relay: exactly one canceller form. A sig_current_root beside half
// of the PoP pair is malformed, not a root cancel with stray bytes. The codec
// admits either half alone (TestCheckRelay's "PoP without spki" encodes), so
// only CheckRelay's own guard refuses it.
func TestCheckRelayRefusesARootCancellerBesideAHalfPoPPair(t *testing.T) {
	ci := cancelIntent(t)
	_, err := CheckRelay(Relay{Intent: ci, SigCurrentRoot: rnd(96)}.Encode())
	require.NoError(t, err, "control: the root form alone")
	for name, r := range map[string]Relay{
		"root and a lone legacy_pop_sig": {Intent: ci, SigCurrentRoot: rnd(96), LegacyPopSig: rnd(512)},
		"root and a lone legacy_spki":    {Intent: ci, SigCurrentRoot: rnd(96), LegacySPKI: testEKSPKI(t)},
	} {
		b := r.Encode()
		require.NotNil(t, b, name)
		_, err := CheckRelay(b)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

func TestCheckQueryReq(t *testing.T) {
	spki := testEKSPKI(t)
	pin := Digest(spki)
	q := Query{RealmID: newID(), AccountID: newID(), SignerDigest: digestOf("root"), IssuedAtMs: 1, Nonce: rnd(16)}
	_, _, err := CheckQueryReq(QueryReq{Query: q.Encode(), QuerySig: rnd(96)}.Encode(), pin)
	require.NoError(t, err)
	pq := q
	pq.SignerDigest = pin
	_, _, err = CheckQueryReq(QueryReq{Query: pq.Encode(), QuerySig: rnd(512), LegacySPKI: spki}.Encode(), pin)
	require.NoError(t, err)
	for name, qr := range map[string]QueryReq{
		"pin signer with a 96-byte signature": {Query: pq.Encode(), QuerySig: rnd(96), LegacySPKI: spki},
		"pin signer without spki":             {Query: pq.Encode(), QuerySig: rnd(512)},
		"root signer with a 512-byte sig":     {Query: q.Encode(), QuerySig: rnd(512)},
		"root signer with spki":               {Query: q.Encode(), QuerySig: rnd(96), LegacySPKI: spki},
	} {
		_, _, err := CheckQueryReq(qr.Encode(), pin)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

func TestCheckCommitted(t *testing.T) {
	res := sampleResult(t).Encode()
	_, err := CheckCommitted(Committed{Result: res, ResultSig: rnd(96)}.Encode())
	require.NoError(t, err)
	_, err = CheckCommitted(Committed{Result: res, ResultSig: rnd(96), DeviceResult: rnd(100)}.Encode())
	require.ErrorIs(t, err, ErrMalformed, "device_result without its signature")
}

func TestRecoveryBindForKind(t *testing.T) {
	key, escrow := testPoint(t), testPoint(t)
	bind := RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindBind, RecoveryKey: key,
		ChainSeq: 3, ChainEntryDigest: digestOf("e"), BindingRootDigest: digestOf("r"), EscrowPublicKey: escrow}
	require.Equal(t, Refusal(""), RecoveryBindForKind(bind))
	noEscrow := bind
	noEscrow.EscrowPublicKey = nil
	require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(noEscrow), "LD-53: a bind B carries the escrow key")
	unbind := bind
	unbind.Kind, unbind.RecoveryKey, unbind.EscrowPublicKey = BindKindUnbind, nil, nil
	require.Equal(t, Refusal(""), RecoveryBindForKind(unbind))
	unbindKey := unbind
	unbindKey.RecoveryKey = key
	require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(unbindKey), "an unbind B with a recovery_key")
	unbindEscrow := unbind
	unbindEscrow.EscrowPublicKey = escrow
	require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(unbindEscrow))
	emptyBind := bind
	emptyBind.RecoveryKey = nil
	require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(emptyBind), "a bind B with an empty recovery_key")
	cancel := RecoveryBind{RealmID: bind.RealmID, AccountID: bind.AccountID, RequestID: newID(), Kind: BindKindCancel,
		ExpiresAtMs: 5, RecoveryKey: key}
	require.Equal(t, Refusal(""), RecoveryBindForKind(cancel))
	cancelEscrow := cancel
	cancelEscrow.EscrowPublicKey = escrow
	require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(cancelEscrow))
	cancelSeq := cancel
	cancelSeq.ChainSeq = 1
	require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(cancelSeq))
}

// TestRecoveryBindForKindEveryPosition flips each §2.22 position one at a
// time, so every clause of every kind has a single-fault negative.
func TestRecoveryBindForKindEveryPosition(t *testing.T) {
	key, escrow := testPoint(t), testPoint(t)
	bind := RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindBind, RecoveryKey: key,
		ChainSeq: 3, ChainEntryDigest: digestOf("e"), BindingRootDigest: digestOf("r"), EscrowPublicKey: escrow}
	unbind := bind
	unbind.Kind, unbind.RecoveryKey, unbind.EscrowPublicKey = BindKindUnbind, nil, nil
	cancel := RecoveryBind{RealmID: bind.RealmID, AccountID: bind.AccountID, RequestID: newID(), Kind: BindKindCancel,
		ExpiresAtMs: 5, RecoveryKey: key}
	targeted := cancel
	targeted.TargetDigest = digestOf("slot")
	require.Equal(t, Refusal(""), RecoveryBindForKind(targeted), "cancel: #9 a digest or \"\" (LD-45)")

	flips := map[string]func(*RecoveryBind){
		"#7 non-zero":    func(b *RecoveryBind) { b.ExpiresAtMs = 9 },
		"#9 set":         func(b *RecoveryBind) { b.TargetDigest = digestOf("t") },
		"#10 zero":       func(b *RecoveryBind) { b.ChainSeq = 0 },
		"#11 empty":      func(b *RecoveryBind) { b.ChainEntryDigest = nil },
		"#12 empty":      func(b *RecoveryBind) { b.BindingRootDigest = nil },
		"#13 flipped":    func(b *RecoveryBind) { b.EscrowPublicKey = flipPoint(b.EscrowPublicKey, escrow) },
		"#8 flipped":     func(b *RecoveryBind) { b.RecoveryKey = flipPoint(b.RecoveryKey, key) },
		"unknown kind":   func(b *RecoveryBind) { b.Kind = "rebind" },
		"kind is cancel": func(b *RecoveryBind) { b.Kind = BindKindCancel },
	}
	for _, base := range []RecoveryBind{bind, unbind} {
		for name, flip := range flips {
			b := base
			flip(&b)
			require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(b), "%s: %s", base.Kind, name)
		}
	}
	cancelFlips := map[string]func(*RecoveryBind){
		"#7 zero":      func(b *RecoveryBind) { b.ExpiresAtMs = 0 },
		"#8 empty":     func(b *RecoveryBind) { b.RecoveryKey = nil },
		"#10 non-zero": func(b *RecoveryBind) { b.ChainSeq = 2 },
		"#11 set":      func(b *RecoveryBind) { b.ChainEntryDigest = digestOf("e") },
		"#12 set":      func(b *RecoveryBind) { b.BindingRootDigest = digestOf("r") },
		"#13 set":      func(b *RecoveryBind) { b.EscrowPublicKey = escrow },
		"kind is bind": func(b *RecoveryBind) { b.Kind = BindKindBind },
		"kind unbind":  func(b *RecoveryBind) { b.Kind = BindKindUnbind },
		"empty kind":   func(b *RecoveryBind) { b.Kind = "" },
	}
	for name, flip := range cancelFlips {
		b := cancel
		flip(&b)
		require.Equal(t, RefusalMalformedForOp, RecoveryBindForKind(b), "cancel: %s", name)
	}
}

// flipPoint empties a set point and sets an empty one.
func flipPoint(cur, set []byte) []byte {
	if len(cur) > 0 {
		return nil
	}
	return set
}

func devIntent(op string) DeviceIntent {
	return DeviceIntent{RealmID: newID(), AccountID: newID(), RequestID: newID(), Op: op, Salt: rnd(32),
		RemovedDeviceIDs: []string{}}
}

func TestDeviceIntentForOp(t *testing.T) {
	nd := testDev(t, 0)
	newDev := func(di *DeviceIntent) {
		di.NewDeviceID, di.NewDeviceDSK, di.NewDeviceDKX = nd.ID, nd.DSK[:], nd.DKX[:]
	}
	actor := func(di *DeviceIntent) {
		di.ExpectedDseq, di.ExpectedDeviceHead, di.ActorDeviceID = 2, digestOf("dh"), newID()
	}
	enroll := devIntent(OpEnrollNew)
	newDev(&enroll)
	require.Equal(t, Refusal(""), DeviceIntentForOp(enroll, testPoint(t)))
	e2 := enroll
	e2.ExpectedDseq = 3
	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(e2, nil), "V2-01: a non-zero #11 with no actor device")
	e3 := enroll
	e3.NewDeviceDKX = e3.NewDeviceDSK
	// A distinct candidate root, so only the dsk = dkx comparison can refuse:
	// with nil the missing-root branch refuses first.
	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(e3, testPoint(t)), "dsk = dkx")
	e4 := enroll
	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(e4, enroll.NewDeviceDSK), "dsk = the candidate root")
	rot := devIntent(OpRotateEK)
	actor(&rot)
	rot.RemovedDeviceIDs = []string{newID()}
	require.Equal(t, Refusal(""), DeviceIntentForOp(rot, nil))
	uns := devIntent(OpUnsuspend)
	actor(&uns)
	uns.RemovedDeviceIDs = []string{newID()}
	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(uns, nil), "a non-empty #17 an op does not use")
	add := devIntent(OpAddDevice)
	actor(&add)
	newDev(&add)
	add.ExpiresAtMs = 9
	require.Equal(t, Refusal(""), DeviceIntentForOp(add, nil))
	add.ExpiresAtMs = 0
	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(add, nil), "a private op carries its expiry")
	cancel := devIntent(OpCancelRecoveryBind)
	actor(&cancel)
	cancel.ExpiresAtMs, cancel.RecoveryBindDigest = 9, digestOf("B")
	require.Equal(t, Refusal(""), DeviceIntentForOp(cancel, nil))
	cancel.BindWait = BindWaitNow
	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(cancel, nil), "a root-form cancel with a non-empty bind_wait")
	bind := devIntent(OpBindRecovery)
	actor(&bind)
	bind.ExpiresAtMs, bind.RecoveryBindDigest, bind.BindWait = 9, digestOf("B"), BindWaitDelta
	require.Equal(t, Refusal(""), DeviceIntentForOp(bind, nil), "#19 is DR9's per-form rule, not this table's")
}

// TestDeviceIntentForOpSingleFaults starts from an admitted device-intent for
// every op in §2.5's per-op private table and breaks exactly one position.
func TestDeviceIntentForOpSingleFaults(t *testing.T) {
	nd := testDev(t, 0)
	cand := testPoint(t)
	withNew := func(di DeviceIntent) DeviceIntent {
		di.NewDeviceID, di.NewDeviceDSK, di.NewDeviceDKX = nd.ID, nd.DSK[:], nd.DKX[:]
		return di
	}
	withActor := func(di DeviceIntent) DeviceIntent {
		di.ExpectedDseq, di.ExpectedDeviceHead, di.ActorDeviceID = 2, digestOf("dh"), newID()
		return di
	}
	private := func(di DeviceIntent) DeviceIntent { di.ExpiresAtMs = 9; return di }
	withBind := func(di DeviceIntent) DeviceIntent { di.RecoveryBindDigest = digestOf("B"); return di }
	good := map[string]DeviceIntent{}
	for _, op := range []string{OpEnrollNew, OpEnrollExisting, OpReplaceRoot, OpRecover} {
		good[op] = withNew(devIntent(op))
	}
	for _, op := range []string{OpRotateRoot, OpRotateEK} {
		r := withActor(devIntent(op))
		r.RemovedDeviceIDs = []string{newID(), newID()}
		good[op] = r
	}
	good[OpUnsuspend] = withActor(devIntent(OpUnsuspend))
	good[OpAddDevice] = private(withNew(withActor(devIntent(OpAddDevice))))
	good[OpBindRecovery] = private(withBind(withActor(devIntent(OpBindRecovery))))
	good[OpCancelRecoveryBind] = private(withBind(withActor(devIntent(OpCancelRecoveryBind))))
	require.Len(t, good, len(deviceOps), "every device-intent op (§2.5 #6) has a row")
	for op, di := range good {
		require.Equal(t, Refusal(""), DeviceIntentForOp(di, cand), op)
	}

	single := func(op, name string, f func(*DeviceIntent)) {
		di := good[op]
		di.RemovedDeviceIDs = append([]string{}, di.RemovedDeviceIDs...)
		f(&di)
		require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(di, cand), "%s: %s", op, name)
	}
	single(OpEnrollNew, "V2-01: #11 non-zero with a matching #12", func(di *DeviceIntent) {
		di.ExpectedDseq, di.ExpectedDeviceHead = 3, digestOf("dh")
	})
	single(OpRecover, "#12 without #11", func(di *DeviceIntent) { di.ExpectedDeviceHead = digestOf("dh") })
	single(OpReplaceRoot, "#13 on an op with no actor device", func(di *DeviceIntent) { di.ActorDeviceID = newID() })
	single(OpEnrollExisting, "#14 alone", func(di *DeviceIntent) { di.NewDeviceDSK, di.NewDeviceDKX = nil, nil })
	single(OpEnrollNew, "#16 missing", func(di *DeviceIntent) { di.NewDeviceDKX = nil })
	single(OpEnrollNew, "#15 missing", func(di *DeviceIntent) { di.NewDeviceDSK = nil })
	single(OpEnrollNew, "#14 missing", func(di *DeviceIntent) { di.NewDeviceID = "" })
	single(OpEnrollNew, "dkx = the candidate root", func(di *DeviceIntent) { di.NewDeviceDKX = cand })
	single(OpEnrollNew, "attached with an expiry", func(di *DeviceIntent) { di.ExpiresAtMs = 9 })
	single(OpRecover, "#18 on an op that binds nothing", func(di *DeviceIntent) { di.RecoveryBindDigest = digestOf("B") })
	single(OpEnrollNew, "#19 on an op that binds nothing", func(di *DeviceIntent) { di.BindWait = BindWaitNow })
	single(OpRotateRoot, "a new device on rotate_root", func(di *DeviceIntent) {
		di.NewDeviceID, di.NewDeviceDSK, di.NewDeviceDKX = nd.ID, nd.DSK[:], nd.DKX[:]
	})
	// #14–#16 are all-or-none even on an op that takes no new device: a lone
	// part fails the all-or-none rule, not the per-op one.
	single(OpRotateRoot, "#14 alone on rotate_root", func(di *DeviceIntent) { di.NewDeviceID = nd.ID })
	single(OpRotateRoot, "#15 alone on rotate_root", func(di *DeviceIntent) { di.NewDeviceDSK = nd.DSK[:] })
	single(OpRotateEK, "#11 zero on an actor op", func(di *DeviceIntent) { di.ExpectedDseq, di.ExpectedDeviceHead = 0, nil })
	single(OpRotateEK, "#12 empty on an actor op", func(di *DeviceIntent) { di.ExpectedDeviceHead = nil })
	single(OpUnsuspend, "#13 empty on an actor op", func(di *DeviceIntent) { di.ActorDeviceID = "" })
	single(OpAddDevice, "#17 non-empty on add_device", func(di *DeviceIntent) { di.RemovedDeviceIDs = []string{newID()} })
	single(OpAddDevice, "no new device on add_device", func(di *DeviceIntent) {
		di.NewDeviceID, di.NewDeviceDSK, di.NewDeviceDKX = "", nil, nil
	})
	single(OpAddDevice, "#18 on add_device", func(di *DeviceIntent) { di.RecoveryBindDigest = digestOf("B") })
	single(OpAddDevice, "#19 on add_device", func(di *DeviceIntent) { di.BindWait = BindWaitDelta })
	single(OpBindRecovery, "#18 missing", func(di *DeviceIntent) { di.RecoveryBindDigest = nil })
	single(OpBindRecovery, "no expiry on a private op", func(di *DeviceIntent) { di.ExpiresAtMs = 0 })
	single(OpBindRecovery, "a new device on bind_recovery", func(di *DeviceIntent) {
		di.NewDeviceID, di.NewDeviceDSK, di.NewDeviceDKX = nd.ID, nd.DSK[:], nd.DKX[:]
	})
	single(OpCancelRecoveryBind, "#18 missing", func(di *DeviceIntent) { di.RecoveryBindDigest = nil })
	single(OpCancelRecoveryBind, "#19 on the root-form cancel", func(di *DeviceIntent) { di.BindWait = BindWaitReplace })

	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(good[OpRecover], nil),
		"an attached new-device op without its candidate root cannot be checked pairwise distinct")
	require.Equal(t, Refusal(""), DeviceIntentForOp(good[OpAddDevice], nil), "add_device has no candidate root")
	unknown := good[OpEnrollNew]
	unknown.Op = OpSuspend
	require.Equal(t, RefusalMalformedForOp, DeviceIntentForOp(unknown, cand), "an op with no private part")
}

func TestCheckDeviceIntentHead(t *testing.T) {
	di := DeviceIntent{ExpectedSeq: 4, ExpectedHeadDigest: digestOf("e4")}
	require.Equal(t, Refusal(""), CheckDeviceIntentHead(di, HeadInput{Seq: 4, EntryDigest: digestOf("e4")}))
	require.Equal(t, RefusalStaleSeq, CheckDeviceIntentHead(di, HeadInput{Seq: 5, EntryDigest: digestOf("e4")}))
	require.Equal(t, RefusalStaleHead, CheckDeviceIntentHead(di, HeadInput{Seq: 4, EntryDigest: digestOf("e5")}))
	require.Equal(t, Refusal(""), CheckDeviceIntentHead(DeviceIntent{}, HeadInput{}), "A4: seq 0 and its \"\" digest")
}

func TestPatternOpSets(t *testing.T) {
	ops := map[string]bool{"": true}
	for _, op := range append(append([]string{}, deviceOps...), publishedOps...) {
		ops[op] = true
	}
	var attached, actors []string
	for op := range ops {
		if attachedOp(op) {
			attached = append(attached, op)
		}
		if actorOpName(op) {
			actors = append(actors, op)
		}
	}
	require.ElementsMatch(t, []string{OpEnrollNew, OpEnrollExisting, OpReplaceRoot, OpRecover, OpRotateRoot, OpRotateEK,
		OpUnsuspend}, attached, "§2.5: the seven ops whose device part is attached to a published intent")
	require.ElementsMatch(t, []string{OpAddDevice, OpBindRecovery, OpCancelRecoveryBind, OpRotateRoot, OpRotateEK,
		OpUnsuspend}, actors, "§2.5 #13: the ops with an actor device")
}

// The fixtures below build real nested messages: the pattern rules decode
// what they order and pair, so random bytes would only test the decoder.

func patAuthz(t *testing.T, account, actor string) []byte {
	b := CPAuthz{RealmID: newID(), AccountID: account, RequestID: newID(), IntentDigest: digestOf("i"), Actor: actor,
		AssertedAtMs: 1, EvidenceDigest: digestOf("ev")}.Encode()
	require.NotNil(t, b)
	return b
}

// patBundle is a published bundle for account at seq; device sets intent #15.
func patBundle(t *testing.T, account string, seq uint64, actor string, device bool) []byte {
	in := sampleIntent(t)
	in.AccountID = account
	if !device {
		in.Op, in.CandidateRoot, in.EKBindingDigest, in.DeviceIntentDigest = OpSuspend, nil, nil, nil
	}
	res := sampleResult(t)
	res.AccountID, res.Seq = account, seq
	bd := Bundle{Intent: in.Encode(), CPAuthz: patAuthz(t, account, actor), CPAuthzSig: rnd(96), Result: res.Encode(),
		ResultSig: rnd(96)}
	if actor == ActorRestore {
		in.Op = OpCancelPending
		bd.Intent, bd.CPAuthzSig, bd.HeldStatus = in.Encode(), nil, rnd(300)
	}
	b := bd.Encode()
	require.NotNil(t, b)
	return b
}

func patDeviceBundle(t *testing.T, account, op string, chainSeq, dseq uint64) []byte {
	dr := DeviceResult{RealmID: newID(), AccountID: account, RequestID: newID(), DeviceIntentDigest: digestOf("D"),
		Op: op, Dseq: dseq, PrevDeviceEntryDigest: digestOf("p"), ChainSeq: chainSeq, ChainEntryDigest: digestOf("ce"),
		DeviceSetDigest: digestOf("s"), IssuedAtMs: 1, DelegationSerial: 1}
	b := DeviceBundle{DeviceIntent: rnd(300), SigActor: rnd(96), DeviceResult: dr.Encode(), DeviceResultSig: rnd(96)}.Encode()
	require.NotNil(t, b)
	return b
}

func encItem(t *testing.T, it ImportItem) []byte {
	b := it.Encode()
	require.NotNil(t, b)
	return b
}

func TestCheckImportItem(t *testing.T) {
	acct, ev := newID(), rnd(200)
	pub := patBundle(t, acct, 5, ActorUser, false)
	attached := patBundle(t, acct, 6, ActorUser, true)
	restore := patBundle(t, acct, 7, ActorRestore, false)
	entry := patDeviceBundle(t, acct, OpRotateRoot, 6, 2)
	privateOp := patDeviceBundle(t, acct, OpAddDevice, 5, 1)
	for name, it := range map[string]ImportItem{
		"a published op with no device part":      {Bundle: pub, CPEvidence: ev},
		"a published op with its attached entry":  {Bundle: attached, DeviceBundle: entry, CPEvidence: ev},
		"a restore-cancel carries no cp-evidence": {Bundle: restore},
		"a private-op item has bundle \"\"":       {DeviceBundle: privateOp, CPEvidence: ev},
		"an operator op is not a restore-cancel":  {Bundle: patBundle(t, acct, 8, ActorOperator, false), CPEvidence: ev},
		"a bind_recovery private-op item":         {DeviceBundle: patDeviceBundle(t, acct, OpBindRecovery, 5, 2), CPEvidence: ev},
		"a cancel_recovery_bind private-op item":  {DeviceBundle: patDeviceBundle(t, acct, OpCancelRecoveryBind, 5, 3), CPEvidence: ev},
	} {
		_, err := CheckImportItem(encItem(t, it))
		require.NoError(t, err, name)
	}
	for name, it := range map[string]ImportItem{
		"neither part":                                 {CPEvidence: ev},
		"a published op without cp-evidence":           {Bundle: pub},
		"a restore-cancel with cp-evidence":            {Bundle: restore, CPEvidence: ev},
		"a private-op item without cp-evidence":        {DeviceBundle: privateOp},
		"a private-op entry beside a bundle":           {Bundle: pub, DeviceBundle: privateOp, CPEvidence: ev},
		"an attached entry without its bundle":         {DeviceBundle: entry, CPEvidence: ev},
		"a device part without its attached entry":     {Bundle: attached, CPEvidence: ev},
		"an attached entry beside an op with no part":  {Bundle: pub, DeviceBundle: entry, CPEvidence: ev},
		"a bundle that is not a bundle":                {Bundle: rnd(200), CPEvidence: ev},
		"a device_bundle that is not a device-bundle":  {DeviceBundle: rnd(200), CPEvidence: ev},
		"a bundle whose cp_authz is not a cp-authz":    {Bundle: corruptBundle(t, pub, func(b *Bundle) { b.CPAuthz = rnd(90) }), CPEvidence: ev},
		"a bundle whose intent is not an intent":       {Bundle: corruptBundle(t, pub, func(b *Bundle) { b.Intent = rnd(90) }), CPEvidence: ev},
		"a bundle whose result is not a result":        {Bundle: corruptBundle(t, pub, func(b *Bundle) { b.Result = rnd(90) }), CPEvidence: ev},
		"an entry whose device_result is not a result": {DeviceBundle: corruptDeviceBundle(t, privateOp), CPEvidence: ev},
	} {
		_, err := CheckImportItem(encItem(t, it))
		require.ErrorIs(t, err, ErrMalformed, name)
	}
	_, err := CheckImportItem(rnd(50))
	require.ErrorIs(t, err, ErrMalformed, "not an import-item")
}

func corruptBundle(t *testing.T, b []byte, f func(*Bundle)) []byte {
	bd, err := DecodeBundle(b)
	require.NoError(t, err)
	f(&bd)
	out := bd.Encode()
	require.NotNil(t, out)
	return out
}

func corruptDeviceBundle(t *testing.T, b []byte) []byte {
	db, err := DecodeDeviceBundle(b)
	require.NoError(t, err)
	db.DeviceResult = rnd(90)
	out := db.Encode()
	require.NotNil(t, out)
	return out
}

// twoAccounts returns two account IDs in ascending order.
func twoAccounts() (string, string) {
	a, b := newID(), newID()
	for a == b {
		b = newID()
	}
	if a > b {
		a, b = b, a
	}
	return a, b
}

func TestCheckImportBatch(t *testing.T) {
	a, b := twoAccounts()
	ev := rnd(200)
	items := [][]byte{
		encItem(t, ImportItem{Bundle: patBundle(t, a, 5, ActorUser, false), CPEvidence: ev}),           // (a, 5, 0)
		encItem(t, ImportItem{DeviceBundle: patDeviceBundle(t, a, OpAddDevice, 5, 1), CPEvidence: ev}), // (a, 5, 1)
		encItem(t, ImportItem{Bundle: patBundle(t, a, 6, ActorUser, true),
			DeviceBundle: patDeviceBundle(t, a, OpRotateEK, 6, 2), CPEvidence: ev}), // (a, 6, 2)
		encItem(t, ImportItem{Bundle: patBundle(t, b, 1, ActorUser, false), CPEvidence: ev}), // (b, 1, 0)
	}
	ok := func(xs ...[]byte) {
		t.Helper()
		_, err := CheckImportBatch(ImportBatch{Items: xs}.Encode())
		require.NoError(t, err)
	}
	bad := func(name string, xs ...[]byte) {
		t.Helper()
		_, err := CheckImportBatch(ImportBatch{Items: xs}.Encode())
		require.ErrorIs(t, err, ErrMalformed, name)
	}
	ok(items...)
	ok(items[0])
	bad("account_id descending", items[3], items[0])
	bad("chain_seq descending within an account", items[2], items[1])
	bad("dseq descending at one chain_seq", items[1], items[0])
	bad("a duplicate key", items[0], items[0])
	bad("an item that breaks its own rule", items[0], encItem(t, ImportItem{Bundle: patBundle(t, b, 1, ActorUser, false)}))
	_, err := CheckImportBatch(rnd(40))
	require.ErrorIs(t, err, ErrMalformed, "not an import-batch")
}

func TestCheckHoldReq(t *testing.T) {
	a, b := twoAccounts()
	ba, bb := patBundle(t, a, 5, ActorUser, false), patBundle(t, b, 2, ActorUser, false)
	_, err := CheckHoldReq(HoldReq{Bundles: [][]byte{ba, bb}}.Encode())
	require.NoError(t, err)
	_, err = CheckHoldReq(HoldReq{Bundles: [][]byte{ba}}.Encode())
	require.NoError(t, err)
	for name, h := range map[string]HoldReq{
		"descending account_id":    {Bundles: [][]byte{bb, ba}},
		"two bundles for one acct": {Bundles: [][]byte{ba, patBundle(t, a, 6, ActorUser, false)}},
		"a bundle that is not one": {Bundles: [][]byte{ba, rnd(80)}},
	} {
		_, err := CheckHoldReq(h.Encode())
		require.ErrorIs(t, err, ErrMalformed, name)
	}
	_, err = CheckHoldReq(rnd(40))
	require.ErrorIs(t, err, ErrMalformed, "not a hold-req")
}

func TestCheckCommittedPairing(t *testing.T) {
	res := sampleResult(t).Encode()
	_, err := CheckCommitted(Committed{Result: res, ResultSig: rnd(96), DeviceResult: rnd(100), DeviceResultSig: rnd(96)}.Encode())
	require.NoError(t, err, "an attached entry with its signature")
	_, err = CheckCommitted(Committed{Result: res, ResultSig: rnd(96), DeviceResultSig: rnd(96)}.Encode())
	require.ErrorIs(t, err, ErrMalformed, "a signature without its device_result")
	_, err = CheckCommitted(rnd(30))
	require.ErrorIs(t, err, ErrMalformed)
}

func TestCheckQueryReqPinDigest(t *testing.T) {
	spki := testEKSPKI(t)
	other := digestOf("another pin")
	q := Query{RealmID: newID(), AccountID: newID(), SignerDigest: other, IssuedAtMs: 1, Nonce: rnd(16)}
	_, _, err := CheckQueryReq(QueryReq{Query: q.Encode(), QuerySig: rnd(512), LegacySPKI: spki}.Encode(), other)
	require.ErrorIs(t, err, ErrMalformed, "a pin-signed query whose spki is not the pin's key")
	_, _, err = CheckQueryReq(QueryReq{Query: q.Encode(), QuerySig: rnd(96)}.Encode(), nil)
	require.NoError(t, err, "no pin: every signer is a key signer")
	_, _, err = CheckQueryReq(QueryReq{Query: rnd(60), QuerySig: rnd(96)}.Encode(), other)
	require.ErrorIs(t, err, ErrMalformed, "a query that is not a query")
}

func TestCheckRelayUndecodable(t *testing.T) {
	_, err := CheckRelay(Relay{Intent: rnd(70), SigCurrentRoot: rnd(96)}.Encode())
	require.ErrorIs(t, err, ErrMalformed, "an intent that is not an intent")
	_, err = CheckRelay(rnd(30))
	require.ErrorIs(t, err, ErrMalformed)
	rr := RelayRecover{Intent: cancelIntent(t), SigRecovery: rnd(96)}.Encode()
	require.NotNil(t, rr)
	_, err = CheckRelay(rr)
	require.ErrorIs(t, err, ErrMalformed, "a relay-recover is not a relay")
}

func recoverIntent(t *testing.T) []byte {
	in := sampleIntent(t)
	in.Op, in.RecoveryBindDigest = OpRecover, digestOf("B")
	b := in.Encode()
	require.NotNil(t, b)
	return b
}

func TestCheckRelayRecover(t *testing.T) {
	key := testPoint(t)
	cancelB := RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: newID(), Kind: BindKindCancel, ExpiresAtMs: 5,
		RecoveryKey: key}.Encode()
	bindB := sampleBind(t).Encode()
	formR := func(intent []byte) RelayRecover {
		return RelayRecover{Intent: intent, SigCandidate: rnd(96), SigRecovery: rnd(96), EKBinding: rnd(200),
			EKBindingSig: rnd(96), RecoveryBind: bindB, RecoveryBindSig: rnd(96), DeviceIntent: rnd(300), SigNewDevice: rnd(96)}
	}
	for name, r := range map[string]RelayRecover{
		"form R: recover":        formR(recoverIntent(t)),
		"form C: cancel_pending": {Intent: cancelIntent(t), SigRecovery: rnd(96)},
		"form B: a sheet cancel": {RecoveryBind: cancelB, SigRecovery: rnd(96)},
	} {
		b := r.Encode()
		require.NotNil(t, b, name)
		_, err := CheckRelayRecover(b)
		require.NoError(t, err, name)
	}
	for name, r := range map[string]RelayRecover{
		"form R with a cancel_pending intent": formR(cancelIntent(t)),
		"form R with a rotate_root intent":    formR(sampleIntent(t).Encode()),
		"form C with a recover intent":        {Intent: recoverIntent(t), SigRecovery: rnd(96)},
		"form C with a rotate_root intent":    {Intent: sampleIntent(t).Encode(), SigRecovery: rnd(96)},
		"form C whose intent is not one":      {Intent: rnd(80), SigRecovery: rnd(96)},
		"form B with a bind B":                {RecoveryBind: bindB, SigRecovery: rnd(96)},
		"form B with an unbind B":             {RecoveryBind: unbindB(t), SigRecovery: rnd(96)},
		"form B whose B is not one":           {RecoveryBind: rnd(80), SigRecovery: rnd(96)},
	} {
		b := r.Encode()
		require.NotNil(t, b, name)
		_, err := CheckRelayRecover(b)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
	_, err := CheckRelayRecover(Relay{Intent: cancelIntent(t), SigCurrentRoot: rnd(96)}.Encode())
	require.ErrorIs(t, err, ErrMalformed, "a relay is not a relay-recover")
}

func unbindB(t *testing.T) []byte {
	b := sampleBind(t)
	b.Kind, b.RecoveryKey, b.EscrowPublicKey = BindKindUnbind, nil, nil
	out := b.Encode()
	require.NotNil(t, out)
	return out
}

func TestCheckDeviceSubmit(t *testing.T) {
	nd := testDev(t, 0)
	di := func(op, requestID string) []byte {
		d := devIntent(op)
		d.RequestID, d.ExpiresAtMs, d.ExpectedDseq, d.ExpectedDeviceHead, d.ActorDeviceID = requestID, 9, 2, digestOf("dh"), newID()
		switch op {
		case OpAddDevice:
			d.NewDeviceID, d.NewDeviceDSK, d.NewDeviceDKX = nd.ID, nd.DSK[:], nd.DKX[:]
		case OpRotateRoot:
			d.ExpiresAtMs = 0
		default:
			d.RecoveryBindDigest = digestOf("B")
		}
		b := d.Encode()
		require.NotNil(t, b)
		return b
	}
	req := newID()
	bindB := sampleBind(t)
	bindB.RequestID = req
	// ExpiresAtMs 9 is di's #8, which the root form's B #7 must equal (E3-10, V3-06).
	cancelB := RecoveryBind{RealmID: newID(), AccountID: newID(), RequestID: req, Kind: BindKindCancel, ExpiresAtMs: 9,
		RecoveryKey: testPoint(t)}
	enc := func(b RecoveryBind) []byte { x := b.Encode(); require.NotNil(t, x); return x }
	unbindRB := sampleBind(t)
	unbindRB.RequestID, unbindRB.Kind, unbindRB.RecoveryKey, unbindRB.EscrowPublicKey = req, BindKindUnbind, nil, nil
	unbind := enc(unbindRB)
	sub := func(d DeviceSubmit) DeviceSubmit {
		d.CPAuthz, d.CPAuthzSig, d.CPEvidence = rnd(300), rnd(96), rnd(200)
		return d
	}
	s := rnd(96)
	for name, d := range map[string]DeviceSubmit{
		"add_device":                {DeviceIntent: di(OpAddDevice, req), SigRoot: s, SigActor: s, SigNewDevice: s},
		"bind: first or root-only":  {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: enc(bindB), SigRecovery: s},
		"bind: with the old key":    {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: enc(bindB), SigRecovery: s, SigRecoveryPrior: s},
		"unbind: root only":         {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: unbind},
		"unbind: with the old key":  {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: unbind, SigRecoveryPrior: s},
		"cancel: root form":         {DeviceIntent: di(OpCancelRecoveryBind, req), SigRoot: s, SigActor: s, RecoveryBind: enc(cancelB)},
		"cancel: sheet form":        {RecoveryBind: enc(cancelB), SigRecovery: s},
		"bind kind is DR9's to say": {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: enc(cancelB)},
	} {
		_, err := CheckDeviceSubmit(sub(d).Encode())
		require.NoError(t, err, name)
	}
	otherReq := cancelB
	otherReq.RequestID = newID()
	otherBind := bindB
	otherBind.RequestID = newID()
	otherUnbind := unbindRB
	otherUnbind.RequestID = newID()
	otherExpiry := cancelB
	otherExpiry.ExpiresAtMs = 10
	for name, d := range map[string]DeviceSubmit{
		"add_device without sig_new_device":    {DeviceIntent: di(OpAddDevice, req), SigRoot: s, SigActor: s},
		"add_device without sig_root":          {DeviceIntent: di(OpAddDevice, req), SigActor: s, SigNewDevice: s},
		"add_device with a recovery_bind":      {DeviceIntent: di(OpAddDevice, req), SigRoot: s, SigActor: s, SigNewDevice: s, RecoveryBind: enc(bindB)},
		"add_device with sig_recovery_prior":   {DeviceIntent: di(OpAddDevice, req), SigRoot: s, SigActor: s, SigNewDevice: s, SigRecoveryPrior: s},
		"bind without the new key's signature": {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: enc(bindB)},
		"bind without its B":                   {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, SigRecovery: s},
		"bind whose B is not one":              {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: rnd(80), SigRecovery: s},
		"bind without sig_actor":               {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, RecoveryBind: enc(bindB), SigRecovery: s},
		"bind with sig_new_device":             {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, SigNewDevice: s, RecoveryBind: enc(bindB), SigRecovery: s},
		"unbind with sig_recovery":             {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: unbind, SigRecovery: s},
		"root cancel with sig_recovery":        {DeviceIntent: di(OpCancelRecoveryBind, req), SigRoot: s, SigActor: s, RecoveryBind: enc(cancelB), SigRecovery: s},
		"root cancel with sig_recovery_prior":  {DeviceIntent: di(OpCancelRecoveryBind, req), SigRoot: s, SigActor: s, RecoveryBind: enc(cancelB), SigRecoveryPrior: s},
		"root cancel, E3-10 request_id":        {DeviceIntent: di(OpCancelRecoveryBind, req), SigRoot: s, SigActor: s, RecoveryBind: enc(otherReq)},
		"root cancel, E3-10 expiry":            {DeviceIntent: di(OpCancelRecoveryBind, req), SigRoot: s, SigActor: s, RecoveryBind: enc(otherExpiry)},
		"bind, E3-10 request_id":               {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: enc(otherBind), SigRecovery: s},
		"unbind, E3-10 request_id":             {DeviceIntent: di(OpBindRecovery, req), SigRoot: s, SigActor: s, RecoveryBind: enc(otherUnbind)},
		"root cancel without sig_root":         {DeviceIntent: di(OpCancelRecoveryBind, req), SigActor: s, RecoveryBind: enc(cancelB)},
		"sheet cancel with a bind B":           {RecoveryBind: enc(bindB), SigRecovery: s},
		"sheet cancel without sig_recovery":    {RecoveryBind: enc(cancelB)},
		"sheet cancel with sig_actor":          {RecoveryBind: enc(cancelB), SigRecovery: s, SigActor: s},
		"sheet cancel with sig_root":           {RecoveryBind: enc(cancelB), SigRecovery: s, SigRoot: s},
		"sheet cancel with sig_recovery_prior": {RecoveryBind: enc(cancelB), SigRecovery: s, SigRecoveryPrior: s},
		"no device-intent and no B":            {SigRecovery: s},
		"an attached op is never submitted":    {DeviceIntent: di(OpRotateRoot, req), SigRoot: s, SigActor: s},
		"a device_intent that is not one":      {DeviceIntent: rnd(90), SigRoot: s, SigActor: s, SigNewDevice: s},
	} {
		b := sub(d).Encode()
		require.NotNil(t, b, name)
		_, err := CheckDeviceSubmit(b)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
	// The sheet form has no device-intent: its B #7 is its only expiry.
	_, err := CheckDeviceSubmit(sub(DeviceSubmit{RecoveryBind: enc(otherExpiry), SigRecovery: s}).Encode())
	require.NoError(t, err, "sheet cancel with another expiry")
	_, err = CheckDeviceSubmit(rnd(40))
	require.ErrorIs(t, err, ErrMalformed, "not a device-submit")
}
