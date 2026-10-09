package authoritycontract

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Every key here is generated at test runtime; nothing PEM- or PKCS#8-shaped
// is committed (DoR §3.1).

func testKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	return k
}

func pointOf(t testing.TB, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	b, err := k.PublicKey.Bytes()
	require.NoError(t, err)
	return b
}

func testPoint(t testing.TB) []byte { return pointOf(t, testKey(t)) }

// hybridOf re-prefixes an uncompressed point as SEC1 hybrid with the correct
// y parity, the one form Node's importKey('raw') accepts (measured).
func hybridOf(p []byte) []byte {
	h := slices.Clone(p)
	h[0] = 0x06 | (p[96] & 1)
	return h
}

func signP1363(t testing.TB, k *ecdsa.PrivateKey, msg []byte) []byte {
	t.Helper()
	h := sha512.Sum384(msg)
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	require.NoError(t, err)
	out := make([]byte, 96)
	r.FillBytes(out[:48])
	s.FillBytes(out[48:])
	return out
}

var (
	rsaOnce sync.Once
	rsaKey  *rsa.PrivateKey
)

// testRSA is one 4096-bit key per test binary (generation takes seconds).
func testRSA(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	rsaOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			panic(err)
		}
		rsaKey = k
	})
	return rsaKey
}

func testEKSPKI(t testing.TB) []byte {
	t.Helper()
	b, err := x509.MarshalPKIXPublicKey(&testRSA(t).PublicKey)
	require.NoError(t, err)
	return b
}

func digestOf(s string) []byte { return Digest([]byte(s)) }

func rnd(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func newID() string          { return uuid.NewString() }
func layoutMax(m specer) int { tag, fs := m.spec(); return maxLen(tag, fs) }

// roundTrip encodes m, decodes it with DecodeT and through the registry, and
// requires the re-encoding to be byte-identical.
func roundTrip[T any, PT interface {
	*T
	Message
}](t *testing.T, m T) []byte {
	t.Helper()
	b, err := Marshal(PT(&m))
	require.NoError(t, err)
	got, err := decodeAs[T, PT](b, 0)
	require.NoError(t, err)
	require.Equal(t, b, PT(&got).Encode())
	tag, _ := PT(&m).spec()
	viaRegistry, err := Decode(tag, b)
	require.NoError(t, err, "layout %s must be registered", tag)
	require.Equal(t, b, viaRegistry.Encode())
	return b
}

func b64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
