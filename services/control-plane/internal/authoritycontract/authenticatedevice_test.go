package authoritycontract

// AuthenticateDeviceBundle (#3587 PR-B3, decision B3-D17) is
// AuthenticateBundle's counterpart for a lone private-op device entry, which
// import's pre-pass authenticates before any head is read (B3-D16): EN5,
// realm and account, §2.5's identity rules with no step, and T7's RSK
// signature, window and scope under a delegation the Trust retains at the
// device result's (gen, serial). It reads no device head.

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// devForge is a private add_device entry at dseq 4, chain_seq 2, signed by
// c's RSK under (0, 1), whose prev digests link to nothing:
// AuthenticateDeviceBundle must read neither.
func devForge(t *testing.T, c *forgeChain, mut func(*DeviceIntent, *CPAuthz, *DeviceResult)) []byte {
	t.Helper()
	issued := testT0 + 60_000
	di := DeviceIntent{RealmID: c.realm, AccountID: c.account, RequestID: newID(), Op: OpAddDevice,
		Salt: make([]byte, 32), ExpiresAtMs: issued + 600_000, ExpectedSeq: 2, ExpectedHeadDigest: digestOf("head 2"),
		ExpectedDseq: 3, ExpectedDeviceHead: digestOf("device 3"), ActorDeviceID: newID(), NewDeviceID: newID(),
		NewDeviceDSK: pointOf(t, testKey(t)), NewDeviceDKX: pointOf(t, testKey(t)), RemovedDeviceIDs: []string{}}
	a := CPAuthz{RealmID: c.realm, AccountID: c.account, RequestID: di.RequestID, Actor: ActorUser,
		AssertedAtMs: issued, EvidenceDigest: digestOf("ev")}
	dr := DeviceResult{RealmID: c.realm, AccountID: c.account, RequestID: di.RequestID, Op: OpAddDevice, Dseq: 4,
		PrevDeviceEntryDigest: digestOf("device 3"), ChainSeq: 2, ChainEntryDigest: digestOf("head 2"),
		DeviceSetDigest: digestOf("a set"), IssuedAtMs: issued, DelegationGen: 0, DelegationSerial: 1}
	if mut != nil {
		mut(&di, &a, &dr)
	}
	diB := di.Encode()
	require.NotNil(t, diB, "device-intent")
	if a.IntentDigest == nil {
		a.IntentDigest = Digest(diB)
	}
	aB := a.Encode()
	require.NotNil(t, aB, "cp-authz")
	if dr.DeviceIntentDigest == nil {
		dr.DeviceIntentDigest = Digest(diB)
	}
	if dr.CPAuthzDigest == nil {
		dr.CPAuthzDigest = Digest(aB)
	}
	drB := dr.Encode()
	require.NotNil(t, drB, "device-result")
	out := DeviceBundle{DeviceIntent: diB, SigRoot: c.sign(testKey(t), drB), SigActor: c.sign(testKey(t), drB),
		SigNewDevice: c.sign(testKey(t), drB), CPAuthz: aB, CPAuthzSig: c.sign(c.r.CAK, aB), DeviceResult: drB,
		DeviceResultSig: c.sign(c.r.RSK, drB)}.Encode()
	require.NotNil(t, out, "device-bundle")
	return out
}

func redevice(t *testing.T, b []byte, mut func(*DeviceBundle)) []byte {
	t.Helper()
	db, err := DecodeDeviceBundle(b)
	require.NoError(t, err)
	mut(&db)
	out := db.Encode()
	require.NotNil(t, out)
	return out
}

func forgeOn(t *testing.T, mut func(*Delegation)) *forgeChain {
	t.Helper()
	r := newTestRealm(t)
	r.addDel(1, mut)
	return &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
}

func TestAuthenticateDeviceBundleAcceptsAnEntryWithNoHead(t *testing.T) {
	c := newForgeChain(t)
	b := devForge(t, c, nil)
	res, sup, r := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
	require.Equal(t, ReasonOK, r)
	require.False(t, sup, "signed under the only delegation: not superseded")
	db, err := DecodeDeviceBundle(b)
	require.NoError(t, err)
	want, err := DecodeDeviceResult(db.DeviceResult)
	require.NoError(t, err)
	require.Equal(t, want, res, "the verified device result is returned, so the caller reads account and dseq from it")
	require.Equal(t, c.account, res.AccountID)
	require.Equal(t, uint64(4), res.Dseq)
}

