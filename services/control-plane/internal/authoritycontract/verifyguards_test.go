package authoritycontract

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// B-SF1 (CWE-345; DoR:715 T3 "realm matches", :925 realm_mismatch, D-115):
// a status names the realm of the Trust it is verified under, not only the
// caller's head. Without it, a caller that pairs the wrong Trust with a head
// takes another realm's RSK as the authority for that head: its first-sight
// pin, its state verdict and its wrap.
func TestVerifyStatusRefusesAStatusOfAnotherRealm(t *testing.T) {
	w := newSeqZeroWorld(t)
	pin := d48("pin")
	_, _, r := VerifyStatus(w.tr, HighWater{}, w.head, w.status(t, pin, nil), AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonOK, r, "control: the trust's own realm")

	other := newID()
	head := GenesisHead(other, w.acct)
	st := w.status(t, pin, func(s *Status) { s.RealmID = other }) // signed by this realm's RSK
	_, _, r = VerifyStatus(w.tr, HighWater{}, head, st, AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonRealmMismatch, r)
	_, _, _, r = VerifyChain(w.tr, HighWater{}, head, ChainInput{Status: st}, AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonRealmMismatch, r)
	_, r = VerifyBundle(w.tr, HighWater{}, head, []byte("bundle never read"), &Fix{Status: st})
	require.Equal(t, ReasonRealmMismatch, r, "VerifyBundle's fix passes the same check")
}

// B-SF1 for a result: every part of a bundle names the head's realm
// (linkCheck), and that realm must be the Trust's. The bundle here is
// consistent in itself and signed by the trust's own RSK and CAK.
func TestVerifyBundleRefusesAResultOfAnotherRealm(t *testing.T) {
	c := newForgeChain(t)
	b, _, _, _ := c.enrollNew(testKey(t), forgedEK(t), testT0)
	_, r := VerifyBundle(c.tr, HighWater{}, GenesisHead(c.realm, c.account), b, nil)
	require.Equal(t, ReasonOK, r, "control: the trust's own realm")

	c.realm = newID()
	b, _, _, st := c.enrollNew(testKey(t), forgedEK(t), testT0)
	genesis := GenesisHead(c.realm, c.account)
	_, r = VerifyBundle(c.tr, HighWater{}, genesis, b, nil)
	require.Equal(t, ReasonRealmMismatch, r)
	_, _, _, r = VerifyChain(c.tr, HighWater{}, genesis, ChainInput{Bundles: [][]byte{b}, Status: st},
		AccountMemory{}, nowOf(t, testT0+1_000), UseNewSecret)
	require.Equal(t, ReasonRealmMismatch, r)
}

// B-N1: §2.13's state verdict names every state; a state it does not know is
// malformed, never the absent path's ok.
func TestStateVerdictRefusesAnUnknownState(t *testing.T) {
	st := Status{LegacyEKDigest: d48("pin")}
	var vs VerifiedStatus
	require.Equal(t, ReasonOK, stateVerdict(pub{State: StateAbsent}, st, AccountMemory{}, Delegation{}, UseNewSecret, &vs),
		"control: the absent path admits a first-sight pin")
	for _, s := range []string{"", "bogus"} {
		vs = VerifiedStatus{}
		require.Equal(t, ReasonMalformed, stateVerdict(pub{State: s}, st, AccountMemory{}, Delegation{}, UseNewSecret, &vs), s)
	}
}

