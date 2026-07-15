package purge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRange(t *testing.T) {
	t.Run("all returns nil cutoff", func(t *testing.T) {
		got, err := ParseRange("all")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("unknown token errors", func(t *testing.T) {
		got, err := ParseRange("bogus")
		require.Error(t, err)
		assert.Nil(t, got)
	})

	cases := map[string]time.Duration{
		"1h":  time.Hour,
		"6h":  6 * time.Hour,
		"12h": 12 * time.Hour,
		"1d":  24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"15d": 15 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
		"90d": 90 * 24 * time.Hour,
	}
	for token, want := range cases {
		token, want := token, want
		t.Run(token, func(t *testing.T) {
			before := time.Now().UTC()
			got, err := ParseRange(token)
			require.NoError(t, err)
			require.NotNil(t, got)

			// Cutoff must be a UTC instant approximately (now - want).
			assert.Equal(t, time.UTC, got.Location(), "cutoff must be UTC")
			expected := before.Add(-want)
			// got was computed a hair after `before`, so got-expected is a small
			// positive slack; allow 2s for slow CI.
			assert.LessOrEqual(t, got.Sub(expected).Abs(), 2*time.Second,
				"cutoff for %q should be ~now-%s", token, want)
		})
	}
}
