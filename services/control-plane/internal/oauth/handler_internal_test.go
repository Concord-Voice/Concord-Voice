package oauth

import "testing"

// TestMaskEmail_Cases exercises the masking branches in isolation. The
// integration test covers the typical "carol@example.test" path; this fills
// in the < 2-char local-part and malformed-input edges so the redaction
// helper never returns the user's plaintext local part.
func TestMaskEmail_Cases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"typical address", "carol@example.test", "c***@example.test"},
		{"two-char local part still gets first letter", "ab@example.com", "a***@example.com"},
		{"single-char local part is fully masked", "a@example.com", "***@example.com"},
		{"empty local part is fully masked", "@example.com", "***@example.com"},
		{"no at-sign returns wildcard", "no-at-sign", "***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskEmail(tc.in)
			if got != tc.want {
				t.Fatalf("maskEmail(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
