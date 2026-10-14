package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// vectorOf returns the committed vector called name, which must be of type typ.
func vectorOf(t *testing.T, name, typ string) vector {
	t.Helper()
	for _, v := range loadVectors(t).Vectors {
		if v.Name == name {
			require.Equal(t, typ, v.Type, name)
			return v
		}
	}
	require.Failf(t, "vector not found", "no vector named %q", name)
	return vector{}
}

// VerifyBundle takes no AccountMemory, so its genesis hypothesis is A0 with no
// pin (DoR §2.12, V3-05: "the seq-0 pin is AccountMemory.pin only"), and a
// verified genesis status passed as its fix keeps only its R4 meaning. This is
// the rev15 review's demonstration inverted: a status whose pin this caller's
// first sight refuses (pin_changed) used to make VerifyBundle accept a
// continuity enrolment under that pin, a pending head VerifyChain refuses.
func TestVerifyBundleGenesisFixSuppliesNoPin(t *testing.T) {
	bv := vectorOf(t, "pin/seq0/bundle-fix-supplies-no-pin", "bundle-verify")
	cv := vectorOf(t, "pin/seq0/chain-under-the-first-sight-pin", "chain")
	require.Equal(t, cv.PrevHead, bv.PrevHead, "both start from the same genesis head")
	require.Equal(t, []string{bv.BytesB64}, cv.Bundles, "both verify the same bundle")
	tr, r := trustOf(t, bv)
	require.Equal(t, ReasonOK, r)
	hw, genesis, bundle, fix := hwOf(bv.HW), prevHeadOf(t, bv), mustB64(t, bv.BytesB64), mustB64(t, bv.Fix)
	require.Empty(t, genesis.HeadResult, "the vector starts at genesis")
	st, err := fixStatus(fix)
	require.NoError(t, err)
	pin, other := st.LegacyEKDigest, d48("the first-sight pin this caller already holds")
	require.NotEmpty(t, pin)
	require.Equal(t, pin, memOf(t, cv.Mem).Pin, "the chain vector's memory holds the account's own pin")

	// The premise: the fix is an authentic genesis status whose pin this
	// caller's first sight refuses, and accepts under the account's own pin.
	asOf := nowOf(t, st.AsOfMs+1_000)
	_, _, r = VerifyStatus(tr, hw, genesis, fix, AccountMemory{Pin: other}, asOf, UseNewSecret)
	require.Equal(t, ReasonPinChanged, r)
	_, _, r = VerifyStatus(tr, hw, genesis, fix, AccountMemory{Pin: pin}, asOf, UseNewSecret)
	require.Equal(t, ReasonOK, r)

	t.Run("VerifyBundle refuses with or without the fix", func(t *testing.T) {
		for name, f := range map[string]*Fix{"the verified genesis status as fix": {Status: fix}, "no fix": nil} {
			head, r := VerifyBundle(tr, hw, genesis, bundle, f)
			require.Equal(t, ReasonTransitionInvalid, r, "%s: A0 admits no enroll_existing", name)
			require.Equal(t, genesis.Bytes(), head.Bytes(), "%s: prev is returned unchanged", name)
		}
	})

	t.Run("VerifyChain accepts the honest chain under the memory pin", func(t *testing.T) {
		status := mustB64(t, cv.BytesB64)
		head, vs, _, r := VerifyChain(tr, hw, genesis, ChainInput{Bundles: [][]byte{bundle}, Status: status},
			AccountMemory{Pin: pin}, nowOf(t, cv.NowMs), UseNewSecret)
		require.Equal(t, ReasonOK, r)
		require.True(t, vs.continuity, "a continuity change pending from absent")
		hp, r := headPub(head)
		require.Equal(t, ReasonOK, r)
		require.Equal(t, pin, hp.Pin)
		require.Equal(t, StatePending, hp.State)
		require.Equal(t, AssuranceContinuity, hp.Assurance)

		head, _, _, r = VerifyChain(tr, hw, genesis, ChainInput{Bundles: [][]byte{bundle}, Status: status},
			AccountMemory{Pin: other}, nowOf(t, cv.NowMs), UseNewSecret)
		require.Equal(t, ReasonTransitionInvalid, r, "under another first-sight pin the chain cannot start")
		require.Equal(t, genesis.Bytes(), head.Bytes())
	})
}
