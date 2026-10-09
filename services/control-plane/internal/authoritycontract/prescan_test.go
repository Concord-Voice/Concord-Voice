package authoritycontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Each case breaks one prescan rule on validTestBytes and leaves the rest
// intact, so the rule it names is the one that refuses it.
func TestPrescanRefusesEachShapeItGuards(t *testing.T) {
	var m testMsg
	_, fs := m.spec()
	v := validTestBytes
	require.NoError(t, prescan([]byte(v), fs), "the canonical bytes pass")

	for name, in := range map[string]string{
		"depth 3":                      strings.Replace(v, `["z","a"]`, `[["z"],"a"]`, 1),
		"depth 3, empty":               strings.Replace(v, `["z","a"]`, `[[],"a"]`, 1),
		"one value short":              strings.Replace(v, `,["AQE=","CQk="]]`, `]`, 1),
		"one value long":               strings.TrimSuffix(v, "]") + `,"x"]`,
		"array at a scalar position":   strings.Replace(v, `"ab"`, `[]`, 1),
		"array at the prefix":          strings.Replace(v, `"concord-account-authority"`, `[]`, 1),
		"array past the last position": strings.TrimSuffix(v, "]") + `,[]]`,
		"one item over maxN":           strings.Replace(v, `["z","a"]`, `["z","a","b"]`, 1),
		"trailing bytes":               v + `]`,
		"a second array":               v + `[]`,
		"not an array":                 `"x"`,
		"unterminated array":           strings.TrimSuffix(v, "]"),
		"unterminated string":          v[:20],
		"empty":                        ``,
	} {
		require.ErrorIs(t, prescan([]byte(in), fs), ErrMalformed, name)
	}
}

// Every shape below is one marshal can write, so prescan must pass it; the
// rest of EN5 decides. The last two are refused later, not by prescan.
func TestPrescanPassesWhatOnlyLaterStepsDecide(t *testing.T) {
	var m testMsg
	_, fs := m.spec()
	v := validTestBytes
	for name, in := range map[string]string{
		"an array at exactly maxN":  v, // seq is l<s4>[1..2] with two items
		"empty arrays":              strings.Replace(strings.Replace(v, `["AQE=","CQk="]`, `[]`, 1), `["0f8fad5b-d9cb-469f-a165-70867728950e","7c9e6679-7425-40de-944b-e07fc1f90ae7"]`, `[]`, 1),
		"brackets inside a string":  strings.Replace(v, `"ab"`, `"[[[,]]]"`, 1),
		"an array below its minN":   strings.Replace(v, `["z","a"]`, `[]`, 1),
		"a number at a string slot": strings.Replace(v, `"ab"`, `7`, 1),
	} {
		require.NoError(t, prescan([]byte(in), fs), name)
	}
}

func TestPrescanNamesThePositionItRefuses(t *testing.T) {
	var m testMsg
	_, fs := m.spec()
	err := prescan([]byte(strings.Replace(validTestBytes, `["z","a"]`, `["z","a","b"]`, 1)), fs)
	require.ErrorIs(t, err, ErrMalformed)
	require.True(t, strings.HasPrefix(err.Error(), "seq: "), err.Error())
	err = prescan([]byte(strings.Replace(validTestBytes, `"ab"`, `[]`, 1)), fs)
	require.True(t, strings.HasPrefix(err.Error(), "name: "), err.Error())
}

// unmarshalPreFix is unmarshal as it stood before prescan, kept as the
// differential oracle below. It returns the EN5 step that refused b, or 0.
func unmarshalPreFix(m specer, b []byte) (int, error) {
	tag, fs := m.spec()
	if limit := capOf(m); limit >= 0 && len(b) > limit {
		return 1, malformed("length")
	}
	if err := en2(b); err != nil {
		return 2, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return 3, malformed("json")
	}
	if err := checkHead(arr, tag, len(fs)); err != nil {
		return 4, err
	}
	for i, f := range fs {
		if err := readField(f, arr[3+i]); err != nil {
			return 5, fmt.Errorf("%s: %w", f.name, asMalformed(err))
		}
	}
	re, err := marshal(m)
	if err != nil {
		return 6, err
	}
	if !bytes.Equal(re, b) {
		return 6, malformed("re-encode")
	}
	return 0, nil
}

