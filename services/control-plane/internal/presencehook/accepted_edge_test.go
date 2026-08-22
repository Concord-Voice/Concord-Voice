package presencehook_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
)

// capturingQuerier records the SQL and arguments AcceptedEdgeExists issues,
// without a database.
//
// It hands back a *sql.Row from a CLOSED pool rather than a nil one: a nil
// *sql.Row panics inside Scan, which would make every assertion below depend on
// a recover() and would hide a genuine nil-handling regression behind the same
// recover. A closed pool yields a Row whose Scan returns "sql: database is
// closed", so the error path is exercised honestly.
type capturingQuerier struct {
	closed *sql.DB
	query  string
	args   []any
}

func newCapturingQuerier(t *testing.T) *capturingQuerier {
	t.Helper()
	db, err := sql.Open("postgres", "postgres://invalid/invalid?sslmode=disable")
	require.NoError(t, err, "sql.Open does not dial, so this cannot fail on connectivity")
	require.NoError(t, db.Close())
	return &capturingQuerier{closed: db}
}

func (c *capturingQuerier) QueryRowContext(
	ctx context.Context, query string, args ...any,
) *sql.Row {
	c.query = query
	c.args = args
	return c.closed.QueryRowContext(ctx, query, args...)
}

func TestAcceptedEdgeExistsCarriesNoLockingClause(t *testing.T) {
	q := newCapturingQuerier(t)
	principal, counterpart := uuid.New(), uuid.New()

	_, err := presencehook.AcceptedEdgeExists(context.Background(), q, principal, counterpart)
	require.Error(t, err, "a closed pool must surface an error, not a false negative")

	upper := strings.ToUpper(q.query)
	for _, clause := range []string{"FOR UPDATE", "FOR NO KEY UPDATE", "FOR SHARE", "FOR KEY SHARE"} {
		require.NotContains(t, upper, clause,
			"C2: the probe predicate must take no row lock — a pooled autocommit read has no lock_timeout, "+
				"so a locking clause there could block indefinitely")
	}
	require.Contains(t, upper, "STATUS = 'ACCEPTED'",
		"the predicate must match the accepted-edge status the write destroys")
	require.Equal(t, []any{principal, counterpart}, q.args,
		"binding order is part of the shared contract, not a per-call-site choice")
}

func TestAcceptedEdgeExistsWrapsTheDriverError(t *testing.T) {
	// The probe fails OPEN at its call sites (C4), which only works if the
	// caller can distinguish an error from a false verdict. Returning
	// (false, nil) on a driver fault would silently deny.
	q := newCapturingQuerier(t)

	exists, err := presencehook.AcceptedEdgeExists(
		context.Background(), q, uuid.New(), uuid.New())

	require.Error(t, err)
	require.False(t, exists)
	require.Contains(t, err.Error(), "read accepted friendship edge",
		"the wrap names the read so a fail-open log says which probe failed")
}
