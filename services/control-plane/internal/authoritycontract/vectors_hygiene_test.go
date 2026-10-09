package authoritycontract

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// allowedKeys is the closed set of JSON field names in vectors.json (DoR §3.1
// schema plus A1's input/expect keys). recipient_dkx_d is the single private
// name (a 48-byte test-only scalar, V-09).
var allowedKeys = map[string]bool{
	"generator_sha384": true, "vectors_sha384": true, "vectors": true,
	"name": true, "type": true, "bytes_b64": true, "input": true, "expect": true, "bundles": true, "trust": true,
	"prev_head": true, "fix": true, "mem": true, "now_ms": true, "use": true, "device_bundles": true,
	"device_status": true, "prev_device_head": true, "recipient_dkx_d": true,
	"pin": true, "everEnrolled": true, "acceptedPin": true,
	"reason": true, "verdict": true, "reencoded_b64": true, "text_b64": true, "digest_b64": true,
	"device_id": true, "safety_number": true,
	"point_b64": true, "msg_b64": true, "sig_b64": true, "spki_b64": true, "dsk_b64": true, "pin_digest_b64": true,
	"chain_head_b64": true, "account_id": true, "user_id": true, "realm_id": true, "session_id": true,
	"challenge": true, "nonce": true, "client_version": true, "epoch": true, "epoch_valid": true, "valid_age": true,
	"nsfw_auth": true, "jurisdiction_obligation": true, "timestamp": true, "issued_at_ms": true, "records": true,
	"legacy_ek_digest_b64": true, "layout": true, "fields": true,
}

func TestVectorsHygiene(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), 4<<20, "keep vectors.json well under check-added-large-files --maxkb=5000")
	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Empty(t, unknownKeys(doc, nil), "closed field-name set")
	require.Empty(t, nonPlainStrings(doc, "", nil), "every string uses the plain alphabet")
	require.Empty(t, privateKeyFindings(doc), "vectors hold public keys and signatures only")
}

func TestAllowedKeysHaveOnePrivateName(t *testing.T) {
	var private []string
	for k := range allowedKeys {
		if strings.HasSuffix(k, "_d") || strings.Contains(k, "pkcs8") || strings.Contains(k, "priv") {
			private = append(private, k)
		}
	}
	require.Equal(t, []string{"recipient_dkx_d"}, private)
}

// The scanner must find what it exists to find (mutation M8).
func TestPrivateKeyScannerFindsPlantedKeys(t *testing.T) {
	k := testKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	sec1, err := x509.MarshalECPrivateKey(k)
	require.NoError(t, err)
	pkcs1 := x509.MarshalPKCS1PrivateKey(testRSA(t))
	nested := DeviceSecrets{EKPKCS8: pkcs8}.Encode()
	require.NotNil(t, nested)
	// A record is base64 segments joined by "." (dev, root and secrets records), so
	// a key can sit as one segment of a string that is not base64 as a whole.
	segment := b64Of(rnd(48)) + "." + b64Of(pkcs8)
	for name, b := range map[string][]byte{
		"PKCS#8": pkcs8, "SEC1": sec1, "PKCS#1": pkcs1, "inside a message": nested,
		"as a record segment inside a message": enMessage(segment),
	} {
		doc := map[string]any{"vectors": []any{map[string]any{"name": "x", "bytes_b64": base64.StdEncoding.EncodeToString(b)}}}
		require.NotEmpty(t, privateKeyFindings(doc), name)
	}
	require.NotEmpty(t, privateKeyFindings(map[string]any{"input": map[string]any{"x": segment}}), "PKCS#8 as a top-level record segment")

	pub, err := k.PublicKey.Bytes()
	require.NoError(t, err)
	require.Empty(t, privateKeyFindings(map[string]any{"x": base64.StdEncoding.EncodeToString(pub)}))
	require.Empty(t, privateKeyFindings(map[string]any{"x": b64Of(rnd(48)) + "." + b64Of(pub) + ".rotate_root"}), "public segments are not findings")
}

