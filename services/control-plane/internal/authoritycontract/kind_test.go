package authoritycontract

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEN2AdmitsExactlyTheAlphabet(t *testing.T) {
	allowed := `ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:/+=-",[]`
	for c := 0; c < 256; c++ {
		err := en2([]byte{byte(c)})
		if strings.IndexByte(allowed, byte(c)) >= 0 {
			require.NoError(t, err, "byte 0x%02x", c)
		} else {
			require.ErrorIs(t, err, ErrMalformed, "byte 0x%02x", c)
		}
	}
}

func TestParseU(t *testing.T) {
	for _, ok := range []string{"0", "1", "9007199254740991"} {
		_, err := parseU(ok)
		require.NoError(t, err, ok)
	}
	for _, bad := range []string{"", "00", "01", "-1", "+1", "1.0", "1e0", "9007199254740992", "18446744073709551616"} {
		_, err := parseU(bad)
		require.ErrorIs(t, err, ErrMalformed, bad)
	}
}

// The Strict half of EN5 step 5 in isolation: the value-level re-encode check
// would also refuse "QR==", so only this test can see Strict() go missing.
func TestDecodeB64StrictRefusesNonZeroTrailingBits(t *testing.T) {
	_, err := decodeB64Strict("QR==")
	require.Error(t, err)
	b, err := decodeB64Strict("QQ==")
	require.NoError(t, err)
	require.Equal(t, []byte("A"), b)
}

func TestDecodeB64(t *testing.T) {
	b, err := decodeB64("QQ==")
	require.NoError(t, err)
	require.Equal(t, []byte("A"), b)
	for _, bad := range []string{"QR==", "QQ", "QQ=", "Q\nQ==", "QQ==\n", "Q Q=="} {
		_, err := decodeB64(bad)
		require.ErrorIs(t, err, ErrMalformed, "%q", bad)
	}
}

func TestCheckString(t *testing.T) {
	v4 := "0f8fad5b-d9cb-469f-a165-70867728950e"
	v1 := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	cases := []struct {
		k  kind
		s  string
		ok bool
	}{
		{kStr(3), "a.b", true}, {kStr(3), "abcd", false}, {kStr(3), "", false}, {kStr(3).OrEmpty(), "", true},
		{kStr(3), "a b", false}, {kStr(3), `a"`, false}, {kStr(3), "a,", false},
		{kS36, v1, true}, {kS36, strings.ToUpper(v4), false}, {kS36, "{" + v4 + "}", false}, {kS36, strings.ReplaceAll(v4, "-", ""), false},
		{kS36v4, v4, true}, {kS36v4, v1, false},
		{kE("", "x"), "", true}, {kE("x"), "", false}, {kE("x"), "y", false}, {kE("x"), "x", true},
	}
	for _, c := range cases {
		err := c.k.checkString(c.s)
		if c.ok {
			require.NoError(t, err, "%q", c.s)
		} else {
			require.ErrorIs(t, err, ErrMalformed, "%q", c.s)
		}
	}
}

func TestCheckBytes(t *testing.T) {
	require.NoError(t, kB(2).checkBytes([]byte{1, 2}))
	require.ErrorIs(t, kB(2).checkBytes([]byte{1}), ErrMalformed)
	require.ErrorIs(t, kB(2).checkBytes(nil), ErrMalformed)
	require.NoError(t, kB(2).OrEmpty().checkBytes(nil))
	require.NoError(t, kBUpTo(3).checkBytes([]byte{1}))
	require.NoError(t, kBUpTo(3).checkBytes([]byte{1, 2, 3}))
	require.ErrorIs(t, kBUpTo(3).checkBytes([]byte{1, 2, 3, 4}), ErrMalformed)
	require.NoError(t, kBOneOf(96, 512).checkBytes(make([]byte, 512)))
	require.ErrorIs(t, kBOneOf(96, 512).checkBytes(make([]byte, 97)), ErrMalformed)
}

func TestMaxEncodedLen(t *testing.T) {
	require.Equal(t, 10, kStr(8).maxEncodedLen())
	require.Equal(t, 38, kS36.maxEncodedLen())
	require.Equal(t, 16, kU.maxEncodedLen())
	require.Equal(t, 1, kU01.maxEncodedLen())
	require.Equal(t, 130, kB(96).maxEncodedLen())
	require.Equal(t, 134, kB(97).maxEncodedLen())
	require.Equal(t, 66, kB(48).maxEncodedLen())
	require.Equal(t, 2734, kBUpTo(2048).maxEncodedLen())
	require.Equal(t, 686, kBOneOf(96, 512).maxEncodedLen())
	require.Equal(t, 8, kE("", "x", "unbind").maxEncodedLen())
	require.Equal(t, 2+4*134+3, kA(kB(97), 1, 4).maxEncodedLen())
	require.Equal(t, 2, kA(kS36, 0, 0).maxEncodedLen())
	require.Equal(t, -1, kL(kStr(4), 0, -1).maxEncodedLen())
}

