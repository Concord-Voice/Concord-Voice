package authoritycontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
)

// field is one layout position (index 3 onward); ptr is *string, *uint64,
// *[]byte, *[]string or *[][]byte.
type field struct {
	name string
	k    kind
	ptr  any
}

// specer is any layout the engine can encode and decode: it names its tag
// and its ordered field table.
type specer interface {
	spec() (tag string, fields []field)
}

// Message is any registered layout. Only this package implements it.
type Message interface {
	spec() (string, []field)
	Encode() []byte
}

// crossChecker is implemented by layouts with a rule over their own positions
// that the DoR names malformed (Global Constraints list). It should return
// malformed(...); the engine wraps anything else (asMalformed).
type crossChecker interface{ crossCheck() error }

// asMalformed makes EN5's "any failure is malformed" a property of the engine
// rather than of every layout author: A2 maps errors.Is(err, ErrMalformed) to
// Reason malformed, so one bare `return err` from a hook (strChk, binChk,
// crossCheck) or a library call would otherwise surface as another Reason.
// An error that already satisfies ErrMalformed passes through with its own
// message. Anything else is replaced, not wrapped with its text, because a
// library error may echo the value it rejected (strconv's does) and a codec
// error names the rule only. err must be non-nil.
func asMalformed(err error) error {
	if errors.Is(err, ErrMalformed) {
		return err
	}
	return malformed("layout rule")
}

// standaloneCaps is §2.3's explicit table; every other layout's cap is maxLen.
var standaloneCaps = map[string]int{
	"anchors": CapAnchors, "signed": CapSigned, "device-set": CapDeviceSet, "device-wrap": CapDeviceWrap,
	"device-intent": CapDeviceIntent, "device-bundle": CapDeviceBundle, "device-history": CapDeviceHistory,
	"recovery-bind": CapRecoveryBind, "relay-recover": CapRelayRecover, "device-result": CapDeviceResult,
	"device-secrets": CapDeviceSecrets, "trust": CapTrust, "chain-head": HeadBlobMax, "device-head": HeadBlobMax,
}

const encodedPrefix = `["` + Prefix + `",1,`

// maxLen is "the sum of its position caps plus its prefix and separators"; -1 if unbounded.
func maxLen(tag string, fs []field) int {
	n := len(encodedPrefix) + len(tag) + 2 + 1
	for _, f := range fs {
		l := f.k.maxEncodedLen()
		if l < 0 {
			return -1
		}
		n += 1 + l
	}
	return n
}

func capOf(m specer) int {
	tag, fs := m.spec()
	if c, ok := standaloneCaps[tag]; ok {
		return c
	}
	return maxLen(tag, fs)
}

// marshal is EN6: it validates every position and writes the bytes, so it
// never writes a message unmarshal would refuse.
func marshal(m specer) ([]byte, error) {
	// Every layout's spec has a pointer receiver that takes field addresses, so
	// a nil interface or a typed nil such as (*Intent)(nil) would panic there.
	if m == nil {
		return nil, malformed("nil message")
	}
	if v := reflect.ValueOf(m); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, malformed("nil message")
	}
	tag, fs := m.spec()
	var buf bytes.Buffer
	buf.WriteString(encodedPrefix + `"` + tag + `"`)
	for _, f := range fs {
		buf.WriteByte(',')
		if err := writeField(&buf, f); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, asMalformed(err))
		}
	}
	buf.WriteByte(']')
	if c, ok := m.(crossChecker); ok {
		if err := c.crossCheck(); err != nil {
			return nil, asMalformed(err)
		}
	}
	if limit := capOf(m); limit >= 0 && buf.Len() > limit {
		return nil, malformed("length")
	}
	return buf.Bytes(), nil
}

