package purge

import (
	"fmt"
	"time"
)

// rangeIntervals maps the client range tokens to their lookback duration. Kept in
// one place so ParseRange and its test agree on the exact supported token set.
var rangeIntervals = map[string]time.Duration{
	"1h":  1 * time.Hour,
	"6h":  6 * time.Hour,
	"12h": 12 * time.Hour,
	"1d":  24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"15d": 15 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

// ParseRange maps a client range token to a UTC cutoff instant.
//
//   - "all"          -> nil (no lower bound / All Time)
//   - a known token  -> &(time.Now().UTC() - interval)
//   - anything else  -> error
//
// The cutoff is an explicit UTC instant so the per-table ::timestamp / ::timestamptz
// casts in the batched delete (engine.go) compare against a stable, session-TZ
// independent value (M2). Exported and homed in the purge package because both the
// messages and dm handlers call purge.ParseRange.
func ParseRange(rangeStr string) (*time.Time, error) {
	if rangeStr == "all" {
		return nil, nil
	}
	interval, ok := rangeIntervals[rangeStr]
	if !ok {
		return nil, fmt.Errorf("purge: unknown range %q", rangeStr)
	}
	cutoff := time.Now().UTC().Add(-interval)
	return &cutoff, nil
}
