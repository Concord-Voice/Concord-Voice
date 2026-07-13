package messages

import "strings"

// sanitizeLogValue strips control characters from a user-derived string before
// it reaches a log sink, preventing CWE-117 line forging. Keep the leading
// ReplaceAll calls in the form recognized by CodeQL's go/log-injection model.
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