// B-N2 (DoR:930, R6 DoR:741): only "a continuity change pending from absent"
// is ok and wraps to the pin SPKI, so each of its conditions is checked, not
// reached by elimination: the prior state is absent, the pin is set and not
// revoked, and the change is continuity.
func TestPendingVerdictContinuityNeedsALivePinFromAbsent(t *testing.T) {
	base := pub{State: StatePending, Prior: StateAbsent, PendRoot: testPoint(t), Assurance: AssuranceContinuity,
		Pin: d48("pin")}
	var vs VerifiedStatus
	require.Equal(t, ReasonOK, pendingVerdict(base, &vs), "control")
	require.True(t, vs.continuity)
	for name, tc := range map[string]struct {
		mut  func(*pub)
		want Reason
	}{
		"no prior state":      {func(p *pub) { p.Prior = "" }, ReasonMalformed},
		"prior disputed":      {func(p *pub) { p.Prior = StateDisputed }, ReasonMalformed},
		"no pin":              {func(p *pub) { p.Pin = nil }, ReasonPendingFromAbsent},
		"a revoked pin":       {func(p *pub) { p.PinRevoked = 1 }, ReasonPendingFromAbsent},
		"not continuity":      {func(p *pub) { p.Assurance = AssuranceUnverified }, ReasonPendingFromAbsent},
		"prior active":        {func(p *pub) { p.Prior = StateActive }, ReasonPendingFromActive},
		"prior suspended":     {func(p *pub) { p.Prior = StateSuspended }, ReasonPendingFromSuspended},
		"a pending erase":     {func(p *pub) { p.PendRoot = nil }, ReasonPendingErase},
		"no pin, no prior":    {func(p *pub) { p.Pin, p.Prior = nil, "" }, ReasonMalformed},
		"revoked, not contin": {func(p *pub) { p.PinRevoked, p.Assurance = 1, AssuranceRecovery }, ReasonPendingFromAbsent},
	} {
		eff := base
		tc.mut(&eff)
		vs := VerifiedStatus{}
		require.Equal(t, tc.want, pendingVerdict(eff, &vs), name)
		require.False(t, vs.continuity, name)
	}
}

// Sec-N4: msOf reads the zero time.Time and every time before the epoch as
// 0, so each exported function that takes `now` refuses a now of 0 ms as
// malformed rather than judge freshness at 1970 (which reads every status as
// status_future, a transient). The epoch itself is refused too: no status is
// fresh then.
func TestVerifyRefusesAnUnsetNow(t *testing.T) {
	w := newSeqZeroWorld(t)
	st := w.status(t, d48("pin"), nil)
	_, _, r := VerifyStatus(w.tr, HighWater{}, w.head, st, AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonOK, r, "control")
	_, _, _, r = VerifyChain(w.tr, HighWater{}, w.head, ChainInput{Status: st}, AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonOK, r, "control")

	for name, now := range map[string]time.Time{"the zero time": {}, "before the epoch": time.UnixMilli(-5),
		"the epoch": time.UnixMilli(0), "below a millisecond after the epoch": time.Unix(0, 999_999)} {
		_, _, r := VerifyStatus(w.tr, HighWater{}, w.head, st, AccountMemory{}, now, UseNewSecret)
		require.Equal(t, ReasonMalformed, r, "VerifyStatus, %s", name)
		_, _, _, r = VerifyChain(w.tr, HighWater{}, w.head, ChainInput{Status: st}, AccountMemory{}, now, UseNewSecret)
		require.Equal(t, ReasonMalformed, r, "VerifyChain, %s", name)
		_, _, _, _, r = VerifyDeviceLedger(w.tr, HighWater{}, DeviceHead{RealmID: w.r.ID, AccountID: w.acct, Chain: w.head},
			DeviceLedgerInput{Status: st}, AccountMemory{}, now, UseNewSecret)
		require.Equal(t, ReasonMalformed, r, "VerifyDeviceLedger, %s", name)
	}
}

// E-S1 + open item 1 (DoR:872, :936, :961; R6): WrapKey wraps only through a
// value VerifyStatus returned for a status it judged with UseNewSecret (τ
// freshness). A struct literal carries no seal, a History status is fresh
// only within V, and the seal is a copy, so editing a returned value's
// exported fields moves no wrap.
func TestWrapKeyRefusesAValueVerifyStatusDidNotReturn(t *testing.T) {
	ek := testEKSPKI(t)
	_, ok := VerifiedStatus{Reason: ReasonOK, ekSPKI: ek}.WrapKey(nil)
	require.False(t, ok, "a struct literal")
	_, ok = VerifiedStatus{Reason: ReasonOK, Pin: Digest(ek), Status: Status{State: StateAbsent}}.WrapKey(ek)
	require.False(t, ok, "a struct literal on the served-SPKI path")
}

