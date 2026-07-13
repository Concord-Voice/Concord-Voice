package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeLogValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "message-123", want: "message-123"},
		{name: "crlf injection", in: "id\r\nforged log entry", want: "idforged log entry"},
		{name: "lone newline", in: "a\nb", want: "ab"},
		{name: "lone carriage return", in: "a\rb", want: "ab"},
		{name: "C0 controls", in: "a\t\x00\x01\x1fb", want: "ab"},
		{name: "DEL", in: "a\x7fb", want: "ab"},
		{name: "Unicode preserved", in: "café-🎉-Ω", want: "café-🎉-Ω"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeLogValue(tt.in))
		})
	}
}
