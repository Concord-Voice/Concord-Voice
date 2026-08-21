package nats

import (
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// Closed-vocabulary classes for the client library's async error callback.
//
// The callback fires for far more than backpressure. In nats.go v1.53.1 it is
// invoked from at least seven sites: slow consumer, bad message headers,
// permissions violation, authorization error, flusher write error, and several
// connect-protocol read failures. Only the first is a shed message; the rest are
// connection- or subscription-level faults, and two of them are FATAL to the
// subscription.
//
// An earlier revision stamped classSlowConsumer onto every invocation. That made
// a permissions violation -- which permanently kills the subscription and loses
// 100% of that subject's traffic -- render as a consumer-lag signal, so an
// operator would tune queue depth while the erasure-clear door was dead. It also
// poisoned slow-consumer alerting with unrelated events. The misattribution gets
// WORSE once the bus is authenticated (#2857), because permission violations go
// from impossible to the expected failure mode.
const (
	// classSlowConsumer: the library shed a message because the subscription's
	// pending queue was full. Backpressure. Recoverable.
	classSlowConsumer = "slow_consumer"
	// classSubscriptionFatal: permissions or authorization. The subscription is
	// gone and will not recover on its own. Every message on that subject is
	// lost until the credential or permission is fixed.
	classSubscriptionFatal = "subscription_fatal"
	// classAsyncError: anything else the callback reports.
	classAsyncError = "async_error"
)

// classifyAsyncError maps a nats.go async error to the closed vocabulary.
func classifyAsyncError(err error) string {
	switch {
	case errors.Is(err, nats.ErrSlowConsumer):
		return classSlowConsumer
	case errors.Is(err, nats.ErrPermissionViolation), errors.Is(err, nats.ErrAuthorization):
		return classSubscriptionFatal
	default:
		return classAsyncError
	}
}

// asyncErrorLogLine renders the async-error report.
//
// Extracted as a pure function so it is testable without a live NATS server.
// Testing this by grepping client.go for "nats.ErrorHandler(" would be a
// documentation pin, not a logic test -- it would stay green while the rendered
// line lost the drop count or misclassified the failure.
//
// BOTH attacker-influencable values are rendered with %q. The subject is ours,
// but err is not: on the permission-violation path nats.go builds it verbatim
// from the server's -ERR payload, so under this deployment's threat model (no
// bus authentication, untrusted shared network) a hostile or on-path broker can
// embed a newline and forge a log record in this stdlib sink. That is exactly
// the unescaped sink [internal]rules/observability.md core principle 5 scopes its
// mandate to.
func asyncErrorLogLine(sub *nats.Subscription, err error) string {
	class := classifyAsyncError(err)

	subject := ""
	// -1 means "not reportable", never "zero dropped". Note Dropped() is a
	// CUMULATIVE lifetime total for the subscription, not a delta -- two
	// consecutive lines both reading 5000 mean no new drops.
	droppedTotal := -1
	if sub != nil {
		subject = sub.Subject
		if n, dropErr := sub.Dropped(); dropErr == nil {
			droppedTotal = n
		}
	}

	return fmt.Sprintf(
		"NATS async error subject=%q failure_class=%q dropped_total=%d err=%q",
		subject, class, droppedTotal, errText(err))
}

// errText renders a possibly-nil error for %q.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