func TestAuthenticateDeviceBundleRefusals(t *testing.T) {
	t.Run("bad_signature: the device-result signature does not verify", func(t *testing.T) {
		c := newForgeChain(t)
		b := redevice(t, devForge(t, c, nil), func(d *DeviceBundle) { d.DeviceResultSig = flipped(d.DeviceResultSig) })
		_, _, r := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonBadSignature, r)
	})

	t.Run("bad_signature: a device result signed by a key that is not an RSK", func(t *testing.T) {
		c := newForgeChain(t)
		b := redevice(t, devForge(t, c, nil), func(d *DeviceBundle) { d.DeviceResultSig = c.sign(testKey(t), d.DeviceResult) })
		_, _, r := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonBadSignature, r)
	})

	t.Run("malformed: bytes that are not a device bundle", func(t *testing.T) {
		c := newForgeChain(t)
		_, _, r := AuthenticateDeviceBundle(c.tr, HighWater{}, []byte("not a device bundle"))
		require.Equal(t, ReasonMalformed, r)
	})

	t.Run("delegation_unknown: no retained delegation at the device result's serial", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(2, nil) // the device result names (0, 1)
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, devForge(t, c, nil))
		require.Equal(t, ReasonDelegationUnknown, got)
	})

	t.Run("delegation_window: issued after the delegation's window", func(t *testing.T) {
		c := forgeOn(t, func(d *Delegation) { d.NotAfterMs = testT0 + 1_000 })
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, devForge(t, c, nil))
		require.Equal(t, ReasonDelegationWindow, got)
	})

	t.Run("out_of_scope: the delegation's scope omits add_device", func(t *testing.T) {
		c := forgeOn(t, func(d *Delegation) {
			d.Scope = slices.DeleteFunc(slices.Clone(scopeValues), func(s string) bool { return s == OpAddDevice })
		})
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, devForge(t, c, nil))
		require.Equal(t, ReasonOutOfScope, got)
	})

	t.Run("realm_mismatch: every part names another realm than the Trust's", func(t *testing.T) {
		c := newForgeChain(t)
		c.realm = newID()
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, devForge(t, c, nil))
		require.Equal(t, ReasonRealmMismatch, got)
	})

	t.Run("realm_mismatch: the device-intent names another account than the result", func(t *testing.T) {
		c := newForgeChain(t)
		b := devForge(t, c, func(di *DeviceIntent, _ *CPAuthz, _ *DeviceResult) { di.AccountID = newID() })
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonRealmMismatch, got)
	})

	t.Run("realm_mismatch: the cp-authz names another account than the result", func(t *testing.T) {
		c := newForgeChain(t)
		b := devForge(t, c, func(_ *DeviceIntent, a *CPAuthz, _ *DeviceResult) { a.AccountID = newID() })
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonRealmMismatch, got)
	})

	t.Run("device_rule: the device result does not name its device-intent", func(t *testing.T) {
		c := newForgeChain(t)
		b := devForge(t, c, func(_ *DeviceIntent, _ *CPAuthz, dr *DeviceResult) { dr.DeviceIntentDigest = digestOf("another") })
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonDeviceRule, got)
	})

	t.Run("device_rule: the device result does not name its cp-authz", func(t *testing.T) {
		c := newForgeChain(t)
		b := devForge(t, c, func(_ *DeviceIntent, _ *CPAuthz, dr *DeviceResult) { dr.CPAuthzDigest = digestOf("another") })
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonDeviceRule, got)
	})

	t.Run("device_rule: the device-intent's expected_seq is not the result's chain_seq", func(t *testing.T) {
		c := newForgeChain(t)
		b := devForge(t, c, func(di *DeviceIntent, _ *CPAuthz, _ *DeviceResult) { di.ExpectedSeq = 1 })
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonDeviceRule, got)
	})

	t.Run("device_rule: an attached op's entry does not authenticate alone", func(t *testing.T) {
		c := newForgeChain(t)
		b := devForge(t, c, func(di *DeviceIntent, _ *CPAuthz, dr *DeviceResult) {
			di.Op, dr.Op = OpRotateRoot, OpRotateRoot
		})
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonDeviceRule, got)
	})

	t.Run("trust_halt: a halted Trust admits nothing, before the entry's own Reason", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(1, nil)
		r.succeed(nil)
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		require.NotNil(t, c.tr.Halted(), "precondition: T4 halts the realm")
		b := devForge(t, c, nil)
		_, _, got := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.Equal(t, ReasonTrustHalt, got)
		_, _, got = AuthenticateDeviceBundle(c.tr, HighWater{}, redevice(t, b, func(d *DeviceBundle) { d.DeviceResultSig = flipped(d.DeviceResultSig) }))
		require.Equal(t, ReasonTrustHalt, got)
	})

	t.Run("trust_halt: a nil Trust", func(t *testing.T) {
		c := newForgeChain(t)
		_, _, got := AuthenticateDeviceBundle(nil, HighWater{}, devForge(t, c, nil))
		require.Equal(t, ReasonTrustHalt, got)
	})

	t.Run("a refusal returns the zero DeviceResult", func(t *testing.T) {
		c := newForgeChain(t)
		b := redevice(t, devForge(t, c, nil), func(d *DeviceBundle) { d.DeviceResultSig = flipped(d.DeviceResultSig) })
		res, sup, r := AuthenticateDeviceBundle(c.tr, HighWater{}, b)
		require.NotEqual(t, ReasonOK, r)
		require.False(t, sup)
		require.Equal(t, DeviceResult{}, res, "a caller that ignores the Reason must not read an unverified account or dseq")
	})
}

