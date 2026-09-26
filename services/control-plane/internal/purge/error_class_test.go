package purge

// Unit coverage for ErrorClass's closed label set (#3463 review). No database:
// every case is a synthetic error value.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

// fakeNetError is a minimal net.Error, since neither stdlib nor this repo
// exports a trivially constructible one for the network branch.
type fakeNetError struct{}

func (fakeNetError) Error() string   { return "fake network error" }
func (fakeNetError) Timeout() bool   { return false }
func (fakeNetError) Temporary() bool { return false }

func TestErrorClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"pq error", &pq.Error{Code: "23514"}, "sqlstate_23514"},
		{"wrapped pq error", fmt.Errorf("wrap: %w", &pq.Error{Code: "40001"}), "sqlstate_40001"},
		{"deadline exceeded", context.DeadlineExceeded, "deadline"},
		{
			"joined deadline and a rolled-back commit's bare ErrTxDone",
			errors.Join(context.DeadlineExceeded, sql.ErrTxDone),
			"deadline",
		},
		{"canceled", context.Canceled, "canceled"},
		{"driver bad conn", driver.ErrBadConn, "bad_conn"},
		{"sql conn done", sql.ErrConnDone, "bad_conn"},
		{"sql tx done", sql.ErrTxDone, "tx_done"},
		{"sql no rows", sql.ErrNoRows, "no_rows"},
		{"network error", fakeNetError{}, "network"},
		{"unrecognized error", errors.New("boom"), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ErrorClass(tc.err))
		})
	}
}
