// Package feedback implements the user-feedback bug-report / feature-request
// pipeline (#158). Submissions arrive over POST /api/v1/feedback,
// authenticated, rate-limited (10/hour/user via existing middleware), and are
// posted to a dedicated feedback repo on GitHub via the REST issues API.
//
// This file implements server-side PII re-sanitization. The client also
// scrubs at capture time (client/desktop/src/renderer/services/system/logBufferService.ts),
// but defense-in-depth dictates a second pass here — the server is the last
// line before a public-ish GitHub issue is created. A compromised client
// could submit unscrubbed bytes; this pass denies that.
//
// The regex set is a Go-port of the client patterns. They MUST stay in sync:
// each pattern category present here exists in logBufferService.PATTERNS and
// vice versa. The unit test (sanitize_test.go) locks each category. Where Go's
// RE2 cannot express a client pattern (no lookaround), the port errs toward
// OVER-redaction rather than dropping the category — see the TURN credential
// entry below.
package feedback

import "regexp"

// scrubPattern pairs a compiled regex with the redaction token it produces.
// Order in `scrubPatterns` matters: longest-prefix / most-specific patterns
// first so that (e.g.) a JWT inside a URL gets caught as a JWT rather than
// as part of the surrounding text.
type scrubPattern struct {
	re          *regexp.Regexp
	replacement string
}