// diffStats counts verdicts: accepts, the step the pre-fix decoder refused
// at, and the prescan rule that pre-empted it.
type diffStats struct {
	n, accepts, acceptsAtMaxN int
	byStep                    [7]int
	byPrescan                 map[string]int
}

// differential feeds n generated inputs, round-robin over every tag, to the
// decoder and to unmarshalPreFix, and fails on any verdict or re-encode that
// differs.
func differential(t *testing.T, seed uint64, n int) diffStats {
	t.Helper()
	g := newInputGen(t, seed)
	tags := Tags()
	st := diffStats{n: n, byPrescan: map[string]int{}}
	for i := range n {
		tag := tags[i%len(tags)]
		b := g.next(tag)
		got, want := registry[tag](), registry[tag]()
		err := unmarshal(got, b, 0)
		step, wantErr := unmarshalPreFix(want, b)
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("verdicts differ for %s (%d bytes): prescan decoder %v, pre-fix decoder %v\n%.400q",
				tag, len(b), err, wantErr, b)
		}
		if err != nil {
			require.ErrorIs(t, err, ErrMalformed)
			st.byStep[step]++
			if step > 2 {
				if perr := prescan(b, specFields(got)); perr != nil {
					msg := perr.Error() // the rule, without the position name
					st.byPrescan[msg[strings.LastIndex(msg, ": ")+2:]]++
				}
			}
			continue
		}
		st.accepts++
		if atMaxN(b, specFields(got)) {
			st.acceptsAtMaxN++
		}
		require.Equal(t, b, got.Encode(), "an accept re-encodes to its input")
		require.Equal(t, want.Encode(), got.Encode(), "both decoders decoded the same message")
	}
	return st
}

// atMaxN reports whether an accepted message has an array position holding
// exactly its maxN items, the accept side of the item count rule's boundary.
func atMaxN(b []byte, fs []field) bool {
	var arr []any
	if json.Unmarshal(b, &arr) != nil {
		return false
	}
	for i, f := range fs {
		if a, ok := arr[3+i].([]any); ok && isArray(f.k) && f.k.maxN > 0 && len(a) == f.k.maxN {
			return true
		}
	}
	return false
}

func specFields(m Message) []field {
	_, fs := m.spec()
	return fs
}

// TestPrescanRefusesOnlyWhatTheDecoderRefuses is the strict-subset proof:
// the decoder with prescan accepts exactly what the decoder without it
// accepted, and decodes each accept to the same message.
func TestPrescanRefusesOnlyWhatTheDecoderRefuses(t *testing.T) {
	st := differential(t, 3587, 20_000)
	require.Positive(t, st.accepts, "the generator must reach accepts")
	require.Positive(t, st.acceptsAtMaxN, "including arrays at exactly maxN")
	require.Positive(t, st.byStep[5]+st.byStep[6], "and refusals after step 4")
	require.NotEmpty(t, st.byPrescan, "and refusals prescan makes first")
}

// inputGen builds decoder inputs from the vectors: positives parsed as trees
// and mutated structurally, any vector mutated bytewise, and inputs built
// from a layout's field table. Every byte is from EN2's alphabet.
type inputGen struct {
	r     *prescanRand
	trees map[string][][]any
	raw   map[string][][]byte
}

const genChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:/+=-"

type rawTok string // a bare JSON token that is not a number: true, null

// prescanRand is a seeded splitmix64 stream. The differential needs inputs
// that reproduce from a seed, not inputs nobody can predict, so a
// cryptographic source would only make a failure harder to replay.
type prescanRand struct{ s uint64 }

// Intn returns a value in [0, n). The modulo bias is irrelevant here.
func (r *prescanRand) Intn(n int) int {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z>>33) % n
}

