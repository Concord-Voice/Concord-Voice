package accountguard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The golden tests below are what pin both domain prefix strings and the byte layout.
//
// Golden digests. They were computed out-of-band by an independent implementation (a
// short Python script over hashlib, hmac and struct that writes the encodings out byte
// by byte), never by running the code under test, so they pin the byte layout and the
// domain prefixes themselves. Each digest below is the hash of the exact input bytes
// held, as hex, in the matching golden...InputHex constant. To reproduce one without
// this repo's code:
//
//	printf '<input hex>' | xxd -r -p | shasum -a 256
//
// and, for the HMAC digests, with the fingerprint-label derivation of ringInputAlpha
// (alphaKnownAnswers[1] in keys_test.go) as the hex HMAC key:
//
//	printf '<input hex>' | xxd -r -p | openssl dgst -sha256 -mac HMAC -macopt hexkey:<that hex>
//
// TestGoldenInputsReproduceTheGoldens re-runs both so the constants cannot drift apart.
const (
	// goldenOriginHex is the origin fingerprint of originGoldenRefs: SHA-256 of goldenOriginInputHex.
	goldenOriginHex = "5d3944877fae6dee49545dde2d7bae685a7c242374c8fde50e08ea18f3964421" // pragma: allowlist secret
	// goldenOriginEmptyHex is the origin fingerprint of no refs (the bare domain prefix): SHA-256 of goldenOriginEmptyInputHex.
	goldenOriginEmptyHex = "67bece55b2d21c0fc9368ca01a330a0ff8b157a31b118a58078855e0086c1fac" // pragma: allowlist secret
	// goldenRequestSHAHex and goldenRequestHMACHex are the two digests of the request
	// over requestGoldenTargets (kind remove, reset true) under ringInputAlpha: SHA-256 and HMAC-SHA-256 of goldenRequestInputHex.
	goldenRequestSHAHex  = "2a7e06afcb53913ec652cb7a17e57087d991bfaf8c4090a9f660bce13a4957ef" // pragma: allowlist secret
	goldenRequestHMACHex = "7c193bad931ad50501bf4a8c57e4dd2e2c3660edd4a85d16ad16875ce56c2c0a" // pragma: allowlist secret
	// The same two digests for a break-glass request with no targets and no reset, over goldenEmptyInputHex.
	goldenEmptySHAHex  = "f4fd4bfe79cd2e90a55a64c9ea55d1085af8c17353b631f91690bd8d410c81f9" // pragma: allowlist secret
	goldenEmptyHMACHex = "4bf6b2594ffa10925a3a9596a2348714a4ac7db7e6f69842b389b40e2f42d071" // pragma: allowlist secret
)

// The exact bytes each golden digest hashes, hex encoded: the domain prefix, the kind,
// the count for a request, each ref as kind || uint16 ID length || ID || int64 version,
// and the reset byte for a request.
const (
	goldenOriginInputHex = "" +
		"636f6e636f72642f6163636f756e7467756172642f6f726967696e2f76310001" + // pragma: allowlist secret
		"0006637265642d610000000000000003060006637265642d6201020304050607" + // pragma: allowlist secret
		"08" // pragma: allowlist secret

	goldenOriginEmptyInputHex = "" +
		"636f6e636f72642f6163636f756e7467756172642f6f726967696e2f763100" // pragma: allowlist secret

	goldenRequestInputHex = "" +
		"636f6e636f72642f6163636f756e7467756172642f726571756573742f763100" + // pragma: allowlist secret
		"0100050100016100000000000000020100016100000000000000090100026161" + // pragma: allowlist secret
		"0000000000000001010001620000000000000005060001610000000000000001" + // pragma: allowlist secret
		"01" // pragma: allowlist secret

	goldenEmptyInputHex = "" +
		"636f6e636f72642f6163636f756e7467756172642f726571756573742f763100" + // pragma: allowlist secret
		"03000000" // pragma: allowlist secret
)

