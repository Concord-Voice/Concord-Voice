package websocket

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
)

func TestWebSocketAuthFailure(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"unavailable", fmt.Errorf("wrap: %w", loginfamily.ErrUnavailable), http.StatusServiceUnavailable, "Authentication temporarily unavailable"},
		{"contract", fmt.Errorf("wrap: %w", loginfamily.ErrContract), http.StatusInternalServerError, "Unauthorized"},
		{"other", errors.New("bad ticket"), http.StatusUnauthorized, "Unauthorized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, message := webSocketAuthFailure(tc.err)
			if status != tc.status || message != tc.message {
				t.Fatalf("webSocketAuthFailure() = %d %q, want %d %q", status, message, tc.status, tc.message)
			}
		})
	}
}
