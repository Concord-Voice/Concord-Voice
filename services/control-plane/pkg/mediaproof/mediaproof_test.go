package mediaproof_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
)

// Assembled from parts so the static credential detectors do not read a test
// fixture as a committed secret (same idiom as testdb.go).
var testSecret = strings.Join([]string{"unit", "test", "shared"}, "-")

const testVersion = "v1"

func nowStamp() string { return strconv.FormatInt(time.Now().Unix(), 10) }

func signedNow(t *testing.T, purpose string, fields ...string) (key []byte, ts, proof string) {
	t.Helper()
	key = mediaproof.DeriveKey(testSecret, purpose)
	require.NotEmpty(t, key)
	ts = nowStamp()
	proof = mediaproof.Sign(key, testVersion, ts, fields...)
	require.Len(t, proof, 64)
	return key, ts, proof
}

func TestVerifyAcceptsAFreshProof(t *testing.T) {
	key, ts, proof := signedNow(t, "ctx/a", "POST", "/p", mediaproof.TokenDigest("tok"))

	assert.True(t, mediaproof.Verify(key, proof, testVersion, ts,
		"POST", "/p", mediaproof.TokenDigest("tok")))
}

// Every field is signed, so altering any one must invalidate the proof. The
// version and timestamp are part of the canonical prefix the package prepends,
// so they are exercised here as arguments rather than as caller fields.
func TestVerifyRejectsEveryTamperedField(t *testing.T) {
	key, ts, proof := signedNow(t, "ctx/a", "POST", "/p", mediaproof.TokenDigest("tok"))

	assert.False(t, mediaproof.Verify(key, proof, testVersion, ts,
		"DELETE", "/p", mediaproof.TokenDigest("tok")), "method")
	assert.False(t, mediaproof.Verify(key, proof, testVersion, ts,
		"POST", "/other", mediaproof.TokenDigest("tok")), "path")
	assert.False(t, mediaproof.Verify(key, proof, testVersion, ts,
		"POST", "/p", mediaproof.TokenDigest("other")), "token")
	assert.False(t, mediaproof.Verify(key, proof, "v2", ts,
		"POST", "/p", mediaproof.TokenDigest("tok")), "version")
	assert.False(t, mediaproof.Verify(key, proof, testVersion, nowStamp()+"1",
		"POST", "/p", mediaproof.TokenDigest("tok")), "timestamp")
}

// Domain separation: the same fields signed under a different purpose must not
// verify. This is what stops the DM authorization proof and the service-hop
// proof being replayable as one another despite sharing the JWT secret.
func TestVerifyRejectsAProofFromAnotherPurpose(t *testing.T) {
	_, ts, proof := signedNow(t, "ctx/a", "POST", "/p", mediaproof.TokenDigest("tok"))
	other := mediaproof.DeriveKey(testSecret, "ctx/b")

	assert.False(t, mediaproof.Verify(other, proof, testVersion, ts,
		"POST", "/p", mediaproof.TokenDigest("tok")))
}

// The skew bound's WIDTH, not merely its existence. Without the inside-the-
// window cases, MaxClockSkew could be widened to hours and every test would
// still pass; without the outside cases it could be narrowed to zero.
func TestVerifyPinsTheSkewWindowWidth(t *testing.T) {
	key := mediaproof.DeriveKey(testSecret, "ctx/a")

	for name, tc := range map[string]struct {
		offset time.Duration
		accept bool
	}{
		"just inside, past":    {-(mediaproof.MaxClockSkew - 2*time.Second), true},
		"just inside, future":  {mediaproof.MaxClockSkew - 2*time.Second, true},
		"just outside, past":   {-(mediaproof.MaxClockSkew + 5*time.Second), false},
		"just outside, future": {mediaproof.MaxClockSkew + 5*time.Second, false},
	} {
		t.Run(name, func(t *testing.T) {
			ts := strconv.FormatInt(time.Now().Add(tc.offset).Unix(), 10)
			// Signed correctly — only the timestamp's distance varies, so a
			// wrong result means the bound itself is wrong.
			proof := mediaproof.Sign(key, testVersion, ts, "POST", "/p")
			assert.Equal(t, tc.accept,
				mediaproof.Verify(key, proof, testVersion, ts, "POST", "/p"))
		})
	}
}

func TestVerifyFailsClosedOnMalformedInput(t *testing.T) {
	key, ts, proof := signedNow(t, "ctx/a", "POST", "/p")

	t.Run("unparseable timestamp", func(t *testing.T) {
		assert.False(t, mediaproof.Verify(key, proof, testVersion, "not-a-number", "POST", "/p"))
	})
	t.Run("non-hex proof of correct width", func(t *testing.T) {
		assert.False(t, mediaproof.Verify(key, strings.Repeat("z", 64), testVersion, ts, "POST", "/p"))
	})
	t.Run("truncated proof", func(t *testing.T) {
		assert.False(t, mediaproof.Verify(key, proof[:16], testVersion, ts, "POST", "/p"))
	})
	t.Run("oversized proof", func(t *testing.T) {
		assert.False(t, mediaproof.Verify(key, proof+proof, testVersion, ts, "POST", "/p"))
	})
	t.Run("empty proof", func(t *testing.T) {
		assert.False(t, mediaproof.Verify(key, "", testVersion, ts, "POST", "/p"))
	})
}

// A newline in any field could shift the boundary between two fields, letting
// two different field lists produce one payload. The package refuses to sign or
// verify such a field rather than reasoning about whether callers can supply
// one — `c.Request.URL.Path` is percent-decoded, so `%0A` yields a real newline.
func TestNewlineBearingFieldsAreUnsignableAndUnverifiable(t *testing.T) {
	key := mediaproof.DeriveKey(testSecret, "ctx/a")
	ts := nowStamp()

	assert.Empty(t, mediaproof.Sign(key, testVersion, ts, "POST", "/p\nX-Evil: 1"))
	assert.False(t, mediaproof.Verify(key, strings.Repeat("a", 64), testVersion, ts,
		"POST", "/p\nX-Evil: 1"))

	// The boundary-shift itself: ["a","b"] and ["a\nb"] must not collide.
	split := mediaproof.Sign(key, testVersion, ts, "a", "b")
	require.NotEmpty(t, split)
	assert.Empty(t, mediaproof.Sign(key, testVersion, ts, "a\nb"))
}

// An unconfigured secret must yield no usable key, so a deployment that forgot
// to set one cannot accidentally accept proofs rather than rejecting them.
func TestUnconfiguredSecretCannotProduceAValidProof(t *testing.T) {
	assert.Nil(t, mediaproof.DeriveKey("", "ctx/a"))

	ts := nowStamp()
	// Sign must refuse too, not just Verify: an asymmetric contract inside one
	// primitive is exactly the shape that gets misread later.
	assert.Empty(t, mediaproof.Sign(nil, testVersion, ts, "POST", "/p"))
	assert.False(t, mediaproof.Verify(nil, strings.Repeat("a", 64), testVersion, ts, "POST", "/p"))
}

func TestTokenDigestDoesNotEmitTheToken(t *testing.T) {
	const probe = "abababababababab-probe" // hex-ish prefix so the check below can fail
	digest := mediaproof.TokenDigest(probe)

	assert.NotContains(t, digest, "abababababababab",
		"a digest that echoed its input would contain this hex-only prefix")
	assert.Len(t, digest, 64)
	assert.Equal(t, digest, mediaproof.TokenDigest(probe))
	assert.NotEqual(t, digest, mediaproof.TokenDigest("another-token"))
}
