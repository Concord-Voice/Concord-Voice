package email

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// NormalizeAddress is the application's one address normalizer (#3680 RT-3):
// strings.TrimSpace then strings.ToLower. The Unicode spaces are built with rune(), never
// escaped in source.
func TestNormalizeAddress(t *testing.T) {
	nbsp := string(rune(0x00A0))
	ideographic := string(rune(0x3000))

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already normal", "user@example.test", "user@example.test"},
		{"ASCII spaces", "  user@example.test  ", "user@example.test"},
		{"no-break space U+00A0", nbsp + "user@example.test" + nbsp, "user@example.test"},
		{"ideographic space U+3000", ideographic + "user@example.test" + ideographic, "user@example.test"},
		{"tab", "\tuser@example.test\t", "user@example.test"},
		{"line feed and carriage return", "\nuser@example.test\r\n", "user@example.test"},
		{"mixed padding kinds", " \t" + nbsp + "user@example.test" + ideographic + "\n ", "user@example.test"},
		{"ASCII upper case", "User@EXAMPLE.Test", "user@example.test"},
		{"non-ASCII letter", "ÉLISE@BÜCHER.test", "élise@bücher.test"},
		{"case and padding together", nbsp + "User@Example.TEST" + "\t", "user@example.test"},
		{"interior white space is kept", " us er@exa mple.test ", "us er@exa mple.test"},
		{"interior no-break space is kept", "us" + nbsp + "er@example.test", "us" + nbsp + "er@example.test"},
		{"interior tab is kept", "us\ter@example.test", "us\ter@example.test"},
		{"empty", "", ""},
		{"only white space", " \t" + nbsp + ideographic + "\n", ""},
		{"not an address, not validated", "  NOT AN ADDRESS  ", "not an address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeAddress(tc.in)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, NormalizeAddress(got), "idempotent")
		})
	}
}
