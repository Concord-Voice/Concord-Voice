package authoritycontract

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The device-wrap-open vectors are the one place vectors.json carries private
// key material: each ciphertext opens, under the recipient_dkx_d the vector
// publishes, to a device-secrets holding test PKCS#8 keys (DoR §3.1 "Wraps",
// V-09). That is acceptable only while those keys are single-use test keys of
// the wrap vectors' own account. This is the compensating control the
// detect-secrets exclusion in .pre-commit-config.yaml names: every wrap is
// opened, and no key it holds may be a key the file publishes for anything
// else (another account's root, EK, RSK or CAK, A1's cached EK), nor a key
// committed in this package's Go tests or in gen.mjs. A key reused that way
// would make an openable ciphertext a signing or decryption key for other
// vectors, and the regeneration with fresh keys that the exclusion relies on
// would no longer be what keeps the file test-only.

// wrapAccountVector reports a vector of the wraps' own account: the wraps and
// the device ledger that produces the head they open against.
func wrapAccountVector(v vector) bool {
	return v.Type == "device-wrap-open" || v.Name == "wrap/head-from-verified-ledger"
}

// wrapSecretKey is one private key a wrap vector's plaintext holds, by the
// public forms the file could publish it in.
type wrapSecretKey struct {
	vector string
	forms  [][]byte // raw point and SPKI DER for an EC key; SPKI DER for RSA
}

// openedWrapKeys opens every device-wrap-open vector with its recipient_dkx_d
// and returns the keys its device-secrets holds. A wrap whose AEAD refuses it
// (a tampered tag, a swapped entry digest) holds nothing a reader can open.
func openedWrapKeys(t *testing.T, vs []vector) (keys []wrapSecretKey, opened int) {
	t.Helper()
	for _, v := range vs {
		if v.Type != "device-wrap-open" {
			continue
		}
		w, err := DecodeDeviceWrap(mustB64(t, v.BytesB64))
		if err != nil {
			continue // the codec half of an epk test: nothing to open
		}
		dkx, err := ecdh.P384().NewPrivateKey(mustB64(t, v.RecipientDkxD))
		require.NoError(t, err, v.Name)
		// The published scalar itself must be single-use too (security re-check N4).
		keys = append(keys, wrapSecretKey{vector: v.Name, forms: [][]byte{dkx.PublicKey().Bytes()}})
		pt, ok := decryptWrap(w, dkx)
		if !ok {
			continue
		}
		opened++
		s, err := DecodeDeviceSecrets(pt)
		require.NoError(t, err, v.Name)
		for _, der := range [][]byte{s.RootPKCS8, s.EKPKCS8} {
			if len(der) == 0 {
				continue
			}
			k, err := x509.ParsePKCS8PrivateKey(der) // lenient on purpose: trailing bytes still hold a key
			require.NoError(t, err, "%s: a wrap opens to a key that does not parse", v.Name)
			keys = append(keys, wrapSecretKey{vector: v.Name, forms: publicForms(t, k)})
		}
	}
	return keys, opened
}

func publicForms(t *testing.T, k any) [][]byte {
	t.Helper()
	var forms [][]byte
	switch p := k.(type) {
	case *ecdsa.PrivateKey:
		pt, err := p.PublicKey.Bytes()
		require.NoError(t, err)
		forms = append(forms, pt)
		spki, err := x509.MarshalPKIXPublicKey(&p.PublicKey)
		require.NoError(t, err)
		forms = append(forms, spki)
	case *rsa.PrivateKey:
		spki, err := x509.MarshalPKIXPublicKey(&p.PublicKey)
		require.NoError(t, err)
		forms = append(forms, spki)
	default:
		t.Fatalf("a wrap opens to a %T key", k)
	}
	return forms
}

