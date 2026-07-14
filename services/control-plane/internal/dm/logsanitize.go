package dm

import "strings"

// sanitizeLogValue strips control characters from a request-derived string
// before it reaches a structured log sink. The explicit CR/LF replacements
// are recognized by CodeQL's go/log-injection model; the rune filter removes
// the remaining C0 controls and DEL for defense in depth. UUID-shaped values
// are sanitized uniformly because the analyzer does not rely on type-based
// validation across request parsing.
func sanitizeLogValue(value string) string {
	value = strings.ReplaceAll(value, "\n", "")
	value = strings.ReplaceAll(value, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}
