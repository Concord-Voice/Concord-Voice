package billing

import (
	"context"
	"errors"
	"net"

	"github.com/lib/pq"
	"github.com/stripe/stripe-go/v87"
)

// maxErrorClassLen bounds errorClass: the class is a log field and a metric
// label, so Stripe sending a longer type string than it documents must not
// widen either.
const maxErrorClassLen = 32

// isResourceMissing reports whether err is Stripe's resource_missing, the
// answer for an object that was deleted or never existed in this account.
func isResourceMissing(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) && se.Code == stripe.ErrorCodeResourceMissing
}

// errorClass names the kind of a failure without carrying its message, which
// can hold request detail. It returns "unactionable", the Stripe error type,
// "timeout", "canceled", "network", "db_<SQLSTATE>" for a PostgreSQL error, or
// "db" for anything else, and never more than maxErrorClassLen characters.
// SQLSTATE codes carry no values, so they are safe to log.
func errorClass(err error) string {
	var se *stripe.Error
	var pe *pq.Error
	var ne net.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrUnactionable):
		return "unactionable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &se):
		return boundedClass(string(se.Type), "stripe")
	case errors.As(err, &pe) && pe.Code != "":
		return boundedClass("db_"+string(pe.Code), "db")
	case errors.As(err, &ne):
		return "network"
	default:
		return "db"
	}
}

// logKeyStage names the step a failure happened in. Its values are constants
// from the code, never error text.
const logKeyStage = "stage"

// stagedError tags err with the constant step that produced it, so a log line
// that carries only errorClass can still tell twelve database failures apart.
type stagedError struct {
	stage string
	err   error
}

func (e *stagedError) Error() string { return e.err.Error() }
func (e *stagedError) Unwrap() error { return e.err }

// withStage tags err with stage; nil stays nil. errorStage reports the
// outermost tag, so tag once, at the statement that failed.
func withStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &stagedError{stage: stage, err: err}
}

// errorStage returns the stage withStage attached to err, or "unknown".
func errorStage(err error) string {
	var se *stagedError
	if errors.As(err, &se) {
		return se.stage
	}
	return "unknown"
}

// boundedClass returns s, or fallback when s is empty, cut to maxErrorClassLen.
func boundedClass(s, fallback string) string {
	if s == "" {
		return fallback
	}
	if len(s) > maxErrorClassLen {
		return s[:maxErrorClassLen]
	}
	return s
}