// The remaining cases pin what the Task 2 field tables will rely on but no
// layout exercises yet.

// An error string is logged, so it names the failed rule and never the value
// it refused (observability.md).
func TestMalformedNamesTheRuleNeverTheValue(t *testing.T) {
	err := malformed("s length")
	require.ErrorIs(t, err, ErrMalformed)
	require.EqualError(t, err, "authoritycontract: malformed: s length")

	const refused = "Zq9 refused"
	for _, err := range []error{
		kStr(3).checkString(refused),
		kS36.checkString(refused),
		kE("x").checkString(refused),
		kB(2).checkBytes([]byte(refused)),
		func() error { _, err := decodeB64(refused); return err }(),
		func() error { _, err := parseU(refused); return err }(),
		en2([]byte(refused)),
	} {
		require.ErrorIs(t, err, ErrMalformed)
		require.NotContains(t, err.Error(), "Zq9")
		require.NotContains(t, err.Error(), "refused")
	}
}

// s36 is lowercase canonical form only; uuid.Parse alone also admits braces,
// urn: and hyphenless forms, which canonicalUUID's round-trip refuses.
func TestCanonicalUUID(t *testing.T) {
	const v4 = "0f8fad5b-d9cb-469f-a165-70867728950e"
	id, ok := canonicalUUID(v4)
	require.True(t, ok)
	require.Equal(t, v4, id.String())

	for _, bad := range []string{
		"", strings.ToUpper(v4), "{" + v4 + "}", "urn:uuid:" + v4,
		strings.ReplaceAll(v4, "-", ""), v4 + "0", v4[:35],
	} {
		_, ok := canonicalUUID(bad)
		require.False(t, ok, "%q", bad)
	}
}

// s36v4 needs both the version nibble and the RFC 4122 variant.
func TestIsUUIDv4(t *testing.T) {
	require.True(t, isUUIDv4("0f8fad5b-d9cb-469f-a165-70867728950e"))
	require.False(t, isUUIDv4("6ba7b810-9dad-11d1-80b4-00c04fd430c8"), "version 1")
	require.False(t, isUUIDv4("0f8fad5b-d9cb-569f-a165-70867728950e"), "version 5")
	require.False(t, isUUIDv4("0f8fad5b-d9cb-469f-0165-70867728950e"), "version 4, NCS variant")
	require.False(t, isUUIDv4("0F8FAD5B-D9CB-469F-A165-70867728950E"), "not lowercase")
	require.False(t, isUUIDv4(""))
}

// EN3 types u, u>0 / u≥1 and u01 differ only by range: parseU admits "0"
// everywhere, so the kind is what refuses it for u>0.
func TestUintKinds(t *testing.T) {
	for _, c := range []struct {
		name     string
		k        kind
		min, max uint64
	}{
		{"u", kU, 0, MaxSafeInt},
		{"u>0", kU1, 1, MaxSafeInt},
		{"u01", kU01, 0, 1},
	} {
		require.Equal(t, tagUint, c.k.tag, c.name)
		require.Equal(t, c.min, c.k.minU, c.name)
		require.Equal(t, c.max, c.k.maxU, c.name)
	}
	zero, err := parseU("0")
	require.NoError(t, err)
	require.Less(t, zero, kU1.minU, "only the kind refuses 0 at a u>0 position")
	require.Equal(t, 16, kU1.maxEncodedLen())
}

func TestKindConstructors(t *testing.T) {
	require.Equal(t, kind{tag: tagStr, maxLen: 8}, kStr(8))
	require.Equal(t, kind{tag: tagUUID}, kS36)
	require.Equal(t, kind{tag: tagUUIDv4}, kS36v4)
	require.Equal(t, []int{2}, kB(2).sizes)
	require.Equal(t, []int{96, 512}, kBOneOf(96, 512).sizes)
	require.Equal(t, 3, kBUpTo(3).upTo)
	require.Empty(t, kBUpTo(3).sizes)
	require.Equal(t, []string{"", "x"}, kE("", "x").enum)

	arr := kA(kS36, 1, 4)
	require.Equal(t, tagArr, arr.tag)
	require.Equal(t, 1, arr.minN)
	require.Equal(t, 4, arr.maxN)
	require.Equal(t, kS36, *arr.elem)

	lst := kL(kStr(4), 0, -1)
	require.Equal(t, tagList, lst.tag)
	require.Equal(t, 0, lst.minN)
	require.Equal(t, -1, lst.maxN)
	require.Equal(t, kStr(4), *lst.elem)
}

