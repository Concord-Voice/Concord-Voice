package authoritycontract

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rebundle decodes a committed bundle, applies mut and re-encodes it.
func rebundle(t *testing.T, b []byte, mut func(*Bundle)) []byte {
	t.Helper()
	d, err := DecodeBundle(b)
	require.NoError(t, err)
	mut(&d)
	out := d.Encode()
	require.NotNil(t, out, "the mutated bundle still encodes")
	return out
}

// flipped returns a copy of sig with one bit of its r half flipped: still 96
// bytes, no longer a valid signature.
func flipped(sig []byte) []byte {
	out := append([]byte(nil), sig...)
	out[10] ^= 1
	return out
}

// chainInputs is a committed chain vector's inputs.
type chainInputs struct {
	tr     *Trust
	hw     HighWater
	prev   ChainHead
	status []byte
	mem    AccountMemory
	now    time.Time
	use    Use
}

func chainInputsOf(t *testing.T, v vector) chainInputs {
	t.Helper()
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	use, ok := ParseUse(v.Use)
	require.True(t, ok)
	return chainInputs{tr: tr, hw: hwOf(v.HW), prev: prevHeadOf(t, v), status: mustB64(t, v.BytesB64), mem: memOf(t, v.Mem),
		now: nowOf(t, v.NowMs), use: use}
}

func (c chainInputs) chain(tr *Trust, bundles [][]byte, status []byte, mem AccountMemory) (ChainHead, VerifiedStatus, HighWater, Reason) {
	return VerifyChain(tr, c.hw, c.prev, ChainInput{Bundles: bundles, Status: status}, mem, c.now, c.use)
}

// T4 halts realm-wide (DoR §2.12: trust_halt is "halt (realm-wide
// refuse-op)"), so neither VerifyBundle nor VerifyChain admits anything under
// a halted trust, and the halt outranks the bundle's own Reason. checkT7 does
// not check halt (its callers do), so here each entry point's own haltReason
// check is the only thing that can return trust_halt. The halt is T4's "no
// valid delegation for the current generation" over a retained set of the
// realm that signed the bundle (a succession to gen 1, no gen-1 delegation).
func TestVerifyBundleAndChainRefuseOnAHaltedTrust(t *testing.T) {
	cv := vectorOf(t, "trust/t7/succession-serial-reset-covered", "chain")
	hv := vectorOf(t, "trust/t4/no-delegation-for-current-generation", "chain")
	require.Equal(t, cv.Anchors, hv.Anchors, "one realm signed both")
	require.Len(t, cv.Bundles, 1)
	c := chainInputsOf(t, cv)
	halted, r := trustOf(t, hv)
	require.Equal(t, ReasonOK, r)
	require.NotNil(t, halted.Halted(), "the retained set halts (T4)")
	require.Nil(t, c.tr.Halted())
	bundle := mustB64(t, cv.Bundles[0])
	broken := rebundle(t, bundle, func(b *Bundle) { b.ResultSig = flipped(b.ResultSig) })

	// Controls under the live trust: the chain verifies, and the broken bundle
	// fails on a Reason of its own.
	_, _, _, r = c.chain(c.tr, [][]byte{bundle}, c.status, c.mem)
	require.Equal(t, ReasonOK, r)
	_, _, _, r = c.chain(c.tr, [][]byte{broken}, c.status, c.mem)
	require.Equal(t, ReasonBadSignature, r)
	_, r = VerifyBundle(c.tr, c.hw, c.prev, broken, nil)
	require.Equal(t, ReasonBadSignature, r)

	for name, b := range map[string][]byte{"a bundle that verifies": bundle, "a bundle that fails on its own": broken} {
		t.Run(name, func(t *testing.T) {
			head, r := VerifyBundle(halted, c.hw, c.prev, b, nil)
			require.Equal(t, ReasonTrustHalt, r, "VerifyBundle")
			require.Equal(t, c.prev.Bytes(), head.Bytes())
			head, vs, hw2, r := c.chain(halted, [][]byte{b}, c.status, c.mem)
			require.Equal(t, ReasonTrustHalt, r, "VerifyChain")
			require.Equal(t, ReasonTrustHalt, vs.Reason)
			require.Equal(t, c.hw, hw2)
			require.Equal(t, c.prev.Bytes(), head.Bytes())
		})
	}

	// AddSigned records a T4 contradiction in the trust; a bundle that would
	// verify is refused under it rather than admitted (rev15 found VerifyBundle
	// failing open here when its halt check was dropped).
	t.Run("a recorded T4 contradiction", func(t *testing.T) {
		v := vectorNamed(t, "row/06/rotate-ek")
		tr, r := trustOf(t, v)
		require.Equal(t, ReasonOK, r)
		prev, b := prevHeadOf(t, v), mustB64(t, v.BytesB64)
		_, r = VerifyBundle(tr, hwOf(v.HW), prev, b, nil)
		require.Equal(t, ReasonOK, r, "control")
		tr.halt = &HaltError{Rule: "T4: two delegations at one (gen, serial)"}
		head, r := VerifyBundle(tr, hwOf(v.HW), prev, b, nil)
		require.Equal(t, ReasonTrustHalt, r)
		require.Equal(t, prev.Bytes(), head.Bytes())
	})
}

