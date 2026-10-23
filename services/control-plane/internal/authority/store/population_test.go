package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"
)

// isSealedRaise is the population guard's refusal and nothing else (B3-D2,
// B1-D32): P0001 is the SQLSTATE of every plpgsql RAISE that names none of
// its own, so the code alone, or the message alone, must never read as sealed.
func TestIsSealedRaiseNeedsTheMessage(t *testing.T) {
	const msg = "authority.legacy_population: sealed"
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"the guard's raise", &pq.Error{Code: "P0001", Message: msg}, true},
		{"the guard's raise, wrapped", fmt.Errorf("insert: %w", &pq.Error{Code: "P0001", Message: msg}), true},
		{"P0001 with another message", &pq.Error{Code: "P0001", Message: "authority.legacy_population: other"}, false},
		{"P0001 with an empty message", &pq.Error{Code: "P0001"}, false},
		{"the message under another code", &pq.Error{Code: "23505", Message: msg}, false},
		{"a non-pq error with the message", errors.New(msg), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSealedRaise(c.err); got != c.want {
				t.Fatalf("isSealedRaise(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
