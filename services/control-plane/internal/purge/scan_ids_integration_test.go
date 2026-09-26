//go:build integration

package purge

// Integration coverage for ScanIDs' two failure branches (#3463 review): a
// scan error and an iteration error that surfaces only after rows have
// already streamed back. Both need real Postgres/lib/pq error timing, which a
// fake *sql.Rows cannot reproduce. Skipped when DATABASE_URL is unset.

import (
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScanIDsWrapsAScanError: a NULL in the one text column fails rows.Scan
// into a non-nullable string, wrapped as "scan row".
func TestScanIDsWrapsAScanError(t *testing.T) {
	db := sweepTestDB(t)
	rows, err := db.Query(`SELECT NULL::text`)
	require.NoError(t, err)

	ids, scanErr := ScanIDs(rows)

	require.Error(t, scanErr)
	assert.Nil(t, ids)
	assert.ErrorContains(t, scanErr, "scan row")
}

// TestScanIDsWrapsAnIterationError: a runtime error (division by zero) that
// fires only after many rows have already streamed back surfaces through
// rows.Err(), wrapped as "iterate rows", with the SQLSTATE still recoverable
// via errors.As.
//
// OBSERVED, and worth recording because it cost several failed attempts: a
// literal "ELSE (1/0)::text" — the form this test started from — fails at
// db.Query() itself, every time, regardless of row count, padding, a real
// table in place of generate_series(), or the extended (bind-parameter)
// protocol in place of the simple one. The cause is not buffering or
// materialization (both were tried and ruled out) — it is that "1/0" is a
// constant with no column reference, so Postgres's planner constant-folds it
// during planning (even a bare EXPLAIN, with no execution, reproduces the same
// error), before any row is ever scanned. Making the divisor reference the
// loop column (here, "1000 - g", zero only at g=1000) defeats constant
// folding and restores genuine per-row evaluation: verified to read exactly
// 999 rows through rows.Next() before rows.Err() surfaces the error.
func TestScanIDsWrapsAnIterationError(t *testing.T) {
	db := sweepTestDB(t)
	rows, err := db.Query(`SELECT CASE WHEN g < 1000 THEN g::text ELSE (1 / (1000 - g))::text END
		FROM generate_series(1, 2000) g`)
	require.NoError(t, err, "the first 999 rows must stream, so Query itself must succeed")

	ids, scanErr := ScanIDs(rows)

	require.Error(t, scanErr)
	assert.Nil(t, ids)
	assert.ErrorContains(t, scanErr, "iterate rows")
	var pqErr *pq.Error
	require.ErrorAs(t, scanErr, &pqErr, "SQLSTATE must survive the iterate-rows wrap")
	assert.Equal(t, "22012", string(pqErr.Code), "division_by_zero")
}