// T7 (DoR §2.12): VerifyBundle returns superseded_uncovered for a superseded
// entry; only VerifyChain, with a status that passes T6 and the history rule,
// accepts one. The entry here is signed under (0, 1) of a retired generation.
func TestVerifyBundleRefusesASupersededEntry(t *testing.T) {
	cv := vectorOf(t, "trust/t7/succession-serial-reset-covered", "chain")
	c := chainInputsOf(t, cv)
	bundle := mustB64(t, cv.Bundles[0])
	head, r := VerifyBundle(c.tr, c.hw, c.prev, bundle, nil)
	require.Equal(t, ReasonSupersededUncovered, r)
	require.Equal(t, c.prev.Bytes(), head.Bytes(), "prev is returned unchanged")
	_, _, _, r = c.chain(c.tr, [][]byte{bundle}, c.status, c.mem)
	require.Equal(t, ReasonOK, r, "covered by the status")
}

// A present candidate signature must verify (DoR §2.12 bad_signature; plan A3:
// it is hypothesis-independent and checked before R3). Nothing else reads
// sig_candidate, and the RSK signature covers the result alone.
func TestVerifyBundleChecksTheCandidateSignature(t *testing.T) {
	v := vectorNamed(t, "row/05/rotate-root")
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	prev, bundle := prevHeadOf(t, v), mustB64(t, v.BytesB64)
	b, err := DecodeBundle(bundle)
	require.NoError(t, err)
	require.NotEmpty(t, b.SigCandidate, "rotate_root carries a candidate signature")
	_, r = VerifyBundle(tr, hwOf(v.HW), prev, bundle, nil)
	require.Equal(t, ReasonOK, r, "control")
	bad := rebundle(t, bundle, func(b *Bundle) { b.SigCandidate = flipped(b.SigCandidate) })
	head, r := VerifyBundle(tr, hwOf(v.HW), prev, bad, nil)
	require.Equal(t, ReasonBadSignature, r)
	require.Equal(t, prev.Bytes(), head.Bytes())
}

// §2.9's restore positions (LD-46): only actor restore carries an unsigned
// cp-authz, so any other actor's empty cp_authz_sig is malformed at parse,
// never a failed CAK check (cak_unlisted).
func TestVerifyBundleRefusesAnUnsignedCPAuthz(t *testing.T) {
	v := vectorNamed(t, "row/06/rotate-ek")
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	prev, bundle := prevHeadOf(t, v), mustB64(t, v.BytesB64)
	unsigned := rebundle(t, bundle, func(b *Bundle) { b.CPAuthzSig = nil })
	head, r := VerifyBundle(tr, hwOf(v.HW), prev, unsigned, nil)
	require.Equal(t, ReasonMalformed, r)
	require.Equal(t, prev.Bytes(), head.Bytes())
}

// T7's order (DoR §2.10): status signature, then chain, then head equality.
// A status that fails to decode or fails T6 is reported before any bundle is
// judged, so its pin never becomes a chain's seq-0 hypothesis.
func TestVerifyChainChecksTheStatusSignatureFirst(t *testing.T) {
	cv := vectorOf(t, "row/02/pop-pending", "chain")
	c := chainInputsOf(t, cv)
	bundles := listB64(t, cv.Bundles)
	sg := mustDecodeSigned(t, c.status)
	sg.SigA = flipped(sg.SigA)
	forged := sg.Encode()
	require.NotNil(t, forged)
	other := AccountMemory{Pin: d48("another first-sight pin")}

	_, _, _, r := c.chain(c.tr, bundles, c.status, other)
	require.Equal(t, ReasonTransitionInvalid, r, "control: under another first-sight pin the bundle fails")
	for name, tc := range map[string]struct {
		status []byte
		mem    AccountMemory
		want   Reason
	}{
		"a forged status before a failing bundle": {forged, other, ReasonBadSignature},
		"a forged status at first contact":        {forged, AccountMemory{}, ReasonBadSignature},
		"an undecodable status":                   {[]byte("not a status"), other, ReasonMalformed},
	} {
		head, _, _, r := c.chain(c.tr, bundles, tc.status, tc.mem)
		require.Equal(t, tc.want, r, name)
		require.Equal(t, c.prev.Bytes(), head.Bytes(), name)
	}
}

