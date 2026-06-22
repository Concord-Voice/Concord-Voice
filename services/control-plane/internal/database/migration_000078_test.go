package database_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000078 flips the privacy_settings.load_gifs_automatically column
// default from FALSE to TRUE (#1766). It is metadata-only (ALTER ... SET DEFAULT)
// with NO data backfill.
//
// Scope note (per Gitar review on PR #1774): the test harness (SetupTestServer)
// applies ALL migrations — including 000078 — at setup, so it cannot seed a row
// BEFORE the migration runs. The subtests therefore verify (a) the new default
// applies to fresh rows and (b) an explicit INSERT value is not coerced by the
// new default. The stronger "previously-stored rows are left untouched by
// SET DEFAULT" invariant is NOT runtime-tested here; it holds structurally —
// PostgreSQL's SET DEFAULT is a catalog-only change that never rewrites existing
// rows, and the up migration contains no backfill UPDATE (verifiable by reading
// the .up.sql). See the design spec's Option B rejection.
func TestMigration000078_LoadGifsAutomaticallyDefaultTrue(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("a new privacy_settings row defaults load_gifs_automatically TRUE", func(t *testing.T) {
		u := ts.CreateTestUser(t, "gifdefault_new")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO privacy_settings (user_id) VALUES ($1)`, u.ID)
		require.NoError(t, err)

		var v bool
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT load_gifs_automatically FROM privacy_settings WHERE user_id = $1`, u.ID).Scan(&v))
		assert.True(t, v, "new rows must default TRUE after migration 000078")
	})

	t.Run("an explicit FALSE on INSERT is honored, not coerced by the new default", func(t *testing.T) {
		u := ts.CreateTestUser(t, "gifdefault_optout")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO privacy_settings (user_id, load_gifs_automatically) VALUES ($1, FALSE)`, u.ID)
		require.NoError(t, err)

		var v bool
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT load_gifs_automatically FROM privacy_settings WHERE user_id = $1`, u.ID).Scan(&v))
		// Proxy for the no-override property: a caller-supplied FALSE survives the
		// new DEFAULT TRUE. (Existing-row preservation is structural — see the
		// scope note on the test function above — not exercised at runtime here.)
		assert.False(t, v, "an explicit FALSE must remain FALSE — DEFAULT only applies when the column is omitted on INSERT")
	})
}
