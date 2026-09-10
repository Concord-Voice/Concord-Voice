package voice

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// unreachableConnector backs a *sql.DB whose every connection attempt fails.
//
// The skew report is emitted before resolveRoom, which is this package's first
// database touch, so a handler driven against this DB reaches the clamp branch
// and then returns on the ordinary state_read path. That keeps these cases pure
// unit tests: the property under test is which stamps report, and a real server
// would add a schema, a Redis and 150 ms per case without adding evidence.
type unreachableConnector struct{}

func (unreachableConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("unreachable test database")
}

func (unreachableConnector) Driver() driver.Driver { return nil }

func skewShedSubscriber(t *testing.T) (*NATSSubscriber, *[]string) {
	t.Helper()
	var sink bytes.Buffer
	observed := &[]string{}
	db := sql.OpenDB(unreachableConnector{})
	t.Cleanup(func() { _ = db.Close() })
	return &NATSSubscriber{
		db:                      db,
		log:                     logger.NewWithWriter(&sink),
		ingressShedObservedHook: func(class string) { *observed = append(*observed, class) },
	}, observed
}

// skewFrame carries every field the four lifecycle events name, so one payload
// drives all four handlers as far as the clamp.
func skewFrame(t *testing.T, stampedAt time.Time) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"channelId": uuid.NewString(),
		"userId":    uuid.NewString(),
		"userIds":   []string{},
		"timestamp": stampedAt.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	return payload
}

// The clamp is zero-tolerance; only the REPORT is threshold-gated (#3205).
//
// Bound to the constant, never to a copied literal: PR #3201's fixtures sat at
// skew/2 and skew+1m, so any bound anywhere in ~(90 s, 239 s] survived its whole
// suite and the width it claimed to pin was never pinned at all.
func TestVoiceSkewShedFiresOnlyBeyondTheBound(t *testing.T) {
	bound := maxVoiceLifecycleForwardSkew
	require.LessOrEqual(t, bound, 4*presence.ActivityStateTTL,
		"the reporting threshold grew; this test would silently move with it")

	for _, tc := range []struct {
		name   string
		ahead  time.Duration
		report bool
	}{
		// == is silent, and it is free: skew is measured BEFORE the clamp, so
		// two clocks in exact agreement produce exactly zero, and a stamp
		// exactly on the bound produces exactly the bound.
		{name: "at the bound does not report", ahead: bound},
		{name: "one nanosecond beyond reports", ahead: bound + time.Nanosecond, report: true},
		{name: "well within the bound is silent", ahead: bound / 2},
		{name: "behind the receipt clock is silent", ahead: -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subscriber, observed := skewShedSubscriber(t)
			receivedAt := time.Now()

			subscriber.handleJoined(skewFrame(t, receivedAt.Add(tc.ahead)), receivedAt)

			if tc.report {
				require.Equal(t, []string{"clock_skew"}, *observed)
				return
			}
			require.Empty(t, *observed,
				"a clamp within the bound is NTP jitter and must stay silent")
		})
	}
}

// A direct call from nats.go can be deleted with every behavioural test green --
// the same failure mode TestSubscribeInstallsTheVoiceGate exists for on the
// decorator. The subjects are DISCOVERED rather than probed from a fixed list,
// so a fifth handled subject has to report too.
func TestVoiceSkewShedIsReachedFromEveryClampSite(t *testing.T) {
	require.NotEmpty(t, voiceIngressHandledSubjects, "this pin needs updating")

	for subject := range voiceIngressHandledSubjects {
		t.Run(subject, func(t *testing.T) {
			subscriber, observed := skewShedSubscriber(t)
			receivedAt := time.Now()

			subscriber.handleVoiceLifecycleEvent(subject,
				skewFrame(t, receivedAt.Add(2*maxVoiceLifecycleForwardSkew)), receivedAt)

			require.Equal(t, []string{"clock_skew"}, *observed,
				"subject %s clamps without reporting", subject)
		})
	}
}

// The EMISSION arm, asserting the log output rather than ingressShedLoggedHook:
// a hook proves the seam ran and never that the line after it was written --
// the finding recorded on TestReportShedWritesAnAggregatedLogLine.
func TestVoiceSkewShedReportWritesOneAggregatedLogLine(t *testing.T) {
	var sink bytes.Buffer
	subscriber := &NATSSubscriber{log: logger.NewWithWriter(&sink)}

	subscriber.voiceSkewShed("clock_skew")
	require.Empty(t, sink.String(),
		"recordN arms the interval silently; recordNReportFirst would emit here, "+
			"and a clamp is APPLIED rather than dropped so nothing was lost to wake on")

	subscriber.voiceSkewShedState.mu.Lock()
	subscriber.voiceSkewShedState.loggedAt = time.Now().Add(-2 * ingressShedLogInterval)
	subscriber.voiceSkewShedState.mu.Unlock()
	subscriber.voiceSkewShed("clock_skew")

	written := sink.String()
	require.Equal(t, 1, strings.Count(written, voiceSkewShedMessage),
		"two sheds must buy exactly one aggregated line, not one line each")
	require.Contains(t, written, "clock_skew")
	require.NotContains(t, written, "resource_limit",
		"resource_limit is retired and must not be recycled for the clamp")

	// The separate state field is the point: a clamp storm must not read as an
	// admission-control shed. Those events were APPLIED, not refused.
	subscriber.voiceShedState.mu.Lock()
	defer subscriber.voiceShedState.mu.Unlock()
	require.Empty(t, subscriber.voiceShedState.counts,
		"a clamp leaked into the ingress gate's own shed counter")
}