// A key wrapped in maxNesting messages is reached and named as a key; one more
// wrapper is past the limit, and the scanner says so rather than stopping silently.
func TestPrivateKeyScannerFindsPlantedKeysAtTheNestingLimit(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey(t))
	require.NoError(t, err)
	wrapped := func(n int) map[string]any {
		b := pkcs8
		for range n {
			b = enMessage(b64Of(b))
		}
		return map[string]any{"bytes_b64": b64Of(b)}
	}

	atLimit := privateKeyFindings(wrapped(maxNesting))
	require.NotEmpty(t, atLimit, "a key at the limit is reached")
	require.NotContains(t, atLimit, nestingFinding, "a key at the limit is named as a key, not reported as too deep")

	require.Contains(t, privateKeyFindings(wrapped(maxNesting+1)), nestingFinding, "one wrapper past the limit is reported")
}

// A malformed message is still opened: a key in one of its strings is found even
// though the message does not parse as JSON (an unclosed array here).
func TestPrivateKeyScannerFindsKeysInMalformedMessages(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey(t))
	require.NoError(t, err)
	malformed := []byte(encodedPrefix + `"x","` + b64Of(pkcs8) + `"`)
	var probe []any
	require.Error(t, json.Unmarshal(malformed, &probe), "the fixture must not parse")

	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(malformed)}))
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(enMessage(b64Of(malformed)))}),
		"a malformed message nested inside a well-formed one")
}

// A key encoded more than once is opened layer by layer: base64 of base64, and
// base64 of a record whose segment is the key. Base64 wrappers count toward the
// same nesting limit as messages, so a deeper stack is reported, not skipped.
func TestPrivateKeyScannerFindsMultiplyEncodedKeys(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey(t))
	require.NoError(t, err)
	wrapped := func(n int) string {
		s := b64Of(pkcs8)
		for range n {
			s = b64Of([]byte(s))
		}
		return s
	}

	twice := privateKeyFindings(map[string]any{"bytes_b64": wrapped(1)})
	require.NotEmpty(t, twice, "base64(base64(PKCS#8))")
	require.NotContains(t, twice, nestingFinding)
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of([]byte(b64Of(rnd(48)) + "." + b64Of(pkcs8)))}),
		"base64 of a record whose segment is the key")
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(enMessage(wrapped(1)))}),
		"a double-encoded key inside a message")

	atLimit := privateKeyFindings(map[string]any{"bytes_b64": wrapped(maxNesting)})
	require.NotEmpty(t, atLimit)
	require.NotContains(t, atLimit, nestingFinding, "a key at the limit is named as a key")
	require.Contains(t, privateKeyFindings(map[string]any{"bytes_b64": wrapped(maxNesting + 1)}), nestingFinding,
		"one layer past the limit is reported")

	pub, err := testKey(t).PublicKey.Bytes()
	require.NoError(t, err)
	require.Empty(t, privateKeyFindings(map[string]any{"x": b64Of([]byte(b64Of(pub)))}), "a double-encoded public key is not a finding")
}

// A PEM private key is a finding wherever it sits: as a plain string (a vector
// name, say), or as the decoded bytes of a base64 field. A PEM public key is not.
func TestPrivateKeyScannerFindsPEMKeys(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey(t))
	require.NoError(t, err)
	for _, typ := range []string{"PRIVATE KEY", "EC PRIVATE KEY", "RSA PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "OPENSSH PRIVATE KEY"} {
		block := string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: pkcs8}))
		require.NotEmpty(t, privateKeyFindings(map[string]any{"name": block}), typ+" as a plain string")
		require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of([]byte(block))}), typ+" as decoded bytes")
	}
	spki, err := x509.MarshalPKIXPublicKey(&testKey(t).PublicKey)
	require.NoError(t, err)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}))
	require.Empty(t, privateKeyFindings(map[string]any{"name": pub, "bytes_b64": b64Of([]byte(pub))}), "a PEM public key is not a finding")
}

// A raw openssh-key-v1 container is a finding, as are a PuTTY key file and its
// base64. The container bytes are what an OPENSSH PRIVATE KEY PEM block wraps.
func TestPrivateKeyScannerFindsOpenSSHAndPuTTYKeys(t *testing.T) {
	blob := append(append([]byte{}, opensshMagic...), rnd(64)...)
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(blob)}), "raw openssh-key-v1 blob")
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(enMessage(b64Of(blob)))}), "inside a message")
	putty := "PuTTY-User-Key-File-3: ecdsa-sha2-nistp384\nEncryption: none\n"
	require.NotEmpty(t, privateKeyFindings(map[string]any{"name": putty}), "PuTTY key file as a plain string")
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of([]byte(putty))}), "PuTTY key file as decoded bytes")
}