func TestGoldenInputsReproduceTheGoldens(t *testing.T) {
	sha := func(inputHex string) string {
		in, err := hex.DecodeString(inputHex)
		require.NoError(t, err)
		sum := sha256.Sum256(in)
		return hex.EncodeToString(sum[:])
	}
	assert.Equal(t, goldenOriginHex, sha(goldenOriginInputHex))
	assert.Equal(t, goldenOriginEmptyHex, sha(goldenOriginEmptyInputHex))
	assert.Equal(t, goldenRequestSHAHex, sha(goldenRequestInputHex))
	assert.Equal(t, goldenEmptySHAHex, sha(goldenEmptyInputHex))

	mac := func(inputHex string) string {
		in, err := hex.DecodeString(inputHex)
		require.NoError(t, err)
		fp, err := DeriveKey(ringInputAlpha, LabelFingerprint)
		require.NoError(t, err)
		h := hmac.New(sha256.New, fp)
		h.Write(in)
		return hex.EncodeToString(h.Sum(nil))
	}
	assert.Equal(t, goldenRequestHMACHex, mac(goldenRequestInputHex))
	assert.Equal(t, goldenEmptyHMACHex, mac(goldenEmptyInputHex))
}

// maxEncodable is the largest ID length, and the largest target count, a uint16 prefix
// can carry.
const maxEncodable = 0xFFFF

// originGoldenRefs is the fixed two-ref input of TestOriginFingerprint_Golden. The second
// version uses every byte of the int64 so a narrower or little-endian encoding differs.
func originGoldenRefs() []decide.CredRef {
	return []decide.CredRef{
		{Kind: decide.CredPassword, ID: "cred-a", Version: 3},
		{Kind: decide.CredTOTP, ID: "cred-b", Version: 0x0102030405060708},
	}
}

// requestGoldenTargets is deliberately NOT in canonical order and exercises every sort
// key: kind (TOTP after password), ID byte-wise ("a" < "aa" < "b", which a length-first
// sort would break) and version (2 < 9). The canonical order is password/a/2,
// password/a/9, password/aa/1, password/b/5, totp/a/1.
func requestGoldenTargets() []decide.CredRef {
	return []decide.CredRef{
		{Kind: decide.CredTOTP, ID: "a", Version: 1},
		{Kind: decide.CredPassword, ID: "b", Version: 5},
		{Kind: decide.CredPassword, ID: "a", Version: 9},
		{Kind: decide.CredPassword, ID: "a", Version: 2},
		{Kind: decide.CredPassword, ID: "aa", Version: 1},
	}
}

// mustOrigin fingerprints refs or fails the test.
func mustOrigin(t *testing.T, refs []decide.CredRef) [32]byte {
	t.Helper()
	fp, err := OriginFingerprint(refs)
	require.NoError(t, err)
	return fp
}

// mustCandidate builds a candidate on a working ring or fails the test.
func mustCandidate(t *testing.T, ring *KeyRing, kind decide.ReqKind, targets []decide.CredRef, reset bool) decide.Candidate {
	t.Helper()
	c, err := NewCandidate(context.Background(), ring, kind, targets, reset)
	require.NoError(t, err)
	return c
}

func assertZeroCandidate(t *testing.T, c decide.Candidate) {
	t.Helper()
	assert.Nil(t, c.Keyed, "a refused candidate carries no HMAC")
	assert.Nil(t, c.Coarse, "a refused candidate carries no digest")
	assert.Zero(t, c.KeyID)
}

// ---------------------------------------------------------------------------
// OriginFingerprint
// ---------------------------------------------------------------------------

func TestOriginFingerprint_Golden(t *testing.T) {
	t.Run("two refs", func(t *testing.T) {
		fp := mustOrigin(t, originGoldenRefs())
		assert.Equal(t, goldenOriginHex, hex.EncodeToString(fp[:]))
	})

	t.Run("no refs hashes the bare domain prefix", func(t *testing.T) {
		for name, refs := range map[string][]decide.CredRef{"nil": nil, "empty": {}} {
			fp := mustOrigin(t, refs)
			assert.Equal(t, goldenOriginEmptyHex, hex.EncodeToString(fp[:]), name)
		}
	})
}

func TestOriginFingerprint_Deterministic(t *testing.T) {
	assert.Equal(t, mustOrigin(t, originGoldenRefs()), mustOrigin(t, originGoldenRefs()))
}

