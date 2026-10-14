package authoritycontract

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// EN3 orders an a<b48> over the base64 TEXT: 0x00… encodes "AAAA…" and
// 0xFC… encodes "/…", so the text order reverses the raw-byte order.
func TestInsertB48OrdersByText(t *testing.T) {
	lo, hi := bytes.Repeat([]byte{0x00}, 48), bytes.Repeat([]byte{0xFC}, 48)
	xs := insertB48(insertB48(nil, lo), hi)
	require.Equal(t, [][]byte{hi, lo}, xs)
	require.Len(t, insertB48(xs, lo), 2, "a duplicate is a no-op")
}

// E3-08: a head over HEAD_BLOB_MAX is refused, never returned. 130 100
// seq_index records of 129 characters encode above 16 777 216 bytes.
func TestVerifyChainRefusesAHeadOverHeadBlobMax(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil) // the status is signed under (gen 0, serial 1)
	tr := r.trust(t)
	pk, err := r.RSK.PublicKey.ECDH()
	require.NoError(t, err)
	root := pk.Bytes()
	const n = 130_100
	account := newID()
	res := Result{RealmID: r.ID, AccountID: account, RequestID: newID(), IntentDigest: Digest([]byte("i")),
		CPAuthzDigest: Digest([]byte("a")), Op: OpRotateEK, Seq: n, State: StateActive, SuspendedBy: []string{},
		ActiveRoot: root, EKBindingDigest: Digest([]byte("e")), PrevEntryDigest: Digest([]byte("p")),
		Assurance: AssuranceInitial, IssuedAtMs: testT0, DelegationSerial: 1}
	rb := res.Encode()
	require.NotNil(t, rb)
	h := GenesisHead(r.ID, account)
	h.HeadResult = rb
	h.SeqIndex = make([]string, n)
	for i := range h.SeqIndex {
		d := Digest([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		h.SeqIndex[i] = SeqRecord(d, d)
	}
	st := r.status(t, func(s *Status) {
		s.AccountID, s.Seq, s.HeadEntryDigest, s.State = account, n, EntryDigest(rb), StateActive
		s.ActiveRootDigest, s.EKBindingDigest = Digest(root), res.EKBindingDigest
	})
	_, _, _, got := VerifyChain(tr, HighWater{}, h, ChainInput{Status: st},
		AccountMemory{}, time.UnixMilli(int64(testT0)+1_000), UseNewSecret)
	require.Equal(t, ReasonMalformed, got)
}

// vectorNamed returns the committed vector called name, which must be a
// bundle-verify vector expected to verify.
func vectorNamed(t *testing.T, name string) vector {
	t.Helper()
	for _, v := range loadVectors(t).Vectors {
		if v.Name == name {
			require.Equal(t, "bundle-verify", v.Type)
			require.Equal(t, string(ReasonOK), v.Expect.Reason)
			return v
		}
	}
	require.Failf(t, "vector not found", "no vector named %q", name)
	return vector{}
}

// ekFiller returns n distinct, ascending 48-byte ek_index entries. Each is the
// 64-character text "<8 decimal digits>AAAA…", whose fixed-width counter makes
// the base64-TEXT order (EN3) the numeric order, so a prefix of the pool is
// itself a valid sorted array and no sorting is needed.
func ekFiller(t *testing.T, n int) [][]byte {
	t.Helper()
	xs := make([][]byte, n)
	for i := range xs {
		b, err := base64.StdEncoding.DecodeString(fmt.Sprintf("%08d", i) + strings.Repeat("A", 56))
		require.NoError(t, err)
		xs[i] = b
	}
	return xs
}

// E3-08 on VerifyBundle's path: a bundle whose appended result would make the
// persisted head exceed HEAD_BLOB_MAX is malformed, and the previous head is
// returned unchanged. The previous head is padded with ek_index filler rather
// than seq_index because headPub ties len(seq_index) to the head result's seq,
// which the bundle fixes; ek_index carries no such tie, and no filler can
// collide with the EK the bundle introduces. Both sides of the boundary are
// measured: the largest padding whose resulting head still fits is accepted,
// and one more entry (the previous head itself still within the cap) is refused.
func TestVerifyBundleRefusesAHeadOverHeadBlobMax(t *testing.T) {
	v := vectorNamed(t, "row/07/suspend-user")
	require.Empty(t, v.Fix, "the padding below bypasses the fix wiring of checkBundleVerifyVector")
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	hw, bundle, prev0 := hwOf(v.HW), mustB64(t, v.BytesB64), prevHeadOf(t, v)

	// Control: the unpadded vector verifies, and the step appends to the head.
	after0, got := VerifyBundle(tr, hw, prev0, bundle, nil)
	require.Equal(t, ReasonOK, got)
	base, growth := len(prev0.Bytes()), len(after0.Bytes())-len(prev0.Bytes())
	require.Positive(t, growth, "the chosen step must append to the head")

	// Measure what one filler entry adds, then size the padding from it.
	var pool [][]byte // ascending filler; a prefix is itself a sorted ek_index
	padded := func(n int) ChainHead {
		p := prev0
		p.EKIndex = slices.Clone(pool[:n])
		for _, e := range prev0.EKIndex { // the few real entries, merged in text order
			p.EKIndex = insertB48(p.EKIndex, e)
		}
		return p
	}
	pool = ekFiller(t, 1)
	step := len(padded(1).Bytes()) - base
	require.Positive(t, step)
	require.LessOrEqual(t, step, growth, "one filler entry must be finer than the step's growth")
	fit := (HeadBlobMax - growth - base) / step // the largest padding whose after-head fits

	pool = ekFiller(t, fit+1)
	atCap, over := padded(fit), padded(fit+1)
	require.NotNil(t, atCap.Bytes())
	require.NotNil(t, over.Bytes(), "the previous head itself stays within HEAD_BLOB_MAX")
	require.LessOrEqual(t, len(atCap.Bytes())+growth, HeadBlobMax, "the appended head fits")
	require.LessOrEqual(t, len(over.Bytes()), HeadBlobMax)
	require.Greater(t, len(over.Bytes())+growth, HeadBlobMax, "the appended head would not fit")

	t.Run("a head at the cap is accepted", func(t *testing.T) {
		head, got := VerifyBundle(tr, hw, atCap, bundle, nil)
		require.Equal(t, ReasonOK, got)
		require.NotNil(t, head.Bytes())
		require.LessOrEqual(t, len(head.Bytes()), HeadBlobMax)
	})
	t.Run("a head over the cap is refused as malformed", func(t *testing.T) {
		head, got := VerifyBundle(tr, hw, over, bundle, nil)
		require.Equal(t, ReasonMalformed, got)
		require.Equal(t, over.Bytes(), head.Bytes(), "the previous head is returned unchanged")
	})
}
