package mfaenforce_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

func TestIsLockConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"deadlock_detected", &pq.Error{Code: "40P01"}, true},
		{"lock_not_available", &pq.Error{Code: "55P03"}, true},
		{"wrapped", fmt.Errorf("lock server: %w", &pq.Error{Code: "55P03"}), true},
		// How LockGateTx reports a users-row timeout: a 500 whose Cause wraps it.
		{"inside a *stepup.Error", &stepup.Error{
			Status: http.StatusInternalServerError,
			Cause:  fmt.Errorf("lock step-up subject: %w", &pq.Error{Code: "40P01"}),
		}, true},
		// A serialization failure is not a lock conflict: retrying it with
		// Retry-After would hide a REPEATABLE READ caller, which the gate refuses.
		{"serialization_failure", &pq.Error{Code: "40001"}, false},
		{"unique_violation", &pq.Error{Code: "23505"}, false},
		{"not a database error", errors.New("55P03"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, mfaenforce.IsLockConflict(tc.err))
		})
	}
}