func TestOriginFingerprint_VersionMatters(t *testing.T) {
	v2 := mustOrigin(t, []decide.CredRef{{Kind: decide.CredPassword, ID: "cred-a", Version: 2}})
	v3 := mustOrigin(t, []decide.CredRef{{Kind: decide.CredPassword, ID: "cred-a", Version: 3}})
	assert.NotEqual(t, v2, v3)
}

func TestOriginFingerprint_KindAndIDMatter(t *testing.T) {
	base := mustOrigin(t, []decide.CredRef{{Kind: decide.CredPassword, ID: "cred-a", Version: 1}})

	otherKind := mustOrigin(t, []decide.CredRef{{Kind: decide.CredTOTP, ID: "cred-a", Version: 1}})
	assert.NotEqual(t, base, otherKind, "the kind is part of the identity")

	otherID := mustOrigin(t, []decide.CredRef{{Kind: decide.CredPassword, ID: "cred-b", Version: 1}})
	assert.NotEqual(t, base, otherID, "the ID is part of the identity")
}

// Without the ID length prefix these two inputs encode to the same bytes: the second
// ref of the split form is hidden inside the single ID of the joined form, supplied
// byte for byte (the first ref's version, the kind, the second ID). Only the prefix
// separates them, so this fails when the prefix is dropped.
func TestOriginFingerprint_LengthPrefixPreventsSplice(t *testing.T) {
	const k = decide.CredPassword
	split := mustOrigin(t, []decide.CredRef{
		{Kind: k, ID: "x", Version: 1},
		{Kind: k, ID: "y", Version: 2},
	})
	joined := mustOrigin(t, []decide.CredRef{
		{Kind: k, ID: spliceID("x", 1, k, "y"), Version: 2},
	})
	assert.NotEqual(t, split, joined, "an ID that spells out a second ref must not collide with that ref")
}

// The runner hashes refs in the order it is given. decide emits canonical order, so
// re-sorting here would be redundant at best; this pins that it does not happen.
func TestOriginFingerprint_HashesInTheOrderGiven(t *testing.T) {
	a := decide.CredRef{Kind: decide.CredPassword, ID: "cred-a", Version: 1}
	b := decide.CredRef{Kind: decide.CredTOTP, ID: "cred-b", Version: 1}

	assert.NotEqual(t, mustOrigin(t, []decide.CredRef{a, b}), mustOrigin(t, []decide.CredRef{b, a}))
}

func TestOriginFingerprint_DoesNotTouchItsInput(t *testing.T) {
	refs := originGoldenRefs()
	want := originGoldenRefs()
	_ = mustOrigin(t, refs)
	assert.Equal(t, want, refs)
}

func TestOriginFingerprint_IDLengthBound(t *testing.T) {
	t.Run("an ID of exactly 0xFFFF bytes is encodable", func(t *testing.T) {
		fp, err := OriginFingerprint([]decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("x", maxEncodable)}})
		require.NoError(t, err)
		assert.NotEqual(t, [32]byte{}, fp)
	})

	t.Run("an ID of 0x10000 bytes is refused, never truncated", func(t *testing.T) {
		fp, err := OriginFingerprint([]decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("x", maxEncodable+1)}})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEncodingTooLong)
		assert.Equal(t, [32]byte{}, fp, "a refused fingerprint is the zero value")
	})

	t.Run("one long ID among short ones still refuses the whole input", func(t *testing.T) {
		_, err := OriginFingerprint([]decide.CredRef{
			{Kind: decide.CredPassword, ID: "short"},
			{Kind: decide.CredTOTP, ID: strings.Repeat("x", maxEncodable+1)},
		})
		assert.ErrorIs(t, err, ErrEncodingTooLong)
	})
}

// ---------------------------------------------------------------------------
// NewCandidate
// ---------------------------------------------------------------------------

func TestNewCandidate_Golden(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)

	t.Run("five unsorted targets", func(t *testing.T) {
		c := mustCandidate(t, ring, decide.ReqCredentialRemove, requestGoldenTargets(), true)
		assert.Equal(t, goldenRequestSHAHex, hex.EncodeToString(c.Coarse))
		assert.Equal(t, goldenRequestHMACHex, hex.EncodeToString(c.Keyed))
	})

	t.Run("no targets", func(t *testing.T) {
		for name, targets := range map[string][]decide.CredRef{"nil": nil, "empty": {}} {
			c := mustCandidate(t, ring, decide.ReqBreakGlass, targets, false)
			assert.Equal(t, goldenEmptySHAHex, hex.EncodeToString(c.Coarse), name)
			assert.Equal(t, goldenEmptyHMACHex, hex.EncodeToString(c.Keyed), name)
		}
	})
}

