package websocket

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckOrigin(t *testing.T) {
	// NewHandler with nil deps is safe — only the allowedOrigins and
	// upgrader fields are used by CheckOrigin.
	h := NewHandler(nil, nil, nil, "", []string{"https://concordvoice.chat"})

	tests := []struct {
		name   string
		origin string // "" means omit the header entirely
		set    bool   // whether to explicitly set the Origin header
		want   bool
	}{
		{"empty origin (native client)", "", false, true},
		{"null origin (sandboxed iframe)", "null", true, false},
		{"file:// origin (Electron desktop)", "file://", true, true},
		{"file:///path origin (Electron desktop)", "file:///app/index.html", true, true},
		{"allowed origin", "https://concordvoice.chat", true, true},
		{"disallowed origin", "https://evil.com", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			if tt.set {
				r.Header.Set("Origin", tt.origin)
			}
			got := h.upgrader.CheckOrigin(r)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckOriginWildcard(t *testing.T) {
	h := NewHandler(nil, nil, nil, "", []string{"*"})

	tests := []struct {
		name   string
		origin string
		set    bool
		want   bool
	}{
		{"wildcard + empty origin", "", false, true},
		{"wildcard + null origin", "null", true, false},
		{"wildcard + file://", "file://", true, true},
		{"wildcard + file:///path", "file:///index.html", true, true},
		{"wildcard + any origin", "https://anything.example.com", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			if tt.set {
				r.Header.Set("Origin", tt.origin)
			}
			got := h.upgrader.CheckOrigin(r)
			assert.Equal(t, tt.want, got)
		})
	}
}