// A JWK in decoded bytes is a finding: base64 of a JSON object, on its own or
// inside a message. A plain-string JWK is the alphabet rule's job.
func TestPrivateKeyScannerFindsJWKs(t *testing.T) {
	jwk := []byte(`{"kty":"EC","crv":"P-384","x":"AA","y":"AA","d":"AA"}`)
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(jwk)}))
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(enMessage(b64Of(jwk)))}), "inside a message")
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of([]byte(`{"d":"AA"}`))}), "a private member alone")
	require.Empty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of([]byte(`{"a":1}`))}),
		"an object with no JWK member is a negative vector, not a finding")
}

// A key in any encoding the alphabet admits is reached: URL-safe and raw base64,
// and hex, on their own and as a record segment.
func TestPrivateKeyScannerFindsKeysInEveryAdmittedEncoding(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey(t))
	require.NoError(t, err)
	for name, s := range map[string]string{
		"URL-safe padded": base64.URLEncoding.EncodeToString(pkcs8),
		"URL-safe raw":    base64.RawURLEncoding.EncodeToString(pkcs8),
		"standard raw":    base64.RawStdEncoding.EncodeToString(pkcs8),
		"hex":             hex.EncodeToString(pkcs8),
		"base32":          base32.StdEncoding.EncodeToString(pkcs8),
		"base32 raw":      base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(pkcs8),
		"base32hex":       base32.HexEncoding.EncodeToString(pkcs8),
	} {
		require.True(t, plainString.MatchString(s), name+" fits the alphabet, so the scanner must reach it")
		require.NotEmpty(t, privateKeyFindings(map[string]any{"name": s}), name)
		require.NotEmpty(t, privateKeyFindings(map[string]any{"name": b64Of(rnd(48)) + "." + s}), name+" as a record segment")
	}
}

// An encrypted PKCS#8 container is a finding: ParsePKCS8PrivateKey refuses it,
// and so do the SEC1 and PKCS#1 parsers. A public SPKI, which shares its outer
// shape, is not.
func TestPrivateKeyScannerFindsEncryptedPKCS8(t *testing.T) {
	pbes2 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	enc, err := asn1.Marshal(encryptedPKCS8{Algorithm: pkix.AlgorithmIdentifier{Algorithm: pbes2}, Data: rnd(64)})
	require.NoError(t, err)
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(enc)}))
	require.NotEmpty(t, privateKeyFindings(map[string]any{"bytes_b64": b64Of(enMessage(b64Of(enc)))}), "inside a message")

	spki, err := x509.MarshalPKIXPublicKey(&testKey(t).PublicKey)
	require.NoError(t, err)
	require.Empty(t, privateKeyFindings(map[string]any{"spki_b64": b64Of(spki)}), "a public SPKI is not a finding")
}

// enMessage is a minimal EN1-shaped message carrying one string position: the
// scanner reads the prefix and the JSON array, nothing else.
func enMessage(s string) []byte { return []byte(encodedPrefix + `"x","` + s + `"]`) }

// plainString is the alphabet every string in the file uses: EN2's S characters
// plus "^" (vector names such as neg/status/u-2^53). Nothing that holds a key in
// text form fits it, since PEM, PuTTY and JWK all need spaces, newlines, quotes
// or braces, so the rule closes plain-text key formats as a class rather than
// one format at a time. safety_number is the one key with its own shape.
var (
	plainString  = regexp.MustCompile(`^[A-Za-z0-9._:/+=^-]*$`)
	safetyNumber = regexp.MustCompile(`^[0-9]{5}( [0-9]{5}){11}$`)
)

// nonPlainStrings returns every string that fits neither shape, by key.
func nonPlainStrings(v any, key string, out []string) []string {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			out = nonPlainStrings(e, k, out)
		}
	case []any:
		for _, e := range x {
			out = nonPlainStrings(e, key, out)
		}
	case string:
		if key == "safety_number" && safetyNumber.MatchString(x) {
			return out
		}
		if !plainString.MatchString(x) {
			out = append(out, key+": "+x[:min(len(x), 24)])
		}
	}
	sort.Strings(out)
	return out
}

