package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

func TestIsResourceMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"resource_missing", &stripe.Error{Code: stripe.ErrorCodeResourceMissing, Type: stripe.ErrorTypeInvalidRequest}, true},
		{"wrapped resource_missing", fmt.Errorf("billing: retrieve subscription: %w", &stripe.Error{Code: stripe.ErrorCodeResourceMissing}), true},
		{"other code", &stripe.Error{Code: stripe.ErrorCodeRateLimit, Type: stripe.ErrorTypeInvalidRequest}, false},
		{"invalid request without a code", &stripe.Error{Type: stripe.ErrorTypeInvalidRequest}, false},
		{"api error", &stripe.Error{Type: stripe.ErrorTypeAPI}, false},
		{"non-Stripe error", errors.New("resource_missing"), false},
		{"wrapped non-Stripe error", fmt.Errorf("wrap: %w", sql.ErrNoRows), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isResourceMissing(tt.err))
		})
	}
}

func TestErrorClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"stripe api_error", &stripe.Error{Type: stripe.ErrorTypeAPI}, "api_error"},
		{"stripe invalid_request_error", &stripe.Error{Type: stripe.ErrorTypeInvalidRequest}, "invalid_request_error"},
		{"stripe rate_limit", &stripe.Error{Type: stripe.ErrorTypeRateLimit}, "rate_limit"},
		{"wrapped stripe error", fmt.Errorf("reconcile: %w", &stripe.Error{Type: stripe.ErrorTypeAPI}), "api_error"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"wrapped deadline", fmt.Errorf("fetch: %w", context.DeadlineExceeded), "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"wrapped canceled", fmt.Errorf("fetch: %w", context.Canceled), "canceled"},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "network"},
		{"database", sql.ErrConnDone, "db"},
		{"unactionable", ErrUnactionable, "unactionable"},
		{"wrapped unactionable", fmt.Errorf("%w: payer absent", ErrUnactionable), "unactionable"},
		{"staged unactionable", withStage(stageFetchSub, fmt.Errorf("%w: no customer", ErrUnactionable)), "unactionable"},
		{"postgres deadlock", &pq.Error{Code: "40P01"}, "db_40P01"},
		{"wrapped postgres error", fmt.Errorf("lock payer: %w", &pq.Error{Code: "57P01"}), "db_57P01"},
		{"staged postgres error", withStage(stageUpsert, &pq.Error{Code: "23505"}), "db_23505"},
		{"postgres error without a sqlstate", &pq.Error{}, "db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := errorClass(tt.err)
			assert.Equal(t, tt.want, got)
			assert.LessOrEqual(t, len(got), 32, "the class is a metric label and a log field")
		})
	}
}

// TestErrorClass_BoundedLength proves no branch can exceed 32 characters, even
// when Stripe sends a type string longer than any it documents.
func TestErrorClass_BoundedLength(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"oversized stripe type": &stripe.Error{Type: stripe.ErrorType(strings.Repeat("x", 200))},
		"empty stripe type":     &stripe.Error{},
		"oversized sqlstate":    &pq.Error{Code: pqerror.Code(strings.Repeat("9", 64))},
		"unknown error":         errors.New(strings.Repeat("boom ", 40)),
		"nil":                   nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.LessOrEqual(t, len(errorClass(err)), 32)
		})
	}
}

// A SQLSTATE longer than any PostgreSQL defines is cut at the bound, keeping
// its prefix, rather than widening the field.
func TestErrorClass_OversizedSQLStateIsCut(t *testing.T) {
	t.Parallel()
	got := errorClass(&pq.Error{Code: pqerror.Code(strings.Repeat("9", 64))})
	assert.Equal(t, "db_"+strings.Repeat("9", maxErrorClassLen-len("db_")), got)
}

func TestWithStage(t *testing.T) {
	t.Parallel()
	base := errors.New("boom")

	t.Run("nil stays nil", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, withStage(stageCommit, nil))
	})
	t.Run("untagged is unknown", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "unknown", errorStage(base))
		assert.Equal(t, "unknown", errorStage(nil))
		assert.Equal(t, "unknown", errorStage(fmt.Errorf("wrap: %w", base)))
	})
	t.Run("tag survives wrapping", func(t *testing.T) {
		t.Parallel()
		err := fmt.Errorf("customer deleted: %w", withStage(stageLockPayer, base))
		assert.Equal(t, stageLockPayer, errorStage(err))
	})
	t.Run("outermost tag wins", func(t *testing.T) {
		t.Parallel()
		err := withStage(stageCommit, fmt.Errorf("wrap: %w", withStage(stageLockPayer, base)))
		assert.Equal(t, stageCommit, errorStage(err))
	})
	t.Run("transparent to errors.Is, errors.As and the message", func(t *testing.T) {
		t.Parallel()
		pe := &pq.Error{Code: "40P01", Message: "deadlock detected"}
		err := withStage(stageUpsert, fmt.Errorf("upsert: %w", pe))
		assert.Equal(t, "upsert: "+pe.Error(), err.Error(), "the tag adds nothing to the text")
		var got *pq.Error
		require.ErrorAs(t, err, &got)
		assert.Same(t, pe, got)
		assert.ErrorIs(t, withStage(stageFetchSub, ErrUnactionable), ErrUnactionable)
	})
}
