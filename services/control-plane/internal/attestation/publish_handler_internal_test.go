package attestation

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// ── truncateForLog ──────────────────────────────────────────────────

// TestTruncateForLog covers the log-volume bound directly rather than only
// through runOIDCVerify's failure path, which no test asserts against.
//
// The helper is security-motivated on both axes it is tested for. VOLUME:
// go-jose interpolates the raw `alg` header into its error and go-oidc embeds
// the whole JWKS body in a key-decode failure, so an unauthenticated 401 could
// otherwise write up to net/http's MaxHeaderBytes into the log sink — measured
// at 200,087 bytes by @red-team on PR #3207. ENCODING: the truncated tail is
// attacker-controlled, so cutting at a fixed byte offset can split a multi-byte
// rune and feed invalid UTF-8 to a JSON log encoder. The split-rune case below
// is the one that fails if the ToValidUTF8 call is ever removed as redundant.
// Raised by @gitar-bot on PR #3207.
func TestTruncateForLog(t *testing.T) {
	// A 3-byte rune positioned so the cut at maxLoggedErrLen lands INSIDE it:
	// two of its bytes fall below the boundary, the third above.
	splitRune := strings.Repeat("a", maxLoggedErrLen-1) + "\u2603" + strings.Repeat("b", 16)

	cases := []struct {
		name   string
		in     string
		assert func(t *testing.T, got string)
	}{
		{
			name: "short string passes through untouched",
			in:   "oidc verify: oidc: malformed jwt",
			assert: func(t *testing.T, got string) {
				require.Equal(t, "oidc verify: oidc: malformed jwt", got)
			},
		},
		{
			name: "exactly at the boundary is not truncated",
			in:   strings.Repeat("a", maxLoggedErrLen),
			assert: func(t *testing.T, got string) {
				require.Len(t, got, maxLoggedErrLen)
				require.NotContains(t, got, "truncated")
			},
		},
		{
			name: "one byte over the boundary is truncated",
			in:   strings.Repeat("a", maxLoggedErrLen+1),
			assert: func(t *testing.T, got string) {
				require.Contains(t, got, "[truncated]")
				require.Less(t, len(got), maxLoggedErrLen+len("…[truncated]")+1)
			},
		},
		{
			name: "a 200KB alg header is bounded, not logged whole",
			in:   strings.Repeat("A", 200000),
			assert: func(t *testing.T, got string) {
				require.Less(t, len(got), 600, "an unauthenticated 401 must not write 200KB to the sink")
			},
		},
		{
			name: "a cut through a multi-byte rune yields valid UTF-8",
			in:   splitRune,
			assert: func(t *testing.T, got string) {
				require.True(t, utf8.ValidString(got),
					"ToValidUTF8 must drop the partial rune — invalid UTF-8 corrupts a JSON log encoder")
				require.NotContains(t, got, "\uFFFD", "the partial rune is dropped, not replaced")
				require.Contains(t, got, "[truncated]")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, truncateForLog(tc.in))
		})
	}
}