// encode is marshal for callers that only need to know whether the message is
// valid: it returns nil when it is not.
func encode(m specer) []byte {
	b, err := marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// Marshal is Encode with the reason a message is refused.
func Marshal(m Message) ([]byte, error) { return marshal(m) }

func isStringKind(t kindTag) bool {
	return t == tagStr || t == tagUUID || t == tagUUIDv4 || t == tagEnum
}

func isArray(k kind) bool { return k.tag == tagArr || k.tag == tagList }

func writeField(buf *bytes.Buffer, f field) error {
	switch p := f.ptr.(type) {
	case *string:
		if !isStringKind(f.k.tag) {
			return malformed(reasonKindType)
		}
		if err := f.k.checkString(*p); err != nil {
			return err
		}
		buf.WriteString(`"` + *p + `"`)
	case *uint64:
		if f.k.tag != tagUint || *p < f.k.minU || *p > f.k.maxU {
			return malformed("u range")
		}
		buf.WriteString(strconv.FormatUint(*p, 10))
	case *[]byte:
		if f.k.tag != tagBytes {
			return malformed(reasonKindType)
		}
		if err := f.k.checkBytes(*p); err != nil {
			return err
		}
		buf.WriteString(`"` + base64.StdEncoding.EncodeToString(*p) + `"`)
	case *[]string:
		return writeStrings(buf, f.k, *p)
	case *[][]byte:
		return writeBins(buf, f.k, *p)
	default:
		return malformed("field type")
	}
	return nil
}

func writeStrings(buf *bytes.Buffer, k kind, in []string) error {
	if !isArray(k) || !isStringKind(k.elem.tag) {
		return malformed(reasonKindType)
	}
	for _, s := range in {
		if s == "" {
			return malformed("empty item")
		}
		if err := k.elem.checkString(s); err != nil {
			return err
		}
	}
	return writeItems(buf, k, slices.Clone(in))
}

func writeBins(buf *bytes.Buffer, k kind, in [][]byte) error {
	if !isArray(k) || k.elem.tag != tagBytes {
		return malformed(reasonKindType)
	}
	items := make([]string, len(in))
	for i, b := range in {
		if len(b) == 0 {
			return malformed("empty item")
		}
		if err := k.elem.checkBytes(b); err != nil {
			return err
		}
		items[i] = base64.StdEncoding.EncodeToString(b)
	}
	return writeItems(buf, k, items)
}

// writeItems enforces the count bounds and, for a, sorts bytewise over the
// encoded strings and refuses duplicates (EN3).
func writeItems(buf *bytes.Buffer, k kind, items []string) error {
	if len(items) < k.minN || (k.maxN >= 0 && len(items) > k.maxN) {
		return malformed(reasonItemCount)
	}
	if k.tag == tagArr {
		slices.Sort(items)
		for i := 1; i < len(items); i++ {
			if items[i] == items[i-1] {
				return malformed("duplicate item")
			}
		}
	}
	buf.WriteByte('[')
	for i, s := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`"` + s + `"`)
	}
	buf.WriteByte(']')
	return nil
}

// unmarshal is EN5. limit ≤ 0 means the standalone cap.
func unmarshal(m specer, b []byte, limit int) error {
	tag, fs := m.spec()
	if limit <= 0 {
		limit = capOf(m)
	}
	if limit >= 0 && len(b) > limit { // step 1
		return malformed("length")
	}
	if err := en2(b); err != nil { // step 2
		return err
	}
	if err := prescan(b, fs); err != nil { // before step 3 allocates anything
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b)) // step 3
	dec.UseNumber()
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return malformed("json")
	}
	if err := checkHead(arr, tag, len(fs)); err != nil { // step 4
		return err
	}
	for i, f := range fs { // step 5
		if err := readField(f, arr[3+i]); err != nil {
			return fmt.Errorf("%s: %w", f.name, asMalformed(err))
		}
	}
	re, err := marshal(m) // step 6 (marshal also runs crossCheck)
	if err != nil {
		return err
	}
	if !bytes.Equal(re, b) {
		return malformed("re-encode")
	}
	return nil
}

