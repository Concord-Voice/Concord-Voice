package sessions

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMaskIPAddress(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"IPv4 masks last octet", "192.168.1.100", "192.168.1.x"},
		{"IPv4 localhost", "127.0.0.1", "127.0.0.x"},
		{"IPv4 private", "10.0.0.1", "10.0.0.x"},
		{"IPv6 keeps /48 prefix", "2001:db8:1234:5678:abcd:ef00:1234:5678", "2001:db8:1234::x"},
		{"IPv6 loopback", "::1", "unknown"},
		{"invalid IP", "not-an-ip", "unknown"},
		{"empty string", "", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, maskIPAddress(tt.input))
		})
	}
}