// A key held as text is refused by the alphabet rule whatever its format; the
// safety number keeps its exact shape and nothing looser.
func TestNonPlainStringsRefusesTextKeyFormats(t *testing.T) {
	jwk := `{"kty":"EC","crv":"P-384","x":"AA","y":"AA","d":"AA"}`
	block := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rnd(32)}))
	for name, s := range map[string]string{
		"JWK": jwk, "PEM": block, "PuTTY": "PuTTY-User-Key-File-3: ecdsa-sha2-nistp384",
		"newline": "a\nb", "space": "a b",
	} {
		require.NotEmpty(t, nonPlainStrings(map[string]any{"name": s}, "", nil), name)
	}
	require.NotEmpty(t, nonPlainStrings(map[string]any{"safety_number": jwk}, "", nil), "safety_number takes only its own shape")
	sn := strings.TrimSpace(strings.Repeat("01234 ", 12))
	require.Empty(t, nonPlainStrings(map[string]any{"safety_number": sn, "name": "neg/status/u-2^53"}, "", nil))
	require.NotEmpty(t, nonPlainStrings(map[string]any{"name": sn}, "", nil), "only safety_number may hold spaces")
}

func unknownKeys(v any, out []string) []string {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if !allowedKeys[k] {
				out = append(out, k)
			}
			out = unknownKeys(e, out)
		}
	case []any:
		for _, e := range x {
			out = unknownKeys(e, out)
		}
	}
	sort.Strings(out)
	return out
}

// maxNesting is how many EN1 messages deep the scanner opens. The real file nests
// at most five.
const maxNesting = 6

// quotedToken matches one JSON string token in a message the scanner cannot
// parse. EN2 strings never contain a quote or a backslash.
var quotedToken = regexp.MustCompile(`"([^"\\]*)"`)