// expandedBytes collects every byte string a value carries, opened the way
// privateKeyFindings opens it: standard base64, "."-joined record segments and
// EN1 messages, to maxNesting messages deep.
func expandedBytes(v any, out map[string]bool, depth int) {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			expandedBytes(e, out, depth)
		}
	case []any:
		for _, e := range x {
			expandedBytes(e, out, depth)
		}
	case string:
		segs := []string{x}
		if strings.Contains(x, ".") {
			segs = append(segs, strings.Split(x, ".")...)
		}
		for _, s := range segs {
			b, err := base64.StdEncoding.Strict().DecodeString(s)
			if err != nil || len(b) == 0 {
				continue
			}
			out[string(b)] = true
			var inner []any
			if depth < maxNesting && bytes.HasPrefix(b, []byte(encodedPrefix)) && json.Unmarshal(b, &inner) == nil {
				expandedBytes(inner, out, depth+1)
			}
		}
	}
}

// publishedSplit is every byte string the vectors carry outside the wraps'
// own account, and inside it (the positive control's haystack).
func publishedSplit(t *testing.T, vs []vector) (outside, inside map[string]bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	require.NoError(t, err)
	var doc struct {
		Vectors []map[string]any `json:"vectors"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Len(t, doc.Vectors, len(vs))
	outside, inside = map[string]bool{}, map[string]bool{}
	for i, v := range vs {
		if wrapAccountVector(v) {
			expandedBytes(doc.Vectors[i], inside, 0)
		} else {
			expandedBytes(doc.Vectors[i], outside, 0)
		}
	}
	return outside, inside
}

// committedSources is the text of every Go test file in this package and of
// gen.mjs: where a fixed test key would have to be committed.
func committedSources(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	pkg := os.DirFS(".")
	names, err := fs.Glob(pkg, "*_test.go")
	require.NoError(t, err)
	require.NotEmpty(t, names)
	for _, n := range names {
		b, err := fs.ReadFile(pkg, n)
		require.NoError(t, err, n)
		sb.Write(b)
	}
	b, err := fs.ReadFile(os.DirFS(filepath.Join("..", "..", "..", "..", "scripts", "authority-vectors")), "gen.mjs")
	require.NoError(t, err, "gen.mjs")
	sb.Write(b)
	return sb.String()
}

func TestWrapVectorKeysAreSingleUseTestKeys(t *testing.T) {
	vs := loadVectors(t).Vectors
	keys, opened := openedWrapKeys(t, vs)
	require.GreaterOrEqual(t, opened, 3, "the ok wraps open: an opener that opens nothing checks nothing")
	require.NotEmpty(t, keys)
	outside, inside := publishedSplit(t, vs)
	src := committedSources(t)

	foundInside := 0
	for _, k := range keys {
		for _, f := range k.forms {
			require.False(t, outside[string(f)], "%s: a wrap's private key is a key the file publishes for another account", k.vector)
			for _, enc := range []string{base64.StdEncoding.EncodeToString(f), base64.RawURLEncoding.EncodeToString(f), hex.EncodeToString(f)} {
				require.NotContains(t, src, enc, "%s: a wrap's private key is committed in a test fixture or gen.mjs", k.vector)
			}
			if inside[string(f)] {
				foundInside++
			}
		}
	}
	// Positive control: the ok wraps' roots and EKs are the wrap account's own,
	// so the same search finds them in that account's head and bundles. Without
	// this, a search that could never match would pass the check above.
	require.GreaterOrEqual(t, foundInside, 3, "the wrap account's own keys are found where it publishes them")
}

// The search reaches a key nested in an encoded message, the shape a reused
// key would take in another account's bundle.
func TestExpandedBytesReachesNestedKeys(t *testing.T) {
	pt := testPoint(t)
	inner := enMessage(base64.StdEncoding.EncodeToString(pt))
	outer := enMessage(base64.StdEncoding.EncodeToString(rnd(48)) + "." + base64.StdEncoding.EncodeToString(inner))
	got := map[string]bool{}
	expandedBytes(map[string]any{"bytes_b64": base64.StdEncoding.EncodeToString(outer)}, got, 0)
	require.True(t, got[string(pt)], "a point two messages deep, behind a record segment")
	require.False(t, got[string(testPoint(t))], "a point the value does not carry")
}
