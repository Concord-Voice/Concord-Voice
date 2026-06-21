package middleware

import (
	"fmt"
	"time"
)

// FormatRetryAfter converts a duration to a human-readable string like "14h 23m".
func FormatRetryAfter(d time.Duration) string {
	totalSec := int(d.Seconds())
	if totalSec < 60 {
		return fmt.Sprintf("%ds", totalSec)
	}
	if totalSec < 3600 {
		return fmt.Sprintf("%dm", totalSec/60)
	}
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