func TestNewCandidate_ShapeAndKeyID(t *testing.T) {
	db := ringDB(t)
	ring := NewKeyRing(db, ringInputAlpha)

	c := mustCandidate(t, ring, decide.ReqAddressChange, requestGoldenTargets(), false)

	assert.Len(t, c.Coarse, 32)
	assert.Len(t, c.Keyed, 32)
	assert.NotEqual(t, c.Coarse, c.Keyed, "the keyed digest must not collapse to the unkeyed one")

	want, err := ring.CurrentKeyID(context.Background())
	require.NoError(t, err)
	assert.Positive(t, want)
	assert.Equal(t, want, c.KeyID, "KeyID names the key the HMAC was made under")
}

func TestNewCandidate_TargetOrderIrrelevant(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)
	a := decide.CredRef{Kind: decide.CredPassword, ID: "a", Version: 1}
	b := decide.CredRef{Kind: decide.CredPassword, ID: "b", Version: 1}
	c := decide.CredRef{Kind: decide.CredTOTP, ID: "a", Version: 1}

	want := mustCandidate(t, ring, decide.ReqCredentialRemove, []decide.CredRef{a, b, c}, false)
	for i, perm := range [][]decide.CredRef{{a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}} {
		got := mustCandidate(t, ring, decide.ReqCredentialRemove, perm, false)
		assert.Equal(t, want, got, "permutation %d", i)
	}
}

// Sorting works on a copy: the caller's slice (which may be the very slice it goes on
// to store as the request's targets) keeps its order.
func TestNewCandidate_LeavesTheCallersSliceUnchanged(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)
	targets := requestGoldenTargets()
	want := requestGoldenTargets()

	_ = mustCandidate(t, ring, decide.ReqCredentialRemove, targets, true)

	assert.Equal(t, want, targets)
}

func TestNewCandidate_ResetFlagSeparates(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)
	targets := requestGoldenTargets()

	off := mustCandidate(t, ring, decide.ReqBreakGlass, targets, false)
	on := mustCandidate(t, ring, decide.ReqBreakGlass, targets, true)

	assert.NotEqual(t, off.Coarse, on.Coarse)
	assert.NotEqual(t, off.Keyed, on.Keyed)
}

func TestNewCandidate_EveryInputSeparates(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)
	base := []decide.CredRef{{Kind: decide.CredPassword, ID: "a", Version: 1}}
	ref := mustCandidate(t, ring, decide.ReqCredentialRemove, base, false)

	variants := map[string]decide.Candidate{
		"request kind": mustCandidate(t, ring, decide.ReqAddressChange, base, false),
		"target kind": mustCandidate(t, ring, decide.ReqCredentialRemove,
			[]decide.CredRef{{Kind: decide.CredTOTP, ID: "a", Version: 1}}, false),
		"target ID": mustCandidate(t, ring, decide.ReqCredentialRemove,
			[]decide.CredRef{{Kind: decide.CredPassword, ID: "b", Version: 1}}, false),
		"target version": mustCandidate(t, ring, decide.ReqCredentialRemove,
			[]decide.CredRef{{Kind: decide.CredPassword, ID: "a", Version: 2}}, false),
		"extra target": mustCandidate(t, ring, decide.ReqCredentialRemove,
			[]decide.CredRef{{Kind: decide.CredPassword, ID: "a", Version: 1}, {Kind: decide.CredPassword, ID: "b", Version: 1}}, false),
		"no target": mustCandidate(t, ring, decide.ReqCredentialRemove, nil, false),
	}
	for name, v := range variants {
		assert.NotEqual(t, ref.Coarse, v.Coarse, "%s must change the coarse digest", name)
		assert.NotEqual(t, ref.Keyed, v.Keyed, "%s must change the keyed digest", name)
	}
}

