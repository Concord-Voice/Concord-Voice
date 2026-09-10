package voice_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
)

// Nothing pinned parseVoiceEventTime before #3205 -- zero test hits for its
// error string -- and the clamp changes a return VALUE rather than an error, so
// no existing test could have failed on it. That makes this table the single
// likeliest place the change ships broken.
//
// The skew return is the PRE-clamp difference producedAt - receivedAt, so the
// reporting threshold in the handlers is an ordinary comparison against it and
// the equal-stamps boundary is exactly zero rather than a re-parsed near-miss.
func TestParseVoiceEventTimeClampsAndMeasuresSkew(t *testing.T) {
	receivedAt := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	rfc := func(tm time.Time) string { return tm.Format(time.RFC3339Nano) }

	for _, tc := range []struct {
		name     string
		raw      string
		wantAt   time.Time
		wantSkew time.Duration
		wantErr  bool
	}{
		{name: "unparseable", raw: "not-a-time", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		// The representability guard's LOWER half. No test existed for it
		// before #3205, on either side of the boundary.
		{name: "pre-epoch", raw: "1969-12-31T23:59:59Z", wantErr: true},
		{name: "the epoch itself", raw: "1970-01-01T00:00:00Z", wantErr: true},
		{name: "beyond representable", raw: "9999-01-01T00:00:00Z", wantErr: true},
		{
			name: "future is clamped to receipt",
			raw:  rfc(receivedAt.Add(time.Hour)),
			// The clamp is zero-tolerance: only the REPORT is threshold-gated.
			wantAt: receivedAt, wantSkew: time.Hour,
		},
		{
			// One nanosecond, which pins that there is no tolerance band. A band
			// would make min() discontinuous at its edge, and boundary
			// discontinuity is what PR #3201 proved harmful.
			name: "one nanosecond ahead is still clamped",
			raw:  rfc(receivedAt.Add(time.Nanosecond)),
			// Reported skew is the pre-clamp difference, not zero.
			wantAt: receivedAt, wantSkew: time.Nanosecond,
		},
		{
			name:   "past passes through verbatim",
			raw:    rfc(receivedAt.Add(-time.Hour)),
			wantAt: receivedAt.Add(-time.Hour), wantSkew: -time.Hour,
		},
		{
			// Boundary: equal must report exactly zero skew. A clamp that
			// reported at == would turn every in-sync event into a shed record.
			name:   "equal passes through and measures zero skew",
			raw:    rfc(receivedAt),
			wantAt: receivedAt, wantSkew: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, skew, err := voice.ParseVoiceEventTimeForTest(tc.raw, receivedAt)
			if tc.wantErr {
				require.Error(t, err)
				require.Zero(t, skew, "a rejected stamp must not report a skew")
				return
			}
			require.NoError(t, err)
			require.True(t, at.Equal(tc.wantAt), "got %s want %s", at, tc.wantAt)
			require.Equal(t, tc.wantSkew, skew)
			require.False(t, at.After(receivedAt),
				"the returned stamp exceeds the receipt time it is bounded by")
		})
	}
}
