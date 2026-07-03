package subscriptions

import "strings"

// sanitizeID strips control characters from an id before it is logged (CWE-117
// log-forging defense). Applied uniformly to logged user-derived strings — even
// a structurally hex-only uuid — per observability.md / #1645 (the type-based
// exemption was retired). Local to the package to avoid importing the websocket
// helper (which lives on a different dependency layer).
func sanitizeID(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