// The request encoding carries a count, so both sides keep two targets, each in
// canonical order ("a..." sorts before "b..." and before "c"). Without the ID length
// prefix both sides encode to the same bytes: the boundary between the two refs sits
// inside an ID on one side and between the IDs on the other. Only the prefix separates
// them, so this fails when the prefix is dropped.
func TestNewCandidate_LengthPrefixPreventsSplice(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)
	const k = decide.CredPassword
	hideSecond := mustCandidate(t, ring, decide.ReqCredentialRemove, []decide.CredRef{
		{Kind: k, ID: "a", Version: 1},
		{Kind: k, ID: spliceID("b", 2, k, "c"), Version: 3},
	}, false)
	hideFirst := mustCandidate(t, ring, decide.ReqCredentialRemove, []decide.CredRef{
		{Kind: k, ID: spliceID("a", 1, k, "b"), Version: 2},
		{Kind: k, ID: "c", Version: 3},
	}, false)

	assert.NotEqual(t, hideSecond.Coarse, hideFirst.Coarse)
	assert.NotEqual(t, hideSecond.Keyed, hideFirst.Keyed)
}

// spliceID returns first || BE64(firstVersion) || byte(kind) || second: the bytes that
// follow an ID's start when a ref with that ID is followed by a ref with the second ID,
// less the second ref's own version.
func spliceID(first string, firstVersion uint64, kind decide.CredKind, second string) string {
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], firstVersion)
	return first + string(v[:]) + string([]byte{byte(kind)}) + second
}

// Only the HMAC depends on the input secret: the coarse digest is unkeyed, so two
// deployments (or one deployment across a rotation) agree on it, while the keyed digest
// and the key id that names it differ.
func TestNewCandidate_KeyedChangesWithSecretCoarseDoesNot(t *testing.T) {
	db := ringDB(t)
	targets := requestGoldenTargets()

	alpha := mustCandidate(t, NewKeyRing(db, ringInputAlpha), decide.ReqCredentialRemove, targets, true)
	beta := mustCandidate(t, NewKeyRing(db, ringInputBeta), decide.ReqCredentialRemove, targets, true)

	assert.Equal(t, alpha.Coarse, beta.Coarse, "the coarse fingerprint must not depend on the secret")
	assert.NotEqual(t, alpha.Keyed, beta.Keyed, "the keyed fingerprint must depend on the secret")
	assert.NotEqual(t, alpha.KeyID, beta.KeyID, "a rotated secret is a different registry id")
}

func TestNewCandidate_DuplicateTargetsRefused(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)
	dup := decide.CredRef{Kind: decide.CredPassword, ID: "a", Version: 1}

	t.Run("an exact duplicate is refused", func(t *testing.T) {
		c, err := NewCandidate(context.Background(), ring, decide.ReqCredentialRemove, []decide.CredRef{dup, dup}, false)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrDuplicateTarget)
		assertZeroCandidate(t, c)
	})

	t.Run("a duplicate that is not adjacent in the input is refused", func(t *testing.T) {
		other := decide.CredRef{Kind: decide.CredTOTP, ID: "z", Version: 1}
		c, err := NewCandidate(context.Background(), ring, decide.ReqCredentialRemove, []decide.CredRef{dup, other, dup}, false)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrDuplicateTarget)
		assertZeroCandidate(t, c)
	})

	t.Run("the same slot at another version is not a duplicate", func(t *testing.T) {
		v2 := dup
		v2.Version = 2
		_, err := NewCandidate(context.Background(), ring, decide.ReqCredentialRemove, []decide.CredRef{dup, v2}, false)
		assert.NoError(t, err)
	})

	t.Run("the same ID under another kind is not a duplicate", func(t *testing.T) {
		other := dup
		other.Kind = decide.CredTOTP
		_, err := NewCandidate(context.Background(), ring, decide.ReqCredentialRemove, []decide.CredRef{dup, other}, false)
		assert.NoError(t, err)
	})
}

