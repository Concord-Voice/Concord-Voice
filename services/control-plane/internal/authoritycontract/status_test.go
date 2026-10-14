package authoritycontract

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seqZeroWorld is one realm (gen 0, serial 1) and one account at genesis.
type seqZeroWorld struct {
	r    *testRealm
	tr   *Trust
	acct string
	head ChainHead
	now  time.Time
}

func newSeqZeroWorld(t *testing.T) seqZeroWorld {
	r := newTestRealm(t)
	r.addDel(1, nil)
	acct := newID()
	return seqZeroWorld{r: r, tr: r.trust(t), acct: acct, head: GenesisHead(r.ID, acct),
		now: time.UnixMilli(int64(testT0) + 1_000)}
}

// status signs an absent seq-0 status for the world's account carrying pin.
func (w seqZeroWorld) status(t *testing.T, pin []byte, mut func(*Status)) []byte {
	return w.r.status(t, func(s *Status) {
		s.AccountID, s.LegacyEKDigest = w.acct, pin
		if mut != nil {
			mut(s)
		}
	})
}

// The seq-0 pin (V3-05, D-149): R5 cannot derive it, so VerifyStatus compares
// the status pin with AccountMemory's first-sight pin itself (absentVerdict).
func TestVerifyStatusComparesTheSeqZeroPinWithMemory(t *testing.T) {
	w := newSeqZeroWorld(t)
	pin, other := d48("pin"), d48("other")
	st := w.status(t, pin, nil)
	cases := []struct {
		name string
		mem  AccountMemory
		use  Use
		want Reason
	}{
		{"first sight", AccountMemory{}, UseNewSecret, ReasonOK},
		{"unchanged", AccountMemory{Pin: pin}, UseNewSecret, ReasonOK},
		{"changed", AccountMemory{Pin: other}, UseNewSecret, ReasonPinChanged},
		{"accepted for this conversation", AccountMemory{Pin: other, AcceptedPin: pin}, UseNewSecret, ReasonOKAcceptedPin},
		{"acceptance needs a fresh status (LD-28)", AccountMemory{Pin: other, AcceptedPin: pin}, UseHistory, ReasonPinChanged},
		{"ever enrolled", AccountMemory{EverEnrolled: true}, UseNewSecret, ReasonAbsentAfterEnrolled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs, _, r := VerifyStatus(w.tr, HighWater{}, w.head, st, tc.mem, w.now, tc.use)
			require.Equal(t, tc.want, r)
			require.Equal(t, tc.want, vs.Reason)
			require.Equal(t, pin, vs.Pin, "the status pin is the first-sight value the caller stores")
		})
	}
	vs, _, r := VerifyStatus(w.tr, HighWater{}, w.head, w.status(t, nil, nil), AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonAbsentUnpinned, r)
	require.Empty(t, vs.Pin)
}

// checkR5 copies realm and account from the status, so VerifyStatus must
// compare them with the head itself.
func TestVerifyStatusComparesRealmAndAccount(t *testing.T) {
	w := newSeqZeroWorld(t)
	other := w.r.status(t, nil) // another account of the same realm
	_, _, r := VerifyStatus(w.tr, HighWater{}, w.head, other, AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonRealmMismatch, r)
	_, _, r = VerifyStatus(w.tr, HighWater{}, GenesisHead(newID(), w.acct), w.status(t, nil, nil), AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonRealmMismatch, r, "another realm's head")
	_, _, r = VerifyStatus(w.tr, HighWater{}, w.head, w.status(t, nil, nil), AccountMemory{}, w.now, 0)
	require.Equal(t, ReasonMalformed, r, "the zero Use is neither mode")
	_, _, r = VerifyStatus(nil, HighWater{}, w.head, w.status(t, nil, nil), AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonTrustHalt, r, "no trust is a halt")
}

