package testhelpers

import (
	"database/sql"
	"sync"
	"testing"
)

var (
	nullableColsMu    sync.Mutex
	nullableColsCache = map[string][]string{}
)

// NullableNoDefaultColumns returns the columns of table whose NULL is reachable
// by an INSERT that simply omits them — nullable AND carrying no default. That
// is exactly the shape of the SSO adapter's insert in internal/auth, so a
// fixture built from this set widens automatically when the next such column is
// added rather than silently leaving it untested (#3290).
//
// The `column_default IS NULL` clause is load-bearing, not defensive. Without
// it the set also picks up created_at and last_used_at, which are nullable but
// carry DEFAULT CURRENT_TIMESTAMP; NULLing those fails with "converting NULL to
// time.Time is unsupported", so the fixture would be permanently red for a
// reason unrelated to the defect, and greening it would need a migration.
//
// Documented residual: a future nullable column WITH a default escapes this
// filter. That is acceptable — such a column is NULL only when a writer
// explicitly writes NULL, which is a deliberate act rather than an omission,
// and deliberate acts are what a hand-written fixture is for.
//
// Cached per process: SetupTestDB serializes every package behind one global
// advisory lock, so per-test DDL introspection multiplies inside that window.
func NullableNoDefaultColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()

	nullableColsMu.Lock()
	defer nullableColsMu.Unlock()
	if cols, ok := nullableColsCache[table]; ok {
		return cols
	}

	rows, err := db.Query(
		`SELECT column_name FROM information_schema.columns
		 WHERE table_name = $1 AND is_nullable = 'YES' AND column_default IS NULL
		 ORDER BY column_name`, table)
	if err != nil {
		t.Fatalf("testhelpers: nullable-column introspection failed for %q: %v", table, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("testhelpers: close nullable-column rows for %q: %v", table, err)
		}
	}()

	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("testhelpers: scan column name for %q: %v", table, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("testhelpers: iterate columns for %q: %v", table, err)
	}

	nullableColsCache[table] = cols
	return cols
}
