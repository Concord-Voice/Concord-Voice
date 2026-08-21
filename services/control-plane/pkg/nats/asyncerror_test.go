package nats

import (
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// Issue #2854 stage B1.
//
// Without an async error handler, nats.go v1.53.1 sheds SILENTLY: each async
// subscription defaults to DefaultSubPendingMsgsLimit (500k) and
// DefaultSubPendingBytesLimit (64MB), and on overflow sets ErrSlowConsumer,
// increments a drop counter and discards -- with no log line and no
// failure_class. That made it a SECOND shedder alongside the ingress gates in
// internal/voice, and an invisible one.

// THE CLASSIFICATION IS THE POINT. An earlier revision stamped slow_consumer
// onto every invocation, so a permissions violation -- which permanently kills
// the subscription and loses 100% of that subject's traffic -- rendered as a
// benign consumer-lag signal. The operator would tune queue depth while the
// erasure-clear door was dead.
func TestAsyncErrorsAreClassifiedByCause(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		class string
	}{
		"slow consumer is backpressure":        {nats.ErrSlowConsumer, "slow_consumer"},
		"permissions violation is fatal":       {nats.ErrPermissionViolation, "subscription_fatal"},
		"authorization failure is fatal":       {nats.ErrAuthorization, "subscription_fatal"},
		"anything else is a generic async err": {errors.New("flusher write failed"), "async_error"},
	} {
		t.Run(name, func(t *testing.T) {
			line := asyncErrorLogLine(nil, tc.err)
			require.Contains(t, line, `failure_class="`+tc.class+`"`,
				"a shed message and a dead subscription must not share a class")
		})
	}
}

// Wrapped errors must classify the same way -- nats.go wraps in several paths.
func TestClassificationUnwraps(t *testing.T) {
	wrapped := errors.Join(errors.New("context"), nats.ErrPermissionViolation)
	require.Equal(t, "subscription_fatal", classifyAsyncError(wrapped),
		"errors.Is semantics, not string matching")
}

// A nil subscription must report an UNKNOWN drop total, never zero. Rendering an
// unknown as 0 would read in a log or an alert as "nothing was dropped", the
// opposite of what this handler exists to surface.
func TestAsyncErrorLogLineDistinguishesUnknownDropTotalFromZero(t *testing.T) {
	line := asyncErrorLogLine(nil, errors.New("boom"))

	require.Contains(t, line, "dropped_total=-1", "unknown is -1, never silently 0")
	require.NotContains(t, line, "dropped_total=0",
		"an unknown drop total must not be indistinguishable from no drops")
}

// A real subscription contributes its subject. The drop total still reports
// unknown here: nats.Subscription.Dropped() returns ErrBadSubscription unless
// attached to a live connection, and those fields are unexported, so the
// assignment is the one statement that needs a running server. Not worth one.
func TestAsyncErrorLogLineCarriesTheSubjectOfARealSubscription(t *testing.T) {
	sub := &nats.Subscription{Subject: "voice.>"}

	line := asyncErrorLogLine(sub, nats.ErrSlowConsumer)

	require.Contains(t, line, `subject="voice.>"`,
		"the shed must be attributable to the subscription it happened on")
	require.Contains(t, line, "dropped_total=-1",
		"a subscription with no live connection reports unknown, not zero")
}

// CWE-117. This is a LOGIC test, not a doc pin.
//
// An earlier revision passed a nil subscription and asserted subject="" — which
// proves the format string has quote characters around an empty string and would
// pass identically if the value were %s-rendered and a control character DID
// split the record. Both attacker-influencable values are exercised with a real
// newline here.
//
// err is the one genuinely remote-controlled value: on the permission-violation
// path nats.go builds it verbatim from the server's -ERR payload, and this
// deployment's bus has no authentication.
func TestControlCharactersCannotForgeALogRecord(t *testing.T) {
	forged := "\nNATS async error subject=\"x\" failure_class=\"slow_consumer\""

	t.Run("in the subject", func(t *testing.T) {
		line := asyncErrorLogLine(&nats.Subscription{Subject: "voice." + forged}, nats.ErrSlowConsumer)
		require.NotContains(t, line, "\n", "a raw newline would split this into two records")
		require.Contains(t, line, `\n`, "it must render as a two-character escape")
		// The forged text still APPEARS -- escaped, inside the quoted subject.
		// That is harmless and is the point: what forges a record is a raw
		// newline, not the presence of field-shaped text within a quoted value.
		// So assert the line is still ONE physical record rather than counting
		// occurrences of the literal.
		require.Equal(t, 1, strings.Count(line, "\n")+1,
			"one physical log record regardless of what the subject spells")
	})

	t.Run("in the error", func(t *testing.T) {
		line := asyncErrorLogLine(nil, errors.New("permissions violation"+forged))
		require.NotContains(t, line, "\n", "a raw newline would split this into two records")
		require.Contains(t, line, `\n`, "it must render as a two-character escape")
	})
}