// prescan refuses, before step 3 builds a value tree, the shapes that make the
// tree large: nesting below depth 2 (EN4), a top-level count other than 3+n,
// an array at a position whose kind is not an array, and an array with more
// items than its kind's maxN. Without it a refused message under its byte cap
// was fully materialised first, at up to 43 times its size.
//
// It refuses only bytes the rest of EN5 refuses. Every accepted message is
// marshal's output, which opens with '[', nests at most two deep, holds 3+n
// values, puts an array only at an array kind with at most maxN items, ends
// at its last byte, and carries no '"' inside a string. EN2 has already
// refused '\', so a '"' always toggles string state. It is one pass with no
// backtracking and allocates only the error it returns.
//
// Positions with maxN = -1 (the persisted heads) have no item cap, so the
// scan bounds their depth but not their count.
func prescan(b []byte, fs []field) error {
	s := shape{fs: fs}
	for i, c := range b {
		if s.inStr {
			s.inStr = c != '"'
			continue
		}
		if err := s.token(c); err != nil {
			return err
		}
		if s.depth == 0 {
			return s.closed(i == len(b)-1)
		}
	}
	return malformed("json")
}

// shape is prescan's state. top counts the top-level values begun so far, so
// the value being read is position top-1; items and maxN describe the open
// depth-2 array; want is set after '[' and ',', where a value may begin.
type shape struct {
	fs                      []field
	depth, top, items, maxN int
	inStr, want             bool
}

func (s *shape) token(c byte) error {
	if s.depth == 0 && c != '[' {
		return malformed("json")
	}
	if s.want && c != ']' {
		s.want = false
		if err := s.begin(); err != nil {
			return err
		}
	}
	switch c {
	case '"':
		s.inStr = true
	case '[':
		return s.open()
	case ']':
		s.depth--
		s.want = false
	case ',':
		s.want = true
	}
	return nil
}

// begin counts a value at depth 1 or 2; open refuses depth 3 before any value
// can begin there.
func (s *shape) begin() error {
	if s.depth == 1 {
		s.top++
		return nil
	}
	s.items++
	if s.maxN >= 0 && s.items > s.maxN {
		return fmt.Errorf("%s: %w", s.fs[s.top-4].name, malformed(reasonItemCount))
	}
	return nil
}

func (s *shape) open() error {
	s.depth++
	s.want = true
	if s.depth == 1 {
		return nil
	}
	if s.depth > 2 {
		return malformed("depth")
	}
	p := s.top - 4 // the layout field of position top-1
	if p < 0 || p >= len(s.fs) {
		return malformed("array position")
	}
	if !isArray(s.fs[p].k) {
		return fmt.Errorf("%s: %w", s.fs[p].name, malformed("array position"))
	}
	s.items, s.maxN = 0, s.fs[p].k.maxN
	return nil
}

func (s *shape) closed(last bool) error {
	if !last {
		return malformed("trailing bytes")
	}
	if s.top != 3+len(s.fs) {
		return malformed("count")
	}
	return nil
}

func checkHead(arr []any, tag string, n int) error {
	if len(arr) != 3+n {
		return malformed("count")
	}
	if s, ok := arr[0].(string); !ok || s != Prefix {
		return malformed("prefix")
	}
	if v, ok := arr[1].(json.Number); !ok || v.String() != "1" {
		return malformed("version")
	}
	if s, ok := arr[2].(string); !ok || s != tag {
		return malformed("type")
	}
	return nil
}

func readField(f field, v any) error {
	switch p := f.ptr.(type) {
	case *string:
		s, ok := v.(string)
		if !ok || !isStringKind(f.k.tag) {
			return malformed("not a string")
		}
		if err := f.k.checkString(s); err != nil {
			return err
		}
		*p = s
	case *uint64:
		return readUint(f.k, v, p)
	case *[]byte:
		return readBytes(f.k, v, p)
	case *[]string:
		return readStrings(f.k, v, p)
	case *[][]byte:
		return readBins(f.k, v, p)
	default:
		return malformed("field type")
	}
	return nil
}

