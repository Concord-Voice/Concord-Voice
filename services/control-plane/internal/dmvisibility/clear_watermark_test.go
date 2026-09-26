package dmvisibility

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDecideClearWatermark(t *testing.T) {
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                    string
		participants, uncleared int
		wm                      sql.NullTime
		want                    ClearWatermark
		eligible, wantErr       bool
	}{
		{"zero participants is unbounded", 0, 0, sql.NullTime{}, ClearWatermark{Unbounded: true}, true, false},
		{"one uncleared participant blocks", 3, 1, sql.NullTime{Time: at, Valid: true}, ClearWatermark{}, false, false},
		{"all cleared uses the minimum", 2, 0, sql.NullTime{Time: at, Valid: true}, ClearWatermark{At: at}, true, false},
		{"all cleared but NULL watermark is an invariant break", 2, 0, sql.NullTime{}, ClearWatermark{}, false, true},
		{"negative counts are rejected", -1, 0, sql.NullTime{}, ClearWatermark{}, false, true},
		{"uncleared above participants is rejected", 1, 2, sql.NullTime{}, ClearWatermark{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, eligible, err := DecideClearWatermark(tc.participants, tc.uncleared, tc.wm)
			if tc.wantErr {
				require.Error(t, err)
				require.False(t, eligible)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.eligible, eligible)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestClearWatermarkLateralIsAConstantWithOnlyClearRanges(t *testing.T) {
	require.Contains(t, ClearWatermarkLateral, "hr.includes_own")
	require.Contains(t, ClearWatermarkLateral, "hr.hidden_from = '-infinity'::timestamptz")
	require.Contains(t, ClearWatermarkLateral, "p.conversation_id = c.id")
	require.NotContains(t, ClearWatermarkLateral, "%", "no fmt placeholders: the fragment is a constant")
}
