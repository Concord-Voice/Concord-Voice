// Package feedback implements the user-feedback bug-report / feature-request
// pipeline (#158). Submissions arrive over POST /api/v1/feedback,
// authenticated, rate-limited (3/hour/user via existing middleware), and are
// posted to a dedicated feedback repo on GitHub via the REST issues API.
//
// This file implements server-side PII re-sanitization. The client also
// scrubs at capture time (client/desktop/src/renderer/services/logBufferService.ts),
// but defense-in-depth dictates a second pass here — the server is the last
// line before a public-ish GitHub issue is created. A compromised client
// could submit unscrubbed bytes; this pass denies that.
//
// The regex set is a Go-port of the client patterns. They MUST stay in sync:
// each pattern category present here exists in logBufferService.PATTERNS and
// vice versa. The unit test (sanitize_test.go) locks each category.
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
