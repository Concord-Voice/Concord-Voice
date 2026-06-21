package websocket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeLogValue(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "user-123", "user-123"},
		{"empty", "", ""},
		{"crlf_injection", "id\r\nFAKE LOG LINE forged by attacker", "idFAKE LOG LINE forged by attacker"},
		{"lone_newline", "a\nb", "ab"},
		{"lone_cr", "a\rb", "ab"},
		{"tab_and_c0_controls", "a\t\x00\x01\x1fb", "ab"},
		{"del", "a\x7fb", "ab"},
		{"unicode_preserved", "café-🎉-Ω", "café-🎉-Ω"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeLogValue(tc.in))
		})
	}
}