// §2.13 / D-149: a continuity change pending from absent is ok, and wraps to
// the pin SPKI (R6), only under the caller's first-sight pin; under another it
// is pin_changed and wraps to nothing. It is reached through VerifyStatus on a
// head an earlier call verified: VerifyChain from genesis takes the memory pin
// as its seq-0 hypothesis, so a mismatch there refuses the chain instead.
func TestVerifyStatusContinuityPendingComparesTheMemoryPin(t *testing.T) {
	cv := vectorOf(t, "row/02/pop-pending", "chain")
	c := chainInputsOf(t, cv)
	bundles := listB64(t, cv.Bundles)
	require.NotEmpty(t, c.mem.Pin)
	head, vs, _, r := c.chain(c.tr, bundles, c.status, c.mem)
	require.Equal(t, ReasonOK, r)
	require.True(t, vs.continuity)
	b, err := DecodeBundle(bundles[0])
	require.NoError(t, err)
	spki := b.LegacySPKI
	require.Equal(t, c.mem.Pin, Digest(spki), "the PoP SPKI is the pinned key")

	for name, tc := range map[string]struct {
		mem  AccountMemory
		want Reason
	}{
		"first sight":             {AccountMemory{}, ReasonOK},
		"the first-sight pin":     {c.mem, ReasonOK},
		"another first-sight pin": {AccountMemory{Pin: d48("another first-sight pin")}, ReasonPinChanged},
	} {
		vs, _, r := VerifyStatus(c.tr, c.hw, head, c.status, tc.mem, c.now, c.use)
		require.Equal(t, tc.want, r, name)
		got, ok := vs.WrapKey(spki)
		require.Equal(t, tc.want == ReasonOK, ok, name)
		if ok {
			require.Equal(t, spki, got, name)
		}
	}
}

// Plan A6: as_of < valid_until <= as_of + V_MAX, a violation malformed. The
// lower edge is strict: a status valid for no time at all is refused.
func TestVerifyStatusValidUntilBounds(t *testing.T) {
	w := newSeqZeroWorld(t)
	pin := d48("pin")
	cases := []struct {
		name string
		mut  func(*Status)
		want Reason
	}{
		{"valid_until below as_of", func(s *Status) { s.ValidUntilMs = s.AsOfMs - 1 }, ReasonMalformed},
		{"valid_until = as_of", func(s *Status) { s.ValidUntilMs = s.AsOfMs }, ReasonMalformed},
		{"valid_until = as_of + 1", func(s *Status) { s.ValidUntilMs = s.AsOfMs + 1 }, ReasonOK},
		{"valid_until = as_of + V_MAX", func(s *Status) { s.ValidUntilMs = s.AsOfMs + VMaxMs }, ReasonOK},
		{"valid_until = as_of + V_MAX + 1", func(s *Status) { s.ValidUntilMs = s.AsOfMs + VMaxMs + 1 }, ReasonMalformed},
	}
	for _, tc := range cases {
		_, _, r := VerifyStatus(w.tr, HighWater{}, w.head, w.status(t, pin, tc.mut), AccountMemory{}, w.now, UseNewSecret)
		require.Equal(t, tc.want, r, tc.name)
	}
}

// DoR §2.12 fork: a status at the head's own seq naming another
// head_entry_digest is a fork (head equality), not an R5 field_mismatch, as a
// status and as VerifyBundle's fix alike.
func TestVerifyStatusEqualSeqForkIsFork(t *testing.T) {
	w := newSeqZeroWorld(t)
	st := w.status(t, d48("pin"), func(s *Status) { s.HeadEntryDigest = d48("another head") })
	_, _, r := VerifyStatus(w.tr, HighWater{}, w.head, st, AccountMemory{}, w.now, UseNewSecret)
	require.Equal(t, ReasonFork, r)
	_, r = VerifyBundle(w.tr, HighWater{}, w.head, []byte("bundle never read"), &Fix{Status: st})
	require.Equal(t, ReasonFork, r)
}

// Plan A7: "before cutover" is judged on the signed status, and as_of >= the HW
// delegation's cutover_at_ms is absent_after_cutover, so as_of = cutover is
// already after it.
func TestVerifyStatusCutoverBoundary(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, func(d *Delegation) { d.CutoverAtMs = testT0 })
	tr := r.trust(t)
	acct := newID()
	head := GenesisHead(r.ID, acct)
	for _, tc := range []struct {
		name string
		asOf uint64
		want Reason
	}{
		{"as_of = cutover - 1", testT0 - 1, ReasonOK},
		{"as_of = cutover", testT0, ReasonAbsentAfterCutover},
		{"as_of = cutover + 1", testT0 + 1, ReasonAbsentAfterCutover},
	} {
		st := r.status(t, func(s *Status) {
			s.AccountID, s.LegacyEKDigest, s.AsOfMs, s.ValidUntilMs = acct, d48("pin"), tc.asOf, tc.asOf+1_000
		})
		_, _, got := VerifyStatus(tr, HighWater{}, head, st, AccountMemory{}, nowOf(t, testT0+1_000), UseNewSecret)
		require.Equal(t, tc.want, got, tc.name)
	}
}
