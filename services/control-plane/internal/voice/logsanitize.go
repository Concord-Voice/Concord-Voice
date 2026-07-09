package voice

import "strings"

// sanitizeLogValue strips control characters from a user-derived string before
// it is written to a log sink, preventing CWE-117 log forging (CRLF injection
// of fabricated log lines). `\n` and `\r` are removed via strings.ReplaceAll —
// the form the CodeQL go/log-injection sanitizer model recognizes — and any
// remaining C0 control characters plus DEL are dropped for defense-in-depth.
//
// Apply it to request-derived string values (URL params, context identifiers
// sourced from the request) logged via the structured logger. Per
// observability.md ("Logging Discipline") and PR #1645, identifiers are NOT
// exempt on the basis that they are "structurally" hex-only: CodeQL's
// go/log-injection does not honor type-based reasoning, so sanitize uniformly
// rather than deciding which values are "safe to log raw." Mirrors the local
// helpers in the websocket and subscriptions packages; kept package-local to
// avoid importing another package's unexported helper.
func sanitizeLogValue(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