func TestWrapKeyRefusesAHistoryVerifiedStatus(t *testing.T) {
	a := newForgeActive(t)
	for name, tc := range map[string]struct {
		use    Use
		wantOK bool
	}{"new-secret": {UseNewSecret, true}, "history": {UseHistory, false}} {
		vs, _, r := VerifyStatus(a.tr, HighWater{}, a.head1, a.status1, AccountMemory{}, nowOf(t, testT0+1_000), tc.use)
		require.Equal(t, ReasonOK, r, name)
		spki, ok := vs.WrapKey(nil)
		require.Equal(t, tc.wantOK, ok, name)
		if ok {
			require.Equal(t, a.ek1, spki, name)
		}
	}
	// The served-SPKI path (an absent account) likewise.
	w := newSeqZeroWorld(t)
	spki := testEKSPKI(t)
	st := w.status(t, Digest(spki), nil)
	for name, tc := range map[string]struct {
		use    Use
		wantOK bool
	}{"new-secret": {UseNewSecret, true}, "history": {UseHistory, false}} {
		vs, _, r := VerifyStatus(w.tr, HighWater{}, w.head, st, AccountMemory{}, w.now, tc.use)
		require.Equal(t, ReasonOK, r, name)
		_, ok := vs.WrapKey(spki)
		require.Equal(t, tc.wantOK, ok, "absent, %s", name)
	}
}

func TestWrapKeyReadsOnlyTheSeal(t *testing.T) {
	a := newForgeActive(t)
	vs, _, r := VerifyStatus(a.tr, HighWater{}, a.head1, a.status1, AccountMemory{}, nowOf(t, testT0+1_000), UseNewSecret)
	require.Equal(t, ReasonOK, r)
	evil := forgedEK(t)
	vs.ekSPKI = evil
	got, ok := vs.WrapKey(nil)
	require.True(t, ok)
	require.Equal(t, a.ek1, got, "an edited ekSPKI is not what WrapKey wraps to")
	got[0] ^= 0xff
	again, _ := vs.WrapKey(nil)
	require.Equal(t, a.ek1, again, "the returned bytes are a copy")

	// A withheld verdict stays withheld whatever its exported fields say.
	vs, _, r = VerifyStatus(a.tr, HighWater{}, a.head1, a.status1, AccountMemory{Pin: d48("first sight")},
		nowOf(t, testT0+1_000), UseNewSecret)
	require.Equal(t, ReasonPinChanged, r)
	vs.Reason, vs.ekSPKI = ReasonOK, evil
	_, ok = vs.WrapKey(nil)
	require.False(t, ok)
}

// Sec-N3: a hand-built DeviceHead whose admitted_by record the codec refuses
// is malformed, never read past. The control is a committed device-ledger
// vector whose prior head carries an admitted record and verifies ok; only
// that record changes between the control and each case.
func TestVerifyDeviceLedgerRefusesAMalformedAdmittedRecord(t *testing.T) {
	for _, v := range loadVectors(t).Vectors {
		if v.Type != "device-ledger" || v.Expect.Reason != string(ReasonOK) {
			continue
		}
		from, err := ParseDeviceHead(mustB64(t, v.PrevDeviceHead), mustB64(t, v.PrevHead))
		require.NoError(t, err)
		if len(from.AdmittedBy) == 0 {
			continue
		}
		tr, r := trustOf(t, v)
		require.Equal(t, ReasonOK, r)
		use, ok := ParseUse(v.Use)
		require.True(t, ok)
		run := func(h DeviceHead) Reason {
			_, _, _, _, got := VerifyDeviceLedger(tr, hwOf(v.HW), h, ledgerInputOf(t, v), memOf(t, v.Mem), nowOf(t, v.NowMs), use)
			return got
		}
		require.Equal(t, ReasonOK, run(from), "control: %s", v.Name)
		good := from.AdmittedBy[0]
		for name, rec := range map[string]string{"no separator": newID(), "not a uuid": "x.",
			"three parts": good + ".x", "upper-case id": strings.ToUpper(good)} {
			bad := from
			bad.AdmittedBy = append([]string{rec}, from.AdmittedBy[1:]...)
			require.Equal(t, ReasonMalformed, run(bad), "%s: %s", v.Name, name)
		}
		return
	}
	t.Fatal("no ok device-ledger vector carries an admitted record")
}