func readUint(k kind, v any, p *uint64) error {
	n, ok := v.(json.Number)
	if !ok || k.tag != tagUint {
		return malformed("not a number")
	}
	x, err := parseU(n.String())
	if err != nil {
		return err
	}
	if x < k.minU || x > k.maxU {
		return malformed("u range")
	}
	*p = x
	return nil
}

func readBytes(k kind, v any, p *[]byte) error {
	s, ok := v.(string)
	if !ok || k.tag != tagBytes {
		return malformed("not a string")
	}
	if s == "" {
		if !k.empty {
			return malformed("empty")
		}
		*p = nil
		return nil
	}
	b, err := decodeB64(s)
	if err != nil {
		return err
	}
	if err := k.checkBytes(b); err != nil {
		return err
	}
	*p = b
	return nil
}

// readItems applies EN4 (strings only, depth 2) and the a ordering.
func readItems(k kind, v any) ([]string, error) {
	raw, ok := v.([]any)
	if !ok || !isArray(k) {
		return nil, malformed("not an array")
	}
	if len(raw) < k.minN || (k.maxN >= 0 && len(raw) > k.maxN) {
		return nil, malformed(reasonItemCount)
	}
	items := make([]string, len(raw))
	for i, e := range raw {
		s, ok := e.(string)
		if !ok || s == "" {
			return nil, malformed("array item")
		}
		if k.tag == tagArr && i > 0 && s <= items[i-1] {
			return nil, malformed("not strictly ascending")
		}
		items[i] = s
	}
	return items, nil
}

func readStrings(k kind, v any, p *[]string) error {
	items, err := readItems(k, v)
	if err != nil {
		return err
	}
	if !isStringKind(k.elem.tag) {
		return malformed(reasonKindType)
	}
	for _, s := range items {
		if err := k.elem.checkString(s); err != nil {
			return err
		}
	}
	if len(items) == 0 {
		items = nil
	}
	*p = items
	return nil
}

func readBins(k kind, v any, p *[][]byte) error {
	items, err := readItems(k, v)
	if err != nil {
		return err
	}
	if k.elem.tag != tagBytes {
		return malformed(reasonKindType)
	}
	var out [][]byte
	for _, s := range items {
		b, err := decodeB64(s)
		if err != nil {
			return err
		}
		if err := k.elem.checkBytes(b); err != nil {
			return err
		}
		out = append(out, b)
	}
	*p = out
	return nil
}

// decodeAs is the generic DecodeT. A failed decode returns the zero value.
func decodeAs[T any, PT interface {
	*T
	specer
}](b []byte, limit int) (T, error) {
	var m T
	if err := unmarshal(PT(&m), b, limit); err != nil {
		var zero T
		return zero, err
	}
	return m, nil
}

var registry = map[string]func() Message{}

// register is called from each layout file's init. A duplicate tag is a
// programming error caught by every test run.
func register(ctors ...func() Message) {
	for _, c := range ctors {
		tag, _ := c().spec()
		if _, dup := registry[tag]; dup {
			panic("authoritycontract: duplicate layout " + tag)
		}
		registry[tag] = c
	}
}

// Decode decodes b as the registered layout tag, under its standalone cap. It
// checks only the positions of that one message. A rule that pairs two
// messages is not applied: Decode("device-head", b) accepts any well-formed
// chain_head_digest, and only ParseDeviceHead(deviceHead, chainHead) refuses
// one that does not match its chain-head.
func Decode(tag string, b []byte) (Message, error) {
	c, ok := registry[tag]
	if !ok {
		return nil, malformed("unknown layout")
	}
	m := c()
	if err := unmarshal(m, b, 0); err != nil {
		return nil, err
	}
	return m, nil
}

// Tags lists every registered layout tag, sorted.
func Tags() []string {
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}
