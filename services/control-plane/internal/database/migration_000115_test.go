package database_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Migration 000115 adds idx_media_files_storage_key_all, the lookup index the
// tier-2 orphan reaper's claim check rides. Two properties are load-bearing and
// neither is obvious from the index name, so both are pinned against a real
// database rather than against the SQL text.

// TestMigration000115_IndexIsNonPartialAndPairKeyed locks the shape.
//
// NON-PARTIAL is the whole reason this index exists alongside the one 000042
// already created. That one is `UNIQUE ... WHERE deleted_at IS NULL`, and a
// partial index cannot serve a predicate that must also see rows OUTSIDE its
// own WHERE clause — which is exactly what the reaper asks, because a
// soft-deleted row still claims its object. Adding a WHERE clause here would
// leave the reaper sequentially scanning media_files once per batch, with no
// symptom other than cost.
//
// PAIR-KEYED because "a row claims this object" is a statement about (bucket,
// key). A key-only index would answer a different question and spare a genuine
// orphan whose key happens to exist on another backend.
func TestMigration000115_IndexIsNonPartialAndPairKeyed(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	// Scan fails with sql.ErrNoRows if 000115 never ran, so this test cannot
	// pass against a database missing the migration.
	var indexdef string
	require.NoError(t, db.QueryRow(`
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'media_files' AND indexname = 'idx_media_files_storage_key_all'`,
	).Scan(&indexdef))

	assert.NotContains(t, indexdef, "WHERE",
		"the index must stay non-partial: the reaper's claim check reads rows with deleted_at set")
	assert.Contains(t, indexdef, "storage_key")
	assert.Contains(t, indexdef, "storage_backend")
	assert.NotContains(t, indexdef, "UNIQUE",
		"soft-deleted rows legitimately share a storage_key with the live row that replaced them")
}
