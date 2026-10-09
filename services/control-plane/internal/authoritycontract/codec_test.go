package authoritycontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testMsg exercises every kind; it is not registered.
type testMsg struct {
	Name  string
	Count uint64
	Flag  uint64
	Blob  []byte
	Opt   []byte
	Mode  string
	Set   []string
	Seq   []string
	Bins  [][]byte
}

func (m *testMsg) spec() (string, []field) {
	return "test-msg", []field{
		{"name", kStr(8), &m.Name},
		{"count", kU1, &m.Count},
		{"flag", kU01, &m.Flag},
		{"blob", kB(4), &m.Blob},
		{"opt", kBUpTo(6).OrEmpty(), &m.Opt},
		{"mode", kE("", "x", "yy"), &m.Mode},
		{"set", kA(kS36, 0, 3), &m.Set},
		{"seq", kL(kStr(4), 1, 2), &m.Seq},
		{"bins", kA(kB(2), 0, 2), &m.Bins},
	}
}

func (m testMsg) Encode() []byte { return encode(&m) }

const (
	idA = "0f8fad5b-d9cb-469f-a165-70867728950e"
	idB = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

func validTestMsg() testMsg {
	return testMsg{Name: "ab", Count: 7, Flag: 1, Blob: []byte{1, 2, 3, 4}, Mode: "x",
		Set: []string{idB, idA}, Seq: []string{"z", "a"}, Bins: [][]byte{{9, 9}, {1, 1}}}
}

const validTestBytes = `["concord-account-authority",1,"test-msg","ab",7,1,"AQIDBA==","","x",` +
	`["0f8fad5b-d9cb-469f-a165-70867728950e","7c9e6679-7425-40de-944b-e07fc1f90ae7"],["z","a"],["AQE=","CQk="]]`

func TestEncodeWritesCanonicalBytesAndSortsArrays(t *testing.T) {
	m := validTestMsg()
	require.Equal(t, validTestBytes, string(m.Encode()))
}

func TestDecodeRoundTrip(t *testing.T) {
	got, err := decodeAs[testMsg]([]byte(validTestBytes), 0)
	require.NoError(t, err)
	require.Equal(t, validTestBytes, string(got.Encode()))
	require.Equal(t, []string{"z", "a"}, got.Seq, "l keeps layout order")
}

func TestEncodeRefusesInvalidValuesWithNil(t *testing.T) {
	for name, mut := range map[string]func(*testMsg){
		"count zero":       func(m *testMsg) { m.Count = 0 },
		"flag 2":           func(m *testMsg) { m.Flag = 2 },
		"blob size":        func(m *testMsg) { m.Blob = []byte{1} },
		"quote injection":  func(m *testMsg) { m.Name = `a","b` },
		"duplicate a item": func(m *testMsg) { m.Set = []string{idA, idA} },
		"list too long":    func(m *testMsg) { m.Seq = []string{"a", "b", "c"} },
		"list too short":   func(m *testMsg) { m.Seq = nil },
		"empty item":       func(m *testMsg) { m.Seq = []string{""} },
		"over 2^53":        func(m *testMsg) { m.Count = MaxSafeInt + 1 },
	} {
		m := validTestMsg()
		mut(&m)
		require.Nil(t, m.Encode(), name)
		_, err := marshal(&m)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

// An array item is written between quotes with no escaping, so the writer must
// check every item against its element kind, as it checks a scalar position:
// otherwise a crafted item could close its own string and add elements.
func TestEncodeChecksEveryArrayItem(t *testing.T) {
	for name, mut := range map[string]func(*testMsg){
		"l item injection":   func(m *testMsg) { m.Seq = []string{`a","b`} },
		"l item over length": func(m *testMsg) { m.Seq = []string{"abcde"} },
		"a item not a uuid":  func(m *testMsg) { m.Set = []string{idA, "not-a-uuid"} },
		"a item injection":   func(m *testMsg) { m.Set = []string{idA + `","x`} },
		"a bytes wrong size": func(m *testMsg) { m.Bins = [][]byte{{1, 1}, {1}} },
		"a bytes empty item": func(m *testMsg) { m.Bins = [][]byte{{1, 1}, {}} },
		"a bytes too many":   func(m *testMsg) { m.Bins = [][]byte{{1, 1}, {2, 2}, {3, 3}} },
	} {
		m := validTestMsg()
		mut(&m)
		require.Nil(t, m.Encode(), name)
		_, err := marshal(&m)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

// jsonEscapedB is JSON's six-byte spelling of "b": a backslash and u0062. It is
// built from two pieces on purpose. A tool that decodes escapes in source text
// turns a written-out u0062 escape into a plain b, and the "escape" case then
// replaces "ab" with itself, accepts valid bytes and fails for the wrong
// reason. That is how the plan's own copy of this case came to be broken.
const jsonEscapedB = `\` + `u0062`

// The "escape" case spells "ab" with a JSON escape: valid JSON for the same
// string, so only EN2's refusal of the backslash stops it.
// TestDecodeEscapeIsRefusedByEN2 pins that reason.
func TestDecodeRefusals(t *testing.T) {
	v := validTestBytes
	cases := map[string]string{
		"2^53":             strings.Replace(v, `,7,`, `,9007199254740992,`, 1),
		"1.0":              strings.Replace(v, `,7,`, `,7.0,`, 1),
		"1e0":              strings.Replace(v, `,7,`, `,7e0,`, 1),
		"negative":         strings.Replace(v, `,7,`, `,-7,`, 1),
		"string for u":     strings.Replace(v, `,7,`, `,"7",`, 1),
		"BOM":              "\xef\xbb\xbf" + v,
		"space":            strings.Replace(v, `1,"test-msg"`, `1, "test-msg"`, 1),
		"escape":           strings.Replace(v, `"ab"`, `"a`+jsonEscapedB+`"`, 1),
		"non-ASCII":        strings.Replace(v, `"ab"`, "\"a\xc3\xa9\"", 1),
		"invalid UTF-8":    strings.Replace(v, `"ab"`, "\"a\xff\"", 1),
		"version 2":        strings.Replace(v, `",1,"`, `",2,"`, 1),
		"version 1.0":      strings.Replace(v, `",1,"`, `",1.0,"`, 1),
		"prefix":           strings.Replace(v, `concord-account-authority`, `concord-account-authorit`, 1),
		"type":             strings.Replace(v, `"test-msg"`, `"test-msh"`, 1),
		"extra element":    strings.TrimSuffix(v, "]") + `,"x"]`,
		"missing element":  strings.Replace(v, `,["AQE=","CQk="]]`, `]`, 1),
		"trailing bytes":   v + `]`,
		"unsorted a":       strings.Replace(v, `["AQE=","CQk="]`, `["CQk=","AQE="]`, 1),
		"duplicate a":      strings.Replace(v, `["AQE=","CQk="]`, `["AQE=","AQE="]`, 1),
		"nested array":     strings.Replace(v, `"ab"`, `["ab"]`, 1),
		"array at a":       strings.Replace(v, `["z","a"]`, `[["z"],"a"]`, 1),
		"bool":             strings.Replace(v, `,1,"AQIDBA=="`, `,true,"AQIDBA=="`, 1),
		"null":             strings.Replace(v, `"ab"`, `null`, 1),
		"Strict bits":      strings.Replace(v, `"AQE="`, `"AQF="`, 1),
		"missing padding":  strings.Replace(v, `"AQIDBA=="`, `"AQIDBA"`, 1),
		"newline in b64":   strings.Replace(v, `"AQIDBA=="`, "\"AQID\nBA==\"", 1),
		"empty non-opt":    strings.Replace(v, `"AQIDBA=="`, `""`, 1),
		"enum outside set": strings.Replace(v, `,"x",`, `,"z",`, 1),
		"object":           `{"a":1}`,
	}
	require.Len(t, cases, 29)
	for name, in := range cases {
		_, err := decodeAs[testMsg]([]byte(in), 0)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

// The "escape" case above would also be caught by the step-6 re-encode
// comparison, so on its own it cannot show EN2 doing its job. This one names
// the rule: the input is valid JSON that parses to the very string the layout
// wants, and the refusal must come from EN2's byte filter.
func TestDecodeEscapeIsRefusedByEN2(t *testing.T) {
	require.Contains(t, validTestBytes, `"ab"`, "the value under replacement is in the valid bytes")
	in := strings.Replace(validTestBytes, `"ab"`, `"a`+jsonEscapedB+`"`, 1)
	require.NotEqual(t, validTestBytes, in, "the replacement must land")
	require.Contains(t, in, `\`, "and carry the backslash EN2 excludes")
	var arr []any
	require.NoError(t, json.Unmarshal([]byte(in), &arr), "the escape is valid JSON")
	require.Equal(t, "ab", arr[3], "and it spells the same string")

	_, err := decodeAs[testMsg]([]byte(in), 0)
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "EN2 byte")
}

// EN5 step 6 re-encodes the decoded value, so a position that slips past a
// decode-side check is usually refused there instead and the end-to-end cases
// cannot tell which check did the work. These call the step-5 and step-4
// helpers directly so each check has a test of its own.
func TestReadUintEnforcesTheKindsRange(t *testing.T) {
	var x uint64
	for _, tc := range []struct {
		k  kind
		in string
		ok bool
	}{
		{kU1, "0", false}, {kU1, "1", true}, {kU1, "9007199254740991", true},
		{kU01, "0", true}, {kU01, "1", true}, {kU01, "2", false},
		{kU, "0", true}, {kU, "9007199254740991", true},
		{kStr(2), "1", false},
	} {
		err := readUint(tc.k, json.Number(tc.in), &x)
		if tc.ok {
			require.NoError(t, err, tc.in)
		} else {
			require.ErrorIs(t, err, ErrMalformed, tc.in)
		}
	}
	require.ErrorIs(t, readUint(kU, "7", &x), ErrMalformed, "a string is not a number")
}

func TestReadItemsEnforcesCountOrderAndElementType(t *testing.T) {
	a, l := kA(kStr(2), 1, 3), kL(kStr(2), 1, 3)
	for _, tc := range []struct {
		name string
		k    kind
		v    any
		ok   bool
	}{
		{"a ascending", a, []any{"a", "b"}, true},
		{"a descending", a, []any{"b", "a"}, false},
		{"a duplicate", a, []any{"a", "a"}, false},
		{"l keeps any order", l, []any{"b", "a"}, true},
		{"l allows a repeat", l, []any{"a", "a"}, true},
		{"below the minimum", a, []any{}, false},
		{"above the maximum", l, []any{"a", "b", "c", "d"}, false},
		{"empty item", l, []any{""}, false},
		{"nested array", l, []any{[]any{"a"}}, false},
		{"number item", l, []any{json.Number("1")}, false},
		{"not an array", a, "a", false},
		{"not an array kind", kStr(2), []any{"a"}, false},
	} {
		_, err := readItems(tc.k, tc.v)
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.ErrorIs(t, err, ErrMalformed, tc.name)
		}
	}
}

func TestCheckHeadEnforcesCountPrefixVersionAndType(t *testing.T) {
	ok := func() []any { return []any{Prefix, json.Number("1"), "t", "x"} }
	require.NoError(t, checkHead(ok(), "t", 1))

	bad := map[string]func([]any) []any{
		"short":          func(a []any) []any { return a[:3] },
		"long":           func(a []any) []any { return append(a, "y") },
		"prefix":         func(a []any) []any { a[0] = "concord-account-authorit"; return a },
		"prefix type":    func(a []any) []any { a[0] = json.Number("1"); return a },
		"version":        func(a []any) []any { a[1] = json.Number("2"); return a },
		"version spelt":  func(a []any) []any { a[1] = json.Number("1.0"); return a },
		"version string": func(a []any) []any { a[1] = "1"; return a },
		"type":           func(a []any) []any { a[2] = "u"; return a },
		"type type":      func(a []any) []any { a[2] = json.Number("1"); return a },
	}
	for name, mut := range bad {
		require.ErrorIs(t, checkHead(mut(ok()), "t", 1), ErrMalformed, name)
	}
}

func TestStep5ReadersValidateEachValue(t *testing.T) {
	var (
		str string
		b   []byte
		ss  []string
		bb  [][]byte
	)
	// A string position.
	require.ErrorIs(t, readField(field{"f", kS36, &str}, "not-a-uuid"), ErrMalformed)
	require.ErrorIs(t, readField(field{"f", kS36, &str}, json.Number("1")), ErrMalformed, "a number is not a string")
	require.NoError(t, readField(field{"f", kS36, &str}, idA))
	require.Equal(t, idA, str)

	// A b position: "" needs OrEmpty, and the size is the kind's.
	require.ErrorIs(t, readBytes(kB(4), "", &b), ErrMalformed, "empty without OrEmpty")
	require.NoError(t, readBytes(kBUpTo(6).OrEmpty(), "", &b))
	require.Nil(t, b)
	require.ErrorIs(t, readBytes(kB(4), "AQI=", &b), ErrMalformed, "2 bytes under b4")
	require.ErrorIs(t, readBytes(kB(4), json.Number("1"), &b), ErrMalformed, "a number is not a string")
	require.NoError(t, readBytes(kB(4), "AQIDBA==", &b))
	require.Equal(t, []byte{1, 2, 3, 4}, b)

	// An a/l of s: every item is checked against the element kind.
	require.ErrorIs(t, readStrings(kA(kS36, 0, 3), []any{"not-a-uuid"}, &ss), ErrMalformed)
	require.NoError(t, readStrings(kA(kS36, 0, 3), []any{idA}, &ss))
	require.Equal(t, []string{idA}, ss)

	// An a/l of b: every item is decoded canonically and sized.
	require.ErrorIs(t, readBins(kA(kB(2), 0, 2), []any{"AQ=="}, &bb), ErrMalformed, "1 byte under b2")
	require.ErrorIs(t, readBins(kA(kB(2), 0, 2), []any{"AQF="}, &bb), ErrMalformed, "non-canonical base64")
	require.NoError(t, readBins(kA(kB(2), 0, 2), []any{"AQE="}, &bb))
	require.Equal(t, [][]byte{{1, 1}}, bb)
}

func TestDecodedEmptyArraysAreNil(t *testing.T) {
	m := validTestMsg()
	m.Set, m.Bins = nil, nil
	got, err := decodeAs[testMsg](m.Encode(), 0)
	require.NoError(t, err)
	require.Nil(t, got.Set, "an empty a of strings decodes to nil, as Encode accepts")
	require.Nil(t, got.Bins, "an empty a of bytes decodes to nil, as Encode accepts")
	require.Nil(t, got.Opt, "and an empty optional b decodes to nil")
	require.Equal(t, m, got)
}

func TestDecodeRefusesDegenerateJSON(t *testing.T) {
	for name, in := range map[string]string{
		"empty":         ``,
		"open bracket":  `[`,
		"close bracket": `]`,
		"empty array":   `[]`,
		"bare null":     `null`,
		"bare string":   `"concord-account-authority"`,
		"bare number":   `1`,
		"deep and open": strings.Repeat("[", 200),
		"deep and shut": strings.Repeat("[", 100) + strings.Repeat("]", 100),
	} {
		_, err := decodeAs[testMsg]([]byte(in), 0)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

func TestMaxLenAndStandaloneCap(t *testing.T) {
	var m testMsg
	tag, fs := m.spec()
	require.Equal(t, 250, maxLen(tag, fs))
	require.Equal(t, 250, capOf(&m))
	_, err := decodeAs[testMsg](bytes.Repeat([]byte("A"), 251), 0)
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "length", "junk over the cap is refused on length, before it is parsed")
	_, err = decodeAs[testMsg]([]byte(validTestBytes), 10)
	require.ErrorIs(t, err, ErrMalformed, "an envelope-position cap below the length refuses at step 1")
}

func TestEnvelopePositionCapIsInclusive(t *testing.T) {
	n := len(validTestBytes)
	_, err := decodeAs[testMsg]([]byte(validTestBytes), n)
	require.NoError(t, err, "a cap equal to the length admits it")
	_, err = decodeAs[testMsg]([]byte(validTestBytes), n-1)
	require.ErrorIs(t, err, ErrMalformed, "one byte under refuses")
	require.ErrorContains(t, err, "length", "and it is step 1 that refuses")
}

// dynMsg is a test-only layout built from a tag and a field table, so the
// engine's defensive branches can be driven without a production layout.
type dynMsg struct {
	tag string
	fs  []field
}

func (m *dynMsg) spec() (string, []field) { return m.tag, m.fs }

func TestStandaloneCapsMatchSection23(t *testing.T) {
	// The literals are DoR §2.3's table, not the constants under test.
	require.Equal(t, map[string]int{
		"anchors":        1024,
		"signed":         6144,
		"device-set":     4608,
		"device-wrap":    6144,
		"device-intent":  2048,
		"device-bundle":  8192,
		"device-history": 180_224,
		"recovery-bind":  1024,
		"relay-recover":  10_278,
		"device-result":  1024,
		"device-secrets": 4080,
		"trust":          2_097_152,
		"chain-head":     16_777_216,
		"device-head":    16_777_216,
	}, standaloneCaps)
}

func TestCapOfPrefersTheStandaloneTable(t *testing.T) {
	var s string
	fs := []field{{"s", kStr(2000), &s}}

	listed := &dynMsg{tag: "anchors", fs: fs}
	require.Equal(t, CapAnchors, capOf(listed), "a listed tag takes the explicit cap")

	unlisted := &dynMsg{tag: "not-listed", fs: fs}
	tag, f := unlisted.spec()
	require.Equal(t, maxLen(tag, f), capOf(unlisted), "any other tag takes the sum of its position caps")
	require.Greater(t, capOf(unlisted), CapAnchors)
}

func TestMarshalEnforcesTheStandaloneCap(t *testing.T) {
	s := strings.Repeat("a", CapAnchors)
	m := &dynMsg{tag: "anchors", fs: []field{{"s", kStr(2000), &s}}}
	_, err := marshal(m)
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "length")

	s = "a"
	b, err := marshal(m)
	require.NoError(t, err)
	require.Equal(t, `["concord-account-authority",1,"anchors","a"]`, string(b))
}

func TestUnboundedListHasNoLength(t *testing.T) {
	var items []string
	m := &dynMsg{tag: "unbounded", fs: []field{{"items", kL(kStr(4), 0, -1), &items}}}
	tag, fs := m.spec()
	require.Equal(t, -1, maxLen(tag, fs), "an item-uncapped list leaves the layout unbounded")
	require.Equal(t, -1, capOf(m))

	items = []string{"a", "b", "c"}
	b, err := marshal(m)
	require.NoError(t, err)
	require.Equal(t, `["concord-account-authority",1,"unbounded",["a","b","c"]]`, string(b))

	items = nil
	require.NoError(t, unmarshal(m, b, 0))
	require.Equal(t, []string{"a", "b", "c"}, items)
}

// A layout table that pairs a kind with a pointer it cannot carry is a
// programming error in a layout file; the engine must refuse it on both paths
// rather than write or accept a value of the wrong shape.
func TestFieldKindAndPointerMustAgree(t *testing.T) {
	var (
		s      string
		b      []byte
		ss     []string
		bb     [][]byte
		n      int
		u0, u2 uint64 = 0, 2
	)
	for name, tc := range map[string]struct {
		f  field
		in string
	}{
		"string under u":        {field{"f", kU1, &s}, `"x"`},
		"u under s":             {field{"f", kStr(2), &u2}, `7`},
		"bytes under s":         {field{"f", kStr(2), &b}, `"x"`},
		"strings under bytes":   {field{"f", kBUpTo(2), &ss}, `["x"]`},
		"strings under b array": {field{"f", kA(kB(2), 0, 2), &ss}, `["x"]`},
		"bins under s array":    {field{"f", kA(kStr(2), 0, 2), &bb}, `["x"]`},
		"bins under bytes":      {field{"f", kBUpTo(2), &bb}, `["x"]`},
		"unsupported pointer":   {field{"f", kU1, &n}, `1`},
		"u above its maximum":   {field{"f", kU01, &u2}, `2`},
		"u below its minimum":   {field{"f", kU1, &u0}, `0`},
		"array below its min":   {field{"f", kA(kStr(2), 1, 2), &ss}, `[]`},
	} {
		m := &dynMsg{tag: "mismatch", fs: []field{tc.f}}
		_, err := marshal(m)
		require.ErrorIs(t, err, ErrMalformed, "marshal: "+name)

		in := `["concord-account-authority",1,"mismatch",` + tc.in + `]`
		require.ErrorIs(t, unmarshal(m, []byte(in), 4096), ErrMalformed, "unmarshal: "+name)
	}
}

// crossMsg carries a rule over its own positions, as the A1 layouts that the
// DoR names malformed do.
type crossMsg struct{ A, B uint64 }

func (m *crossMsg) spec() (string, []field) {
	return "cross-msg", []field{{"a", kU, &m.A}, {"b", kU, &m.B}}
}

func (m *crossMsg) crossCheck() error {
	if m.A == m.B {
		return malformed("a equals b")
	}
	return nil
}

func TestCrossCheckRunsOnEncodeAndDecode(t *testing.T) {
	good := crossMsg{A: 5, B: 6}
	b, err := marshal(&good)
	require.NoError(t, err)
	require.Equal(t, `["concord-account-authority",1,"cross-msg",5,6]`, string(b))
	var back crossMsg
	require.NoError(t, unmarshal(&back, b, 0))
	require.Equal(t, good, back)

	bad := crossMsg{A: 5, B: 5}
	_, err = marshal(&bad)
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "a equals b")

	// Every position is individually valid and the bytes are canonical, so
	// only the cross-check can refuse this on the way in.
	var refused crossMsg
	err = unmarshal(&refused, []byte(`["concord-account-authority",1,"cross-msg",5,5]`), 0)
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "a equals b")
}

// tinyMsg is a second test-only layout. Registered under several tags it gives
// the registry enough entries for the order of Tags to mean something.
type tinyMsg struct {
	tag string
	N   uint64
}

func (m *tinyMsg) spec() (string, []field) { return m.tag, []field{{"n", kU, &m.N}} }

func (m tinyMsg) Encode() []byte { return encode(&m) }

func TestDecodeUnknownTag(t *testing.T) {
	_, err := Decode("no-such-layout", []byte(validTestBytes))
	require.ErrorIs(t, err, ErrMalformed)
}

// register has no production caller until the layout files land, and the
// registry is a package global: this test registers test-only layouts and
// removes them again, because the registry must hold exactly the production
// tags when the completeness test counts them.
func TestRegisterDecodeMarshalAndTags(t *testing.T) {
	testTag, _ := (&testMsg{}).spec()
	tinyTags := []string{"test-tiny-zz", "test-tiny-bb", "test-tiny-mm", "test-tiny-aa", "test-tiny-qq", "test-tiny-kk"}
	all := append([]string{testTag}, tinyTags...)
	for _, tag := range all {
		require.NotContains(t, registry, tag, "another test left its layout behind")
	}

	ctors := make([]func() Message, 0, len(all))
	ctors = append(ctors, func() Message { return &testMsg{} })
	for _, tag := range tinyTags {
		ctors = append(ctors, func() Message { return &tinyMsg{tag: tag} })
	}
	register(ctors...)
	t.Cleanup(func() {
		for _, tag := range all {
			delete(registry, tag)
		}
	})

	tags := Tags()
	for _, tag := range all {
		require.Contains(t, tags, tag, "every registered layout is listed")
	}
	require.True(t, slices.IsSorted(tags), "in tag order, not registration or map order: %v", tags)

	m, err := Decode(testTag, []byte(validTestBytes))
	require.NoError(t, err)
	b, err := Marshal(m)
	require.NoError(t, err)
	require.Equal(t, validTestBytes, string(b), "Decode then Marshal is the identity on canonical bytes")
	require.Equal(t, validTestBytes, string(m.Encode()))

	_, err = Decode(testTag, []byte(strings.Replace(validTestBytes, `,7,`, `,07,`, 1)))
	require.ErrorIs(t, err, ErrMalformed, "a registered layout still decodes strictly")

	_, err = Decode(testTag, bytes.Repeat([]byte("A"), 251))
	require.ErrorContains(t, err, "length", "Decode applies the layout's standalone cap at step 1")

	_, err = Decode(testTag, []byte(`["concord-account-authority",1,"test-tiny-mm",1]`))
	require.ErrorIs(t, err, ErrMalformed, "and the tag in the bytes must be the tag asked for")

	got, err := Decode("test-tiny-mm", []byte(`["concord-account-authority",1,"test-tiny-mm",3]`))
	require.NoError(t, err)
	tiny, ok := got.(*tinyMsg)
	require.True(t, ok, "the constructor's concrete type comes back")
	require.Equal(t, uint64(3), tiny.N)

	require.PanicsWithValue(t, "authoritycontract: duplicate layout "+testTag, func() {
		register(func() Message { return &testMsg{} })
	}, "a duplicate tag is a programming error")
}

// errEchoesValue stands in for a library error that echoes its input, as
// strconv's does. Its text must never reach a codec error.
var errEchoesValue = errors.New("echoes-a-value 4242")

// plainErrMsg is a test-only layout whose three hooks (a string rule, a bytes
// rule, and a cross-position rule) all return an error that is NOT ErrMalformed,
// the way a single careless `return err` from a library call in a real layout
// would.
type plainErrMsg struct {
	Name string
	Raw  []byte
	N    uint64
}

func (m *plainErrMsg) spec() (string, []field) {
	badName := func(s string) error {
		if s == "bad" {
			return errEchoesValue
		}
		return nil
	}
	badRaw := func(b []byte) error {
		if b[0] == 0 {
			return errEchoesValue
		}
		return nil
	}
	return "test-plain-err", []field{
		{"name", kStr(4).withStr(badName), &m.Name},
		{"raw", kB(2).withBin(badRaw), &m.Raw},
		{"n", kU, &m.N},
	}
}

func (m *plainErrMsg) crossCheck() error {
	if m.N == 9 {
		return errEchoesValue
	}
	return nil
}

func (m plainErrMsg) Encode() []byte { return encode(&m) }

// EN5 says any failure is malformed, and A2 maps errors.Is(err, ErrMalformed)
// to Reason malformed. The engine, not each layout author, must guarantee it.
func TestEveryHookFailureIsMalformedAndEchoesNothing(t *testing.T) {
	const tag = "test-plain-err"
	require.NotContains(t, registry, tag, "another test left its layout behind")
	register(func() Message { return &plainErrMsg{} })
	t.Cleanup(func() { delete(registry, tag) })

	head := `["concord-account-authority",1,"test-plain-err",`
	valid := head + `"ok","AQI=",1]`
	m, err := Decode(tag, []byte(valid))
	require.NoError(t, err, "the layout itself is valid")
	require.Equal(t, valid, string(m.Encode()))

	for name, tc := range map[string]struct {
		msg   plainErrMsg
		bytes string
	}{
		"strChk hook": {plainErrMsg{Name: "bad", Raw: []byte{1, 2}, N: 1}, head + `"bad","AQI=",1]`},
		"binChk hook": {plainErrMsg{Name: "ok", Raw: []byte{0, 2}, N: 1}, head + `"ok","AAI=",1]`},
		"crossCheck":  {plainErrMsg{Name: "ok", Raw: []byte{1, 2}, N: 9}, head + `"ok","AQI=",9]`},
	} {
		_, err := Marshal(&tc.msg)
		require.ErrorIs(t, err, ErrMalformed, "Marshal: "+name)
		require.NotContains(t, err.Error(), "echoes-a-value", "Marshal: "+name+" must not echo the hook's text")
		require.NotErrorIs(t, err, errEchoesValue, "Marshal: "+name)
		require.Nil(t, tc.msg.Encode(), "Encode: "+name)

		_, err = Decode(tag, []byte(tc.bytes))
		require.ErrorIs(t, err, ErrMalformed, "Decode: "+name)
		require.NotContains(t, err.Error(), "echoes-a-value", "Decode: "+name+" must not echo the hook's text")
	}

	// An error that already satisfies ErrMalformed keeps its own message.
	_, err = Decode(tag, []byte(head+`"ok","AQI=",9007199254740992]`))
	require.ErrorIs(t, err, ErrMalformed)
	require.ErrorContains(t, err, "u range", "an already-malformed error is not replaced by the generic one")
}

// A nil message is refused as malformed, never a panic: both the nil interface
// and a typed nil pointer. (A layout's Encode has a value receiver, so calling
// it through a nil pointer panics in the language before any code here runs.)
func TestMarshalRefusesNilMessages(t *testing.T) {
	for name, m := range map[string]Message{"nil interface": nil, "typed nil": (*Intent)(nil)} {
		require.NotPanics(t, func() {
			b, err := Marshal(m)
			require.Nil(t, b, name)
			require.ErrorIs(t, err, ErrMalformed, name)
		}, name)
	}
}