// pemPrivateKey matches the header of every PEM private-key block: PKCS#8
// ("PRIVATE KEY"), encrypted PKCS#8, SEC1 ("EC PRIVATE KEY"), PKCS#1 ("RSA
// PRIVATE KEY") and OpenSSH ("OPENSSH PRIVATE KEY").
// A PuTTY key file header ("PuTTY-User-Key-File-3: ...") is matched the same way.
var pemPrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|PuTTY-User-Key-File-[0-9]+:`)

// opensshMagic opens a raw openssh-key-v1 container, the bytes inside an
// OPENSSH PRIVATE KEY PEM block.
var opensshMagic = []byte("openssh-key-v1\x00")

// nestingFinding is what the scanner reports for a message it will not open: a
// nesting it cannot see into is a finding, never a silent stop.
var nestingFinding = fmt.Sprintf("nesting deeper than %d", maxNesting)

// privateKeyFindings walks every string. A string, and each "."-joined segment of
// one (dev strings, root and secrets records carry base64 segments that way), is
// checked as a Strict base64 private key and, when it is an EN1 message, its own
// positions are walked.
func privateKeyFindings(v any) []string {
	var out []string
	var walk func(any, int)
	var inspect func(s string, b []byte, depth int)
	scan := func(s string, depth int) {
		for _, b := range decodings(s) {
			inspect(s, b, depth)
		}
	}
	inspect = func(s string, b []byte, depth int) {
		if looksPrivate(b) || pemPrivateKey.Match(b) || isJSONObject(b) {
			out = append(out, s[:min(len(s), 16)]+"…")
			return
		}
		isMessage := bytes.HasPrefix(b, []byte(encodedPrefix))
		if !isMessage && !isB64Text(b) {
			return
		}
		if depth >= maxNesting {
			out = append(out, nestingFinding)
			return
		}
		if !isMessage {
			// base64 of base64 text, or of a "."-joined record: a key wrapped in
			// more than one encoding is still a key, so open the next layer.
			walk(string(b), depth+1)
			return
		}
		var inner []any
		if json.Unmarshal(b, &inner) == nil {
			walk(inner, depth+1)
			return
		}
		// A malformed message is still scanned: negative vectors are malformed on
		// purpose, and a key in one of their strings is still a key. Walk every
		// quoted token in the raw bytes instead of the parsed positions.
		for _, m := range quotedToken.FindAllSubmatch(b, -1) {
			walk(string(m[1]), depth+1)
		}
	}
	walk = func(v any, depth int) {
		switch x := v.(type) {
		case map[string]any:
			for _, e := range x {
				walk(e, depth)
			}
		case []any:
			for _, e := range x {
				walk(e, depth)
			}
		case string:
			if pemPrivateKey.MatchString(x) {
				out = append(out, "PEM private key")
			}
			scan(x, depth)
			for _, seg := range strings.Split(x, ".") {
				if seg != "" && seg != x {
					scan(seg, depth)
				}
			}
		}
	}
	walk(v, 0)
	return out
}

// decodings is every non-empty value s decodes to: standard and URL-safe base64,
// each padded or raw, base32 (standard and extended-hex alphabets, padded or
// raw), and hex. The generator emits only padded standard base64,
// but the alphabet rule admits the other forms, so a key in any of them must
// still reach the checks.
func decodings(s string) [][]byte {
	var out [][]byte
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding.Strict(), base64.RawStdEncoding.Strict(),
		base64.URLEncoding.Strict(), base64.RawURLEncoding.Strict(),
	} {
		if b, err := enc.DecodeString(s); err == nil && len(b) > 0 {
			out = append(out, b)
		}
	}
	for _, enc := range []*base32.Encoding{
		base32.StdEncoding, base32.StdEncoding.WithPadding(base32.NoPadding),
		base32.HexEncoding, base32.HexEncoding.WithPadding(base32.NoPadding),
	} {
		if b, err := enc.DecodeString(s); err == nil && len(b) > 0 {
			out = append(out, b)
		}
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) > 0 {
		out = append(out, b)
	}
	return out
}

// isB64Text reports whether decoded bytes are themselves base64 text (standard
// or URL-safe) or hex, or such segments joined by ".", and so could carry
// another encoded layer.
func isB64Text(b []byte) bool {
	for _, c := range b {
		if ('A' > c || c > 'Z') && ('a' > c || c > 'z') && ('0' > c || c > '9') &&
			c != '+' && c != '/' && c != '=' && c != '.' && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// isJSONObject flags decoded bytes that are a JWK: a JSON object with a "kty"
// member or a private-key member ("d", or RSA's "p"/"q"). A bare object is not
// enough, because the negative vectors carry objects such as {"a":1} on purpose
// to show EN1 refuses them.
func isJSONObject(b []byte) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	for _, k := range []string{"kty", "d", "p", "q"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func looksPrivate(b []byte) bool {
	if bytes.HasPrefix(b, opensshMagic) {
		return true
	}
	if _, err := x509.ParsePKCS8PrivateKey(b); err == nil {
		return true
	}
	if looksEncryptedPKCS8(b) {
		return true
	}
	if _, err := x509.ParseECPrivateKey(b); err == nil {
		return true
	}
	_, err := x509.ParsePKCS1PrivateKey(b)
	return err == nil
}

// encryptedPKCS8 is RFC 5958's EncryptedPrivateKeyInfo: an algorithm
// identifier and the encrypted key as an OCTET STRING.
type encryptedPKCS8 struct {
	Algorithm pkix.AlgorithmIdentifier
	Data      []byte
}

// looksEncryptedPKCS8 recognises the container whatever its encryption
// algorithm, since an encrypted key is still key material. A public SPKI has
// the same outer shape but carries a BIT STRING, which does not decode into
// Data, so the vectors' EK SPKIs are not findings.
func looksEncryptedPKCS8(b []byte) bool {
	var e encryptedPKCS8
	rest, err := asn1.Unmarshal(b, &e)
	return err == nil && len(rest) == 0 && len(e.Data) > 0 && len(e.Algorithm.Algorithm) > 0
}

// Every value of every closed set sits inside a JSON string, where EN2 admits only
// the S alphabet. A value with a byte outside it would be written by the encoder and
// then refused by the decoder's pre-filter (kind.go isSChar), so it is pinned here
// byte by byte for each set enums.go declares.
func TestClosedSetValuesAreSChars(t *testing.T) {
	sets := map[string][]string{
		"publishedOps": publishedOps, "deviceOps": deviceOps, "scopeValues": scopeValues,
		"stateValues": stateValues, "priorStateValues": priorStateValues, "assuranceValues": assuranceValues,
		"segmentAssurances": segmentAssurances, "suspenders": suspenders, "actorValues": actorValues,
		"sigRoles": sigRoles, "factorNames": factorNames, "bindKinds": bindKinds, "bindWaits": bindWaits,
		"signedTypes": signedTypes, "secretsOps": secretsOps, "nodeRoles": nodeRoles, "ekAlgs": ekAlgs,
		"refusalCodes": refusalCodes, "errorCodes": errorCodes,
	}
	for name, vals := range sets {
		require.NotEmpty(t, vals, name)
		for _, v := range vals {
			for i := 0; i < len(v); i++ {
				require.Truef(t, isSChar(v[i]), "%s value %q: byte %d (%#02x) is outside the S alphabet", name, v, i, v[i])
			}
		}
	}
}