// VerifyBundle verifies its fix argument before using it (fixAllows trusts R5
// ran). A verified fix keeps only its R4 meaning: at genesis it supplies no
// pin (TestVerifyBundleGenesisFixSuppliesNoPin).
func TestVerifyBundleVerifiesItsFix(t *testing.T) {
	w := newSeqZeroWorld(t)
	forged := Signed{Type: ScopeStatus, Msg: mustDecodeSigned(t, w.status(t, d48("pin"), nil)).Msg,
		SigA: signP1363(t, testKey(t), []byte("x"))}.Encode()
	for name, tc := range map[string]struct {
		fix  []byte
		want Reason
	}{
		"forged signature":     {forged, ReasonBadSignature},
		"another account":      {w.r.status(t, nil), ReasonRealmMismatch},
		"not a status":         {[]byte("not a signed message"), ReasonMalformed},
		"names a later head":   {w.status(t, d48("pin"), func(s *Status) { s.Seq = 1; s.HeadEntryDigest = d48("e1") }), ReasonChainBehind},
		"valid_until > V_MAX":  {w.status(t, d48("pin"), func(s *Status) { s.ValidUntilMs = s.AsOfMs + VMaxMs + 1 }), ReasonMalformed},
		"a derived field lies": {w.status(t, d48("pin"), func(s *Status) { s.RootRevoked = 1 }), ReasonFieldMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			head, r := VerifyBundle(w.tr, HighWater{}, w.head, []byte("bundle never read"), &Fix{Status: tc.fix})
			require.Equal(t, tc.want, r)
			require.Equal(t, w.head.Bytes(), head.Bytes(), "a refused call returns prev unchanged")
		})
	}
	_, r := VerifyBundle(w.tr, HighWater{}, w.head, []byte("not a bundle"), &Fix{Status: w.status(t, d48("pin"), nil)})
	require.Equal(t, ReasonMalformed, r, "a verified fix lets the bundle itself be judged")
}

func mustDecodeSigned(t *testing.T, b []byte) Signed {
	t.Helper()
	s, err := DecodeSigned(b)
	require.NoError(t, err)
	return s
}

func TestSeqZeroPinSources(t *testing.T) {
	w := newSeqZeroWorld(t)
	pin, mem := d48("status-pin"), d48("memory-pin")
	st := Status{LegacyEKDigest: pin} // passed T6 in signedStatus
	bundles := [][]byte{[]byte("b")}

	require.Equal(t, mem, chainSeq0Pin(w.head, bundles, st, AccountMemory{Pin: mem}), "AccountMemory's first-sight pin wins (V3-05)")
	require.Equal(t, pin, chainSeq0Pin(w.head, bundles, st, AccountMemory{}), "first sight: the status this call sees (D-149)")
	require.Nil(t, chainSeq0Pin(w.head, nil, st, AccountMemory{}), "no bundle needs a seq-0 hypothesis")
	require.Nil(t, chainSeq0Pin(ChainHead{HeadResult: []byte("r")}, bundles, st, AccountMemory{}),
		"past genesis the pin is the head result's, never the status's")
}