func TestNewCandidate_LengthBounds(t *testing.T) {
	ring := NewKeyRing(ringDB(t), ringInputAlpha)

	t.Run("a target ID of exactly 0xFFFF bytes is encodable", func(t *testing.T) {
		c := mustCandidate(t, ring, decide.ReqCredentialRemove,
			[]decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("x", maxEncodable)}}, false)
		assert.Len(t, c.Coarse, 32)
	})

	t.Run("a target ID of 0x10000 bytes is refused, never truncated", func(t *testing.T) {
		c, err := NewCandidate(context.Background(), ring, decide.ReqCredentialRemove,
			[]decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("x", maxEncodable+1)}}, false)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEncodingTooLong)
		assertZeroCandidate(t, c)
	})

	t.Run("exactly 0xFFFF targets are encodable", func(t *testing.T) {
		c := mustCandidate(t, ring, decide.ReqCredentialRemove, manyTargets(maxEncodable), false)
		assert.Len(t, c.Coarse, 32)
	})

	t.Run("0x10000 targets are refused, never wrapped", func(t *testing.T) {
		c, err := NewCandidate(context.Background(), ring, decide.ReqCredentialRemove, manyTargets(maxEncodable+1), false)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEncodingTooLong)
		assertZeroCandidate(t, c)
	})
}

// manyTargets returns n distinct targets.
func manyTargets(n int) []decide.CredRef {
	out := make([]decide.CredRef, n)
	for i := range out {
		out[i] = decide.CredRef{Kind: decide.CredPassword, ID: fmt.Sprintf("id-%06d", i), Version: 1}
	}
	return out
}

// A ring that cannot name its key refuses before it computes anything: the caller must
// not receive a coarse digest it could use unkeyed, and the key-ring fault is reported
// ahead of any fault in the input.
func TestNewCandidate_KeyRingDownRefuses(t *testing.T) {
	targets := requestGoldenTargets()

	ringDB(t) // migrate and empty the registry for the closed-pool case below
	dead := extraPool(t)
	require.NoError(t, dead.Close())
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := map[string]struct {
		ring *KeyRing
		ctx  context.Context
	}{
		"nil ring":          {nil, context.Background()},
		"zero ring":         {&KeyRing{}, context.Background()},
		"empty secret":      {NewKeyRing(nil, ""), context.Background()},
		"no database":       {NewKeyRing(nil, ringInputAlpha), context.Background()},
		"closed pool":       {NewKeyRing(dead, ringInputAlpha), context.Background()},
		"cancelled context": {NewKeyRing(extraPool(t), ringInputAlpha), cancelled},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := NewCandidate(tc.ctx, tc.ring, decide.ReqCredentialRemove, targets, true)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKeyUnavailable)
			assertZeroCandidate(t, c)
		})

		t.Run(name+" is reported ahead of an input fault", func(t *testing.T) {
			dup := decide.CredRef{Kind: decide.CredPassword, ID: "a", Version: 1}
			c, err := NewCandidate(tc.ctx, tc.ring, decide.ReqCredentialRemove, []decide.CredRef{dup, dup}, false)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKeyUnavailable)
			assert.NotErrorIs(t, err, ErrDuplicateTarget)
			assertZeroCandidate(t, c)
		})
	}
}

// compareRefs here and decide.refLess are two copies of one order (kind, then ID byte for
// byte, then version) that must never drift: a request fingerprint sorted one way and a
// stored target list sorted the other would disagree on the same request. Both are held
// to the same fixture, testdata/ref_order.json, which lists refs in the expected order
// (decide/ref_order_test.go holds refLess to it).
func TestCompareRefs_MatchesTheSharedOrderFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/ref_order.json")
	require.NoError(t, err)
	var want []decide.CredRef
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Greater(t, len(want), 1)

	for i := 1; i < len(want); i++ {
		assert.Negative(t, compareRefs(want[i-1], want[i]), "fixture entry %d must sort strictly after entry %d", i, i-1)
		assert.Positive(t, compareRefs(want[i], want[i-1]), "fixture entries %d and %d", i-1, i)
	}

	reversed := slices.Clone(want)
	slices.Reverse(reversed)
	rotated := append(slices.Clone(want[3:]), want[:3]...)
	for name, shuffled := range map[string][]decide.CredRef{"reversed": reversed, "rotated": rotated} {
		slices.SortFunc(shuffled, compareRefs)
		assert.Equal(t, want, shuffled, name)
	}
}
