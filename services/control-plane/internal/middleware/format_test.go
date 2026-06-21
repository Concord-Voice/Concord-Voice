package middleware_test

import (
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/stretchr/testify/assert"
)

func TestFormatRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"zero", 0, "0s"},
		{"seconds", 30 * time.Second, "30s"},
		{"one minute", time.Minute, "1m"},
		{"minutes", 45 * time.Minute, "45m"},
		{"one hour", time.Hour, "1h"},
		{"hours and minutes", 14*time.Hour + 23*time.Minute, "14h 23m"},
		{"24 hours", 24 * time.Hour, "24h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, middleware.FormatRetryAfter(tt.duration))
		})
	}
}