func newInputGen(t *testing.T, seed uint64) *inputGen {
	t.Helper()
	g := &inputGen{r: &prescanRand{s: seed}, trees: map[string][][]any{}, raw: map[string][][]byte{}}
	for _, v := range loadVectors(t).Vectors {
		if _, ok := registry[v.Type]; !ok || strings.HasPrefix(v.Name, "prim/") {
			continue
		}
		b := mustB64(t, v.BytesB64)
		g.raw[v.Type] = append(g.raw[v.Type], b)
		if !wantOK(v) {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var arr []any
		require.NoError(t, dec.Decode(&arr), v.Name)
		g.trees[v.Type] = append(g.trees[v.Type], arr)
	}
	return g
}

func (g *inputGen) next(tag string) []byte {
	switch g.r.Intn(10) {
	case 0, 1:
		return g.tree(tag, 0)
	case 2, 3, 4, 5:
		return g.tree(tag, 1+g.r.Intn(3))
	case 6, 7:
		return g.edit(g.pick(g.raw[tag]), 1+g.r.Intn(4))
	case 8:
		return g.grammar(tag)
	}
	return g.edit(g.tree(tag, 1+g.r.Intn(2)), 1+g.r.Intn(2))
}

func (g *inputGen) pick(bs [][]byte) []byte {
	if len(bs) == 0 {
		return []byte(encodedPrefix + `"x"]`)
	}
	return bs[g.r.Intn(len(bs))]
}

func (g *inputGen) tree(tag string, k int) []byte {
	ts := g.trees[tag]
	if len(ts) == 0 {
		return g.grammar(tag)
	}
	top := copyArray(ts[g.r.Intn(len(ts))])
	fs := specFields(registry[tag]())
	for range k {
		top = g.mutate(top, fs)
	}
	var buf bytes.Buffer
	writeValue(&buf, top)
	return buf.Bytes()
}

func deepCopy(v any) any {
	if a, ok := v.([]any); ok {
		return copyArray(a)
	}
	return v
}

func copyArray(a []any) []any {
	out := make([]any, len(a))
	for i, e := range a {
		out[i] = deepCopy(e)
	}
	return out
}

func writeValue(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case string:
		buf.WriteString(`"` + x + `"`)
	case json.Number:
		buf.WriteString(string(x))
	case rawTok:
		buf.WriteString(string(x))
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeValue(buf, e)
		}
		buf.WriteByte(']')
	}
}

// mutate applies one structural change. Several aim at the prescan rules:
// counts at maxN-1, maxN and maxN+1, arrays at scalar positions, depth 3 and
// deeper, and top-level values added or dropped.
func (g *inputGen) mutate(top []any, fs []field) []any {
	if len(top) == 0 {
		return []any{g.value(0)}
	}
	i := g.r.Intn(len(top))
	switch g.r.Intn(12) {
	case 0, 1:
		g.resize(top, fs)
	case 2:
		top[i] = []any{top[i]}
	case 3:
		top[i] = g.nest(top[i], 1+g.r.Intn(4))
	case 4:
		top = append(top[:i:i], top[i+1:]...)
	case 5:
		top = append(top, g.value(0))
	case 6:
		top[i] = g.value(0)
	case 7:
		g.inArray(top, func(a []any) []any {
			j, k := g.r.Intn(len(a)), g.r.Intn(len(a))
			a[j], a[k] = a[k], a[j]
			return a
		})
	case 8:
		g.inArray(top, func(a []any) []any { return a[:0] })
	case 9:
		g.inArray(top, func(a []any) []any {
			a[g.r.Intn(len(a))] = g.nest(g.value(1), 1+g.r.Intn(2))
			return a
		})
	case 10:
		top[min(i, 2)] = g.value(0) // prefix, version or type
	default:
		g.inArray(top, func(a []any) []any {
			a[g.r.Intn(len(a))] = g.value(1)
			return a
		})
	}
	return top
}

