package devicerecovery

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type vector struct {
	OriginCases []struct {
		Origin string `json:"origin"`
		Valid  bool   `json:"valid"`
	} `json:"origin_cases"`
	Context         Context `json:"context"`
	Offer           Offer   `json:"offer"`
	UserID          string  `json:"user_id"`
	JTI             string  `json:"recovery_token_jti"`
	Canonical       string  `json:"canonical_transcript"`
	CanonicalBase64 string  `json:"canonical_transcript_base64"`
	Hash            string  `json:"transcript_hash"`
	RequesterScalar string  `json:"requester_private_scalar"`
	ResponderScalar string  `json:"responder_private_scalar"`
	Shared          string  `json:"shared_secret"`
	EncryptionKey   string  `json:"encryption_key"`
	Fingerprint     string  `json:"fingerprint_bytes"`
	Envelope        string  `json:"encrypted_payload"`
}

func fixture(t *testing.T) vector {
	t.Helper()
	b, err := os.ReadFile("../../../../docs/design/trusted-recovery-v2-vectors.json")
	require.NoError(t, err)
	var v vector
	require.NoError(t, json.Unmarshal(b, &v))
	return v
}
func TestSharedCanonicalVectorAndIndependentDerivations(t *testing.T) {
	v := fixture(t)
	require.Equal(t, v.Context.AccountBinding, AccountBinding(v.UserID))
	require.Equal(t, v.Context.RecoveryTokenJTIHash, Encode(Hash(v.JTI)))
	b, h, err := Transcript(v.Context, v.Offer)
	require.NoError(t, err)
	require.Equal(t, v.Canonical, string(b))
	require.Equal(t, v.CanonicalBase64, Encode(b))
	require.Equal(t, v.Hash, Encode(h))
	for _, pair := range [][2]string{{v.RequesterScalar, v.Offer.ResponderPublicKey}, {v.ResponderScalar, v.Context.RequesterPublicKey}} {
		privateBytes, err := Decode(pair[0], 48)
		require.NoError(t, err)
		priv, err := ecdh.P384().NewPrivateKey(privateBytes)
		require.NoError(t, err)
		publicBytes, err := PublicKey(pair[1])
		require.NoError(t, err)
		pub, err := ecdh.P384().NewPublicKey(publicBytes)
		require.NoError(t, err)
		bits, err := priv.ECDH(pub)
		require.NoError(t, err)
		require.Equal(t, v.Shared, Encode(bits))
		key, err := hkdf.Key(sha256.New, bits, h, "concord-trusted-device-recovery/v2/encryption", 32)
		require.NoError(t, err)
		require.Equal(t, v.EncryptionKey, Encode(key))
		fp, err := hkdf.Key(sha256.New, bits, h, "concord-trusted-device-recovery/v2/fingerprint", 16)
		require.NoError(t, err)
		require.Equal(t, v.Fingerprint, Encode(fp))
	}
	_, err = Envelope(v.Envelope)
	require.NoError(t, err)
}
func TestTranscriptEveryFieldIsBound(t *testing.T) {
	v := fixture(t)
	variants := []func(*Context, *Offer){
		func(c *Context, _ *Offer) { c.ServerOrigin = "http://localhost:8080" },
		func(c *Context, _ *Offer) { c.RequestID = "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee" },
		func(c *Context, _ *Offer) { c.AccountBinding = Encode(Hash("other-account")) },
		func(c *Context, _ *Offer) { c.ExpiresAt++ },
		func(c *Context, _ *Offer) { c.RequesterNonce = Encode(Hash("other-nonce")) },
		func(_ *Context, o *Offer) { o.ResponderNonce = Encode(Hash("other-nonce")) },
		func(c *Context, _ *Offer) { c.RecoveryTokenJTIHash = Encode(Hash("other-token")) },
		func(c *Context, o *Offer) { c.RequesterPublicKey = o.ResponderPublicKey },
		func(c *Context, o *Offer) { o.ResponderPublicKey = c.RequesterPublicKey },
	}
	for i, mutate := range variants {
		c, o := v.Context, v.Offer
		mutate(&c, &o)
		_, h, err := Transcript(c, o)
		require.NoError(t, err)
		require.NotEqual(t, v.Hash, Encode(h), i)
	}
	c := v.Context
	c.ProtocolVersion = 1
	_, _, err := Transcript(c, v.Offer)
	require.Error(t, err)
	o := v.Offer
	o.ResponderPublicKey = Encode(make([]byte, 97))
	_, _, err = Transcript(v.Context, o)
	require.Error(t, err)
}
func TestCanonicalBounds(t *testing.T) {
	for _, s := range []string{"https://example.test", "http://localhost:8080", "http://127.0.0.1:0", "https://[::1]:8443", "https://xn--bcher-kva.example", "https://foo_bar.test", "https://foo!bar.test", "https://[::ffff:c000:201]", "https://[1::2:0:0:3:4]"} {
		require.True(t, Origin(s), s)
	}
	for _, s := range []string{"HTTPS://example.test", "https://Example.test", "https://example.test/", "https://example.test?", "https://example.test#", "https://u@example.test", "https://example.test:443", "http://localhost:08080", "http://127.1", "http://0x7f000001", "http://[0:0::1]", "https://bücher.example", "https://[::ffff:192.0.2.1]", "https://foo^bar.test", "https://foo|bar.test", "ftp://example.test", "https://example.test:"} {
		require.False(t, Origin(s), s)
	}
	for _, s := range []string{"", "AA==", Encode(make([]byte, 32)) + "\n", Encode(make([]byte, 32))[:43]} {
		_, err := Decode(s, 32)
		require.Error(t, err)
	}
	_, err := PublicKey(Encode(make([]byte, 97)))
	require.Error(t, err)
	for _, n := range []int{0, 29, 8193} {
		b := make([]byte, n)
		if n > 0 {
			b[0] = 2
		}
		_, err := Envelope(Encode(b))
		require.Error(t, err)
	}
	b := make([]byte, 30)
	b[0] = 1
	_, err = Envelope(Encode(b))
	require.Error(t, err)
	b[0] = 2
	_, err = Envelope(Encode(b))
	require.NoError(t, err)
}

// These same fixture cases are checked against the renderer WHATWG URL rule.
func TestWHATWGOriginControls(t *testing.T) {
	v := fixture(t)
	require.NotEmpty(t, v.OriginCases)
	for _, tc := range v.OriginCases {
		require.Equal(t, tc.Valid, Origin(tc.Origin), tc.Origin)
	}
}