var scrubPatterns = []scrubPattern{
	// JWT — three base64url segments joined by dots, prefix `eyJ` for the
	// typical {"alg":...} header start.
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), "<jwt>"},
	// Bearer tokens with the literal "Bearer " prefix.
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{10,}`), "Bearer <token>"},
	// Email addresses.
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "<email>"},
	// Filesystem paths containing usernames — POSIX form.
	{regexp.MustCompile(`/Users/[^/\s"']+`), "/Users/<user>"},
	{regexp.MustCompile(`/home/[^/\s"']+`), "/home/<user>"},
	// Filesystem paths — Windows form (no /i flag; the drive letter is
	// canonicalised uppercase by the redaction token).
	{regexp.MustCompile(`[A-Z]:\\Users\\[^\\/\s"']+`), `C:\Users\<user>`},
	// IPv4 dotted-quad.
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "<ip>"},
	// IPv6 — lenient: at least 2 colon-separated hex groups. Covers compressed
	// forms (::1, fe80::abcd) and full forms.
	{regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{1,4}\b`), "<ip>"},
	// Long hex strings (hashes, raw token bytes) — 32+ hex chars.
	{regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), "<hex>"},
	// Long base64 / url-safe-base64 strings (keys, encrypted blobs) — 40+ chars.
	{regexp.MustCompile(`\b[A-Za-z0-9+/=_-]{40,}\b`), "<base64>"},

	// ─── TURN relay credentials (#3104) ──────────────────────────────────
	//
	// Parity mirror of the TURN entries in logBufferService.PATTERNS. The
	// control plane mints ephemeral coturn credentials per voice join
	// (pkg/config/turn.go): username `<unix expiry>:<userID>`, credential
	// base64 of an HMAC-SHA1 digest. 20 digest bytes encode to exactly 27
	// base64 characters plus one '=' pad — 28 characters, which passes under
	// the 40-char floor of the long-base64 pattern above.
	//
	// Placement is doubly constrained: AFTER the base64 entry, so a longer
	// blob is consumed first and the narrow band below cannot chop a fragment
	// out of one; and BEFORE the raw-UUID backstop, which would otherwise
	// rewrite the userID half of the username and leave the expiry standing.
	//
	// Key-shaped redaction first — shape-independent, so it still holds if the
	// control plane changes the credential algorithm. An optional identifier
	// prefix covers `turnCredential` / `newPassword`. The value quantifier is
	// unbounded here where the client bounds it: RE2 has no backtracking, so
	// the S5852 ReDoS bound the client needs buys nothing in Go, and a bound
	// would silently miss an over-long value.
	//
	// Two entries, split by whether the KEY is quoted. The first is the JSON
	// serialization form; the second is the bare-key form, and its value half
	// is a two-branch alternation because a bare key can legitimately carry
	// EITHER quote style (`credential: 'x'` from util.inspect,
	// `credential: "x"` from a hand-formatted or JS-object-literal line). That
	// second form used to be redacted by NOTHING — the JSON entry requires a
	// quoted key and this one required a single-quoted value, so a bare key
	// with a double-quoted value satisfied neither. Alternating two complete
	// branches, rather than widening one value class to `["']`, is what keeps
	// each branch's class excluding only its OWN quote: a shared class would
	// stop at an embedded foreign quote and leak the rest of the value. RE2 has
	// no backreference to pin the closing quote to the opening one, and the
	// alternation means it needs none — a mismatched pair matches neither
	// branch, which is correct, since no serializer emits one.
	{regexp.MustCompile(`(?i)"([^"\\]{0,32}(?:credential|password))"\s{0,8}:\s{0,8}"[^"\\]*"`), `"${1}":"<redacted>"`},
	{regexp.MustCompile(`(?i)([A-Za-z0-9_$]{0,32}(?:credential|password))\s{0,8}:\s{0,8}(?:'[^'\\]*'|"[^"\\]*")`), `${1}: '<redacted>'`},
	// TURN REST-API username — `<unix expiry>:<userID>`.
	{regexp.MustCompile(`\b\d{9,12}:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<turn-username>"},
	// TURN REST-API credential — the 28-char band the 40-char floor misses.
	// RE2 has no lookaround, so the client's standalone-token pin (a lookbehind
	// plus a lookahead) is NOT portable here. The leading half is a CONSUMED
	// group instead — start-of-text or one non-base64 character, re-emitted by
	// `${1}` — because `\b` cannot express it. `\b` needs a word/non-word
	// transition, and it considers the DELIMITER without considering the
	// token's own first character: when that character is `+` or `/` (both
	// non-word, exactly like every realistic delimiter) there is no transition
	// to find, so `\b[A-Za-z0-9+/]{27}=` matched nothing. Two of the 64 base64
	// characters, so ~3% of minted credentials reached a PUBLIC GitHub issue
	// unredacted. The trailing half is still deliberately dropped, so the
	// accepted residual is unchanged: OVER-redaction of a 28-char prefix inside
	// a 28..39-char run. That is the safe direction for a last-line backstop,
	// and the client keeps the precise form where fidelity matters.
	//
	// One narrowing comes with the consumed group and is stated rather than
	// hidden: two credentials written back-to-back with no separator
	// (`<27chars>=<27chars>=`) redact only the first, because the second's only
	// delimiter is the first's pad and that pad was consumed. No serializer the
	// pipeline sees emits that shape — JSON and util.inspect both separate
	// values — and the key-shaped entries above cover the keyed forms anyway.
	{regexp.MustCompile(`(^|[^A-Za-z0-9+/])[A-Za-z0-9+/]{27}=`), "${1}<base64>"},
	// Raw UUID strip backstop (defense-in-depth for a compromised client that
	// bypassed the renderer-side ordinal pseudonymizer). Fixed-width → RE2-safe.
	// The honest client already replaced UUIDs with <id:N> ordinals, so this
	// only fires on raw UUIDs that slipped through. NOT parity-matched to a
	// client PATTERN — the client's pseudonym is a fidelity feature, this is a
	// fail-safe.
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<uuid>"},
}

// Sanitize runs every scrubPattern across the input and returns the redacted
// copy. The input is never mutated. Empty input returns empty. Patterns are
// applied in declared order; iteration is O(n_patterns * len(input)).
func Sanitize(input string) string {
	if input == "" {
		return ""
	}
	out := input
	for _, p := range scrubPatterns {
		out = p.re.ReplaceAllString(out, p.replacement)
	}
	return out
}