// resize sets an array position's item count to maxN-1, maxN or maxN+1 by
// repeating one of its items (an l accepts repeats; an a refuses them).
func (g *inputGen) resize(top []any, fs []field) {
	var at []int
	for i := 3; i < len(top) && i-3 < len(fs); i++ {
		if isArray(fs[i-3].k) {
			at = append(at, i)
		}
	}
	if len(at) == 0 {
		return
	}
	i := at[g.r.Intn(len(at))]
	a, _ := top[i].([]any)
	item := any(g.str(1 + g.r.Intn(8)))
	if len(a) > 0 {
		item = a[g.r.Intn(len(a))]
	}
	n := fs[i-3].k.maxN - 1 + g.r.Intn(3)
	if fs[i-3].k.maxN < 0 {
		n = g.r.Intn(3000)
	}
	out := make([]any, 0, max(n, 0))
	out = append(out, a[:min(len(a), max(n, 0))]...)
	for len(out) < n {
		out = append(out, item)
	}
	top[i] = out
}

func (g *inputGen) inArray(top []any, f func([]any) []any) {
	for range 4 {
		i := g.r.Intn(len(top))
		if a, ok := top[i].([]any); ok && len(a) > 0 {
			top[i] = f(a)
			return
		}
	}
}

func (g *inputGen) nest(v any, d int) any {
	for range d {
		v = []any{v}
	}
	return v
}

func (g *inputGen) str(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = genChars[g.r.Intn(len(genChars))]
		if g.r.Intn(40) == 0 {
			b[i] = "[],"[g.r.Intn(3)]
		}
	}
	return string(b)
}

func (g *inputGen) value(depth int) any {
	switch g.r.Intn(8) {
	case 0:
		return g.str(g.r.Intn(4))
	case 1, 2:
		return g.str(1 + g.r.Intn(60))
	case 3:
		return json.Number([]string{"0", "1", "7", "01", "9007199254740991", "9007199254740992"}[g.r.Intn(6)])
	case 4:
		return rawTok([]string{"true", "null", "-1", "1e0"}[g.r.Intn(4)])
	case 5:
		return []any{}
	}
	if depth > 2 {
		return g.str(4)
	}
	a := make([]any, g.r.Intn(6))
	for i := range a {
		a[i] = g.value(depth + 1)
	}
	return a
}

// grammar builds a message from tag's field table with a random count near
// 3+n: array positions get arrays near their maxN, scalars get scalars, and
// some positions get the wrong shape.
func (g *inputGen) grammar(tag string) []byte {
	fs := specFields(registry[tag]())
	top := []any{Prefix, json.Number("1"), tag}
	for i := range len(fs) + g.r.Intn(3) - 1 {
		if i >= len(fs) || g.r.Intn(8) == 0 {
			top = append(top, g.value(0))
			continue
		}
		if !isArray(fs[i].k) {
			top = append(top, g.str(1+g.r.Intn(40)))
			continue
		}
		n := g.r.Intn(4)
		if fs[i].k.maxN >= 0 && g.r.Intn(2) == 0 {
			n = fs[i].k.maxN + g.r.Intn(3) - 1
		}
		a := make([]any, max(n, 0))
		for j := range a {
			a[j] = g.str(1 + g.r.Intn(8))
		}
		top = append(top, a)
	}
	var buf bytes.Buffer
	writeValue(&buf, top)
	return buf.Bytes()
}

// edit applies k byte-level edits: replace, insert, delete, a structural
// run, truncation, or a trailing run.
func (g *inputGen) edit(in []byte, k int) []byte {
	b := bytes.Clone(in)
	runs := []string{`[`, `]`, `[[`, `]]`, `,`, `""`, `,""`, `[]`, `"`, `,[]`, `[[[`}
	alpha := genChars + `[]",`
	for range k {
		i := 0
		if len(b) > 0 {
			i = g.r.Intn(len(b))
		}
		switch g.r.Intn(6) {
		case 0:
			if len(b) > 0 {
				b[i] = alpha[g.r.Intn(len(alpha))]
			}
		case 1:
			b = append(b[:i:i], append([]byte{alpha[g.r.Intn(len(alpha))]}, b[i:]...)...)
		case 2:
			if len(b) > 0 {
				b = append(b[:i:i], b[i+1:]...)
			}
		case 3:
			b = append(b[:i:i], append([]byte(runs[g.r.Intn(len(runs))]), b[i:]...)...)
		case 4:
			b = b[:i]
		default:
			b = append(b, runs[g.r.Intn(len(runs))]...)
		}
	}
	return b
}