// The value-receiver builders return a modified copy and leave the receiver
// alone, because the package-level kinds (kS36, kU, ...) are shared.
func TestKindBuildersDoNotMutateTheReceiver(t *testing.T) {
	base := kB(2)
	require.NoError(t, base.OrEmpty().checkBytes(nil))
	require.ErrorIs(t, base.checkBytes(nil), ErrMalformed)

	rule := func(string) error { return malformed("rule") }
	require.ErrorIs(t, kStr(3).withStr(rule).checkString("abc"), ErrMalformed)
	require.NoError(t, kStr(3).checkString("abc"))

	bin := func([]byte) error { return malformed("rule") }
	require.ErrorIs(t, base.withBin(bin).checkBytes([]byte{1, 2}), ErrMalformed)
	require.NoError(t, base.checkBytes([]byte{1, 2}))
}

// withStr adds a rule after the base rules; it never replaces them and it
// never sees the empty value of an OrEmpty position.
func TestWithStrRunsAfterTheBaseRules(t *testing.T) {
	calls := 0
	noLeadingZero := func(s string) error {
		calls++
		if s[0] == '0' {
			return malformed("leading zero")
		}
		return nil
	}
	k := kStr(4).withStr(noLeadingZero)

	require.NoError(t, k.checkString("1234"))
	require.Equal(t, 1, calls)
	require.ErrorIs(t, k.checkString("0123"), ErrMalformed)
	require.Equal(t, 2, calls)

	calls = 0
	require.ErrorIs(t, k.checkString("12345"), ErrMalformed, "length is checked first")
	require.ErrorIs(t, k.checkString("1 3"), ErrMalformed, "alphabet is checked first")
	require.ErrorIs(t, k.checkString(""), ErrMalformed, "empty is refused unless the position allows it")
	require.Zero(t, calls, "the extra rule must not run on a value the base rules refused")

	require.NoError(t, k.OrEmpty().checkString(""))
	require.Zero(t, calls, "the extra rule must not run on the empty value (it indexes s[0])")
}

// withBin is the same for decoded bytes, e.g. P-384 point validity in Task 3.
func TestWithBinRunsAfterTheBaseRules(t *testing.T) {
	calls := 0
	prefix04 := func(b []byte) error {
		calls++
		if b[0] != 0x04 {
			return malformed("prefix")
		}
		return nil
	}
	k := kB(3).withBin(prefix04)

	require.NoError(t, k.checkBytes([]byte{4, 1, 2}))
	require.Equal(t, 1, calls)
	require.ErrorIs(t, k.checkBytes([]byte{6, 1, 2}), ErrMalformed)
	require.Equal(t, 2, calls)

	calls = 0
	require.ErrorIs(t, k.checkBytes([]byte{4, 1}), ErrMalformed, "size is checked first")
	require.ErrorIs(t, k.checkBytes(nil), ErrMalformed, "empty is refused unless the position allows it")
	require.Zero(t, calls, "the extra rule must not run on a value the base rules refused")

	require.NoError(t, k.OrEmpty().checkBytes(nil))
	require.Zero(t, calls, "the extra rule must not run on the empty value (it indexes b[0])")

	upTo := kBUpTo(2).withBin(prefix04)
	require.NoError(t, upTo.checkBytes([]byte{4}))
	require.ErrorIs(t, upTo.checkBytes([]byte{5}), ErrMalformed)
}

// checkString answers only for string-valued kinds; a number, bytes or
// container position refuses rather than passing vacuously.
func TestCheckStringRefusesNonStringKinds(t *testing.T) {
	for name, k := range map[string]kind{
		"u": kU, "u01": kU01, "b2": kB(2), "a": kA(kS36, 0, 1), "l": kL(kStr(4), 0, 1), "zero kind": {},
	} {
		require.ErrorIs(t, k.checkString("1"), ErrMalformed, name)
	}
	require.Equal(t, -1, kind{}.maxEncodedLen(), "an unknown kind has no bound")
}