func TestAuthenticateDeviceBundleReportsASupersededEntry(t *testing.T) {
	t.Run("a renewal installed after the entry", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(1, nil)
		r.addDel(2, nil) // MinValidSerial = 2
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		res, sup, got := AuthenticateDeviceBundle(c.tr, HighWater{}, devForge(t, c, nil))
		require.Equal(t, ReasonOK, got)
		require.True(t, sup)
		require.Equal(t, c.account, res.AccountID)
	})

	t.Run("a persisted high-water mark past the entry's serial", func(t *testing.T) {
		c := newForgeChain(t)
		_, sup, got := AuthenticateDeviceBundle(c.tr, HighWater{Gen: 0, Serial: 2, MinValid: 2}, devForge(t, c, nil))
		require.Equal(t, ReasonOK, got)
		require.True(t, sup)
	})
}

// Every private entry of a device-ledger vector VerifyDeviceLedger accepts
// authenticates here (the checks are the ledger's own, in its order), with
// the entry's own device result returned. Attached entries are device_rule.
func TestAuthenticateDeviceBundleAgreesWithVerifyDeviceLedgerOnTheCorpus(t *testing.T) {
	private, attached := 0, 0
	for _, v := range loadVectors(t).Vectors {
		if v.Type != "device-ledger" || !stateReasons[Reason(v.Expect.Reason)] {
			continue
		}
		tr, r := trustOf(t, v)
		require.Equal(t, ReasonOK, r)
		from, err := ParseDeviceHead(mustB64(t, v.PrevDeviceHead), mustB64(t, v.PrevHead))
		require.NoError(t, err)
		use, ok := ParseUse(v.Use)
		require.True(t, ok)
		_, _, _, _, vr := VerifyDeviceLedger(tr, hwOf(v.HW), from, ledgerInputOf(t, v), memOf(t, v.Mem), nowOf(t, v.NowMs), use)
		require.True(t, stateReasons[vr], "%s: control: VerifyDeviceLedger says %s", v.Name, vr)
		for _, b := range listB64(t, v.DeviceBundles) {
			db, err := DecodeDeviceBundle(b)
			require.NoError(t, err)
			want, err := DecodeDeviceResult(db.DeviceResult)
			require.NoError(t, err)
			res, _, got := AuthenticateDeviceBundle(tr, hwOf(v.HW), b)
			if attachedOp(want.Op) {
				require.Equal(t, ReasonDeviceRule, got, "%s: an attached entry alone", v.Name)
				attached++
				continue
			}
			require.Equal(t, ReasonOK, got, "%s dseq %d", v.Name, want.Dseq)
			require.Equal(t, want, res, "%s dseq %d", v.Name, want.Dseq)
			private++
		}
	}
	require.Positive(t, private, "the corpus has accepted private entries")
	t.Logf("corpus: %d private entries authenticated, %d attached entries refused alone", private, attached)
}
