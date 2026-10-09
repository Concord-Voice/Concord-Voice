package authoritycontract

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/google/uuid"
)

// ErrMalformed is EN5's single failure: every codec error wraps it.
var ErrMalformed = errors.New("authoritycontract: malformed")

// malformed names the failed rule only, never a value, so an error string is
// safe to log.
func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, a...))
}

// Rule names the engine reports from more than one place.
const (
	reasonKindType  = "kind/type"
	reasonItemCount = "item count"
)

type kindTag uint8

const (
	tagStr    kindTag = iota + 1 // s: [A-Za-z0-9._:/+=-]{1,maxLen}
	tagUUID                      // s36
	tagUUIDv4                    // s36v4
	tagUint                      // u, u>0 / u≥1, u01
	tagBytes                     // bN, b≤N, b∈{N1,N2}
	tagEnum                      // e{…}
	tagArr                       // a<t>[m..n]: strictly ascending bytewise
	tagList                      // l<t>[..n]: layout-defined order
)

// kind is one EN3 position type.
type kind struct {
	tag    kindTag
	maxLen int                // tagStr
	minU   uint64             // tagUint
	maxU   uint64             // tagUint
	sizes  []int              // tagBytes: exact sizes; empty means 1..upTo
	upTo   int                // tagBytes: b≤N
	empty  bool               // the position also admits ""
	enum   []string           // tagEnum (may contain "")
	elem   *kind              // tagArr, tagList
	minN   int                // tagArr, tagList
	maxN   int                // tagArr, tagList; -1 = no item cap (persisted heads only)
	strChk func(string) error // extra rule on a tagStr value
	binChk func([]byte) error // extra rule on decoded bytes
}

func kStr(n int) kind { return kind{tag: tagStr, maxLen: n} }

var (
	kS36   = kind{tag: tagUUID}
	kS36v4 = kind{tag: tagUUIDv4}
	kU     = kind{tag: tagUint, maxU: MaxSafeInt}
	kU1    = kind{tag: tagUint, minU: 1, maxU: MaxSafeInt} // u>0 and u≥1
	kU01   = kind{tag: tagUint, maxU: 1}
)

func kB(n int) kind              { return kind{tag: tagBytes, sizes: []int{n}} }
func kBOneOf(n ...int) kind      { return kind{tag: tagBytes, sizes: n} }
func kBUpTo(n int) kind          { return kind{tag: tagBytes, upTo: n} }
func kE(vals ...string) kind     { return kind{tag: tagEnum, enum: vals} }
func kA(e kind, mn, mx int) kind { return kind{tag: tagArr, elem: &e, minN: mn, maxN: mx} }
func kL(e kind, mn, mx int) kind { return kind{tag: tagList, elem: &e, minN: mn, maxN: mx} }

func (k kind) OrEmpty() kind                     { k.empty = true; return k }
func (k kind) withStr(f func(string) error) kind { k.strChk = f; return k }
func (k kind) withBin(f func([]byte) error) kind { k.binChk = f; return k }

func isSChar(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
		c == '.' || c == '_' || c == ':' || c == '/' || c == '+' || c == '=' || c == '-'
}

// en2 is EN2's byte pre-filter. It rejects a BOM, whitespace, escapes,
// non-ASCII, braces, CR and LF before any parsing.
func en2(b []byte) error {
	for _, c := range b {
		if !isSChar(c) && c != '"' && c != ',' && c != '[' && c != ']' {
			return malformed("EN2 byte")
		}
	}
	return nil
}

// canonicalUUID is s36: a lowercase canonical UUID (precedent devicerecovery/protocol.go:87).
func canonicalUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil && id.String() == s
}

// isUUIDv4 is s36v4.
func isUUIDv4(s string) bool {
	id, ok := canonicalUUID(s)
	return ok && id.Version() == 4 && id.Variant() == uuid.RFC4122
}

// decodeB64Strict is RFC 4648 §3.5 strict decoding, EN5 step 5's first half.
func decodeB64Strict(s string) ([]byte, error) {
	return base64.StdEncoding.Strict().DecodeString(s)
}

// decodeB64 adds EN5's value-level comparison: the re-encoding must equal s.
// That also refuses a CR or LF, which Strict() still ignores.
func decodeB64(s string) ([]byte, error) {
	b, err := decodeB64Strict(s)
	if err != nil || base64.StdEncoding.EncodeToString(b) != s {
		return nil, malformed("base64")
	}
	return b, nil
}

// parseU is EN3's u: "0" or [1-9][0-9]*, at most MaxSafeInt.
func parseU(s string) (uint64, error) {
	if s == "" || len(s) > 16 || (s[0] == '0' && len(s) > 1) {
		return 0, malformed("u form")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, malformed("u form")
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v > MaxSafeInt {
		return 0, malformed("u range")
	}
	return v, nil
}

// checkString validates a string-valued position (s, s36, s36v4, e).
func (k kind) checkString(s string) error {
	if s == "" {
		if k.empty || (k.tag == tagEnum && slices.Contains(k.enum, "")) {
			return nil
		}
		return malformed("empty")
	}
	switch k.tag {
	case tagStr:
		return k.checkS(s)
	case tagUUID:
		if _, ok := canonicalUUID(s); !ok {
			return malformed("s36")
		}
		return nil
	case tagUUIDv4:
		if !isUUIDv4(s) {
			return malformed("s36v4")
		}
		return nil
	case tagEnum:
		if !slices.Contains(k.enum, s) {
			return malformed("enum")
		}
		return nil
	}
	return malformed("not a string kind")
}

func (k kind) checkS(s string) error {
	if len(s) > k.maxLen {
		return malformed("s length")
	}
	for i := 0; i < len(s); i++ {
		if !isSChar(s[i]) {
			return malformed("s char")
		}
	}
	if k.strChk != nil {
		return k.strChk(s)
	}
	return nil
}

// checkBytes validates a decoded b position; nil or empty means "".
func (k kind) checkBytes(b []byte) error {
	if len(b) == 0 {
		if k.empty {
			return nil
		}
		return malformed("empty")
	}
	if len(k.sizes) > 0 {
		if !slices.Contains(k.sizes, len(b)) {
			return malformed("b size")
		}
	} else if len(b) > k.upTo {
		return malformed("b length")
	}
	if k.binChk != nil {
		return k.binChk(b)
	}
	return nil
}

func b64Len(n int) int { return (n + 2) / 3 * 4 }

// maxEncodedLen is the largest encoding of one value of k; -1 if unbounded.
func (k kind) maxEncodedLen() int {
	switch k.tag {
	case tagStr:
		return k.maxLen + 2
	case tagUUID, tagUUIDv4:
		return 38
	case tagUint:
		return len(strconv.FormatUint(k.maxU, 10))
	case tagBytes:
		n := k.upTo
		for _, s := range k.sizes {
			n = max(n, s)
		}
		return b64Len(n) + 2
	case tagEnum:
		n := 0
		for _, v := range k.enum {
			n = max(n, len(v))
		}
		return n + 2
	case tagArr, tagList:
		switch {
		case k.maxN < 0:
			return -1
		case k.maxN == 0:
			return 2
		}
		return 2 + k.maxN*k.elem.maxEncodedLen() + k.maxN - 1
	}
	return -1
}