// R6: only the verified EK, or a served SPKI whose digest is the status pin
// on an absent or continuity-pending verdict, may be wrapped to. Each case is
// sealed as verifyStatus seals a NewSecret verdict (seal); an unsealed value
// wraps to nothing (TestWrapKeyRefusesAValueVerifyStatusDidNotReturn).
func TestWrapKey(t *testing.T) {
	spki := testEKSPKI(t)
	pin := Digest(spki)
	ek := append([]byte(nil), spki...)
	cases := []struct {
		name   string
		vs     VerifiedStatus
		served []byte
		want   []byte
	}{
		{"active: the verified EK", VerifiedStatus{Reason: ReasonOK, ekSPKI: ek}, nil, ek},
		{"absent, first-sight pin", VerifiedStatus{Reason: ReasonOK, Pin: pin, Status: Status{State: StateAbsent}}, spki, spki},
		{"absent, accepted pin", VerifiedStatus{Reason: ReasonOKAcceptedPin, Pin: pin, Status: Status{State: StateAbsent}}, spki, spki},
		{"continuity pending from absent", VerifiedStatus{Reason: ReasonOK, Pin: pin, continuity: true,
			Status: Status{State: StatePending}}, spki, spki},
		{"served SPKI is not the pin", VerifiedStatus{Reason: ReasonOK, Pin: d48("x"), Status: Status{State: StateAbsent}}, spki, nil},
		{"pending, not continuity", VerifiedStatus{Reason: ReasonOK, Pin: pin, Status: Status{State: StatePending}}, spki, nil},
		{"withheld verdict", VerifiedStatus{Reason: ReasonPinChanged, Pin: pin, ekSPKI: ek, Status: Status{State: StateAbsent}}, spki, nil},
		{"no pin", VerifiedStatus{Reason: ReasonOK, Status: Status{State: StateAbsent}}, spki, nil},
	}
	for _, tc := range cases {
		tc.vs.seal(UseNewSecret)
		got, ok := tc.vs.WrapKey(tc.served)
		require.Equal(t, tc.want != nil, ok, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
	bad := append([]byte(nil), spki...)
	bad[0] ^= 0xff
	vs := VerifiedStatus{Reason: ReasonOK, Pin: Digest(bad), Status: Status{State: StateAbsent}}
	vs.seal(UseNewSecret)
	_, ok := vs.WrapKey(bad)
	require.False(t, ok, "a served SPKI must be a valid EK (§2.4)")
}

// A9: every root the chain installs is recorded in root_index with a
// non-empty segment, so segmentOf never answers "" for an installed root.
func TestSegmentOfEveryInstalledRoot(t *testing.T) {
	enrolRoot, rotRoot, pendRoot := testPoint(t), testPoint(t), testPoint(t)
	h := GenesisHead(newID(), newID())
	appendResult(&h, parsedBundle{res: Result{Op: OpEnrollNew, State: StateActive, ActiveRoot: enrolRoot}, entry: d48("e1")}, "")
	require.Equal(t, Assurance(AssuranceInitial), segmentOf(h, enrolRoot), "enroll_new installs initial")
	appendResult(&h, parsedBundle{res: Result{Op: OpRotateRoot, State: StateActive, ActiveRoot: rotRoot}, entry: d48("e2")},
		AssuranceUnverified)
	require.Equal(t, Assurance(AssuranceUnverified), segmentOf(h, rotRoot), "rotate_root records the segment's assurance")
	require.Equal(t, Assurance(AssuranceInitial), segmentOf(h, enrolRoot), "an earlier root keeps its record")
	recordActivation(&h, Result{Seq: 2, Op: OpReplaceRoot, PendingRoot: pendRoot, Assurance: AssuranceRecovery},
		pub{State: StateActive})
	require.Equal(t, Assurance(AssuranceRecovery), segmentOf(h, pendRoot), "an activation records the result's assurance")
	require.True(t, strings.HasSuffix(h.SeqIndex[1], b64s(Digest(pendRoot))), "an activation yielding active binds its root")
	require.Equal(t, Assurance(""), segmentOf(h, testPoint(t)), "a root the chain never installed")
	require.Equal(t, Assurance(""), segmentOf(h, nil))

	// Every chain-head the committed vectors carry obeys the same rule.
	for _, v := range loadVectors(t).Vectors {
		for _, s := range []string{v.PrevHead, v.Expect.HeadB64} {
			if s == "" || v.Type == "chain-head" {
				continue
			}
			ch, err := ParseChainHead(mustB64(t, s))
			require.NoError(t, err, v.Name)
			for _, rec := range ch.RootIndex {
				require.NotEqual(t, '.', rec[len(rec)-1], "%s: a root_index record with no segment", v.Name)
			}
			if r, err := DecodeResult(ch.HeadResult); err == nil && len(r.ActiveRoot) > 0 {
				require.NotEmpty(t, segmentOf(ch, r.ActiveRoot), "%s: the head's active root has a segment", v.Name)
			}
		}
	}
}

// appendResult empties the pending EK slot exactly when result #17 is ""
// (DoR:865), so a pending erase burns a pending candidate's EK.
func TestAppendResultPendingEKFollowsResult17(t *testing.T) {
	h := ChainHead{PendingEKSPKI: testEKSPKI(t), PendingEKGeneration: 2, SeqIndex: []string{SeqRecord(d48("e1"), nil)}}
	appendResult(&h, parsedBundle{res: Result{Op: OpErase, State: StatePending}, entry: d48("e2")}, "")
	require.Empty(t, h.PendingEKSPKI, "a pending erase (#17 \"\") empties the slot")
	require.Zero(t, h.PendingEKGeneration)
}

func TestHeadPubAndRetainFix(t *testing.T) {
	_, r := headPub(ChainHead{SeqIndex: []string{"x"}})
	require.Equal(t, ReasonMalformed, r, "a genesis head with an index")
	_, r = headPub(ChainHead{HeadResult: []byte("junk")})
	require.Equal(t, ReasonMalformed, r)
	p, r := headPub(GenesisHead(newID(), newID()))
	require.Equal(t, ReasonOK, r)
	require.Equal(t, genesisPub(), p)

	// A non-pending head never keeps a fix (A8).
	h := retainFix(ChainHead{Fix: []byte("old")}, []byte("new"), Status{})
	require.Nil(t, h.Fix)
}
