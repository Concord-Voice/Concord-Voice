package database_test

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000183 (epic #2033 PR-2, Refs #1306): admits the two Stripe billing
// operations metrics to the closed ops_metric_* CHECKs. Rollback follows the
// 000142 protocol: lock, delete the retired rows, restore the narrower CHECK.

// migration000183NewKeys are the two keys this migration admits: the pending
// erasure-obligation gauge and the drift-correction counter (spec section 14).
var migration000183NewKeys = []string{
	"billing_erasure_outbox_pending",
	"billing_drift_corrections_total",
}

// migration000183CatalogKeys is the catalog AS OF 000183: the frozen 72-key
// 000142 set plus the two keys above, sorted.
//
// A literal pin, so the SQL is checked against a fixed list rather than
// whatever the Go catalog holds today. The Go catalog already carries both
// keys, and TestMigration000183_FilesAndSchemaLock also checks that
// opsmetrics.Catalog() equals this list: a metric added to the catalog without
// a migration widening the CHECK fails there, not in production. The next
// migration that admits keys moves that catalog check into its own test.
func migration000183CatalogKeys() []string {
	keys := append([]string{}, migration000142FrozenKeys()...)
	keys = append(keys, migration000183NewKeys...)
	sort.Strings(keys)
	return keys
}

func migration000183SQL(t *testing.T, direction string) string {
	t.Helper()
	return migrationReadFile(t, filepath.Join("..", "..", "migrations",
		"000183_billing_ops_metrics."+direction+".sql"))
}

func TestMigration000183_FilesAndSchemaLock(t *testing.T) {
	up := migration000183SQL(t, "up")
	down := migration000183SQL(t, "down")
	readme := migrationReadFile(t, filepath.Join("..", "..", "migrations", "README.md"))

	catalogKeys := migration000183CatalogKeys()
	require.Len(t, catalogKeys, 74)

	upSamples := migration000136ConstraintKeys(up, "ops_metric_samples_metric_key_check")
	// Guards the silent-empty failure described on migration000136MetricLiteral:
	// a prefix the regex does not know yields nil, and nil == nil would pass.
	// "billing" is a prefix the shared regex predates, so this Len is load-bearing.
	require.Len(t, upSamples, len(catalogKeys),
		"parsed no usable key set from the up SQL -- check migration000136MetricLiteral covers every prefix")
	assert.Equal(t, catalogKeys, upSamples)
	assert.Equal(t, catalogKeys, migration000136ConstraintKeys(up, "ops_metric_rollups_metric_key_check"))

	// The down constraint is exactly the 000142 catalog: this migration's keys removed.
	frozen := migration000142FrozenKeys()
	require.Len(t, frozen, 72)
	assert.Equal(t, frozen, migration000136ConstraintKeys(down, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, frozen, migration000136ConstraintKeys(down, "ops_metric_rollups_metric_key_check"))

	// Rollback ordering: lock, then delete the retired rows, then restore the
	// narrower constraint, so a pre-rollback writer cannot fail the ALTER.
	lock := strings.Index(down, "LOCK TABLE ops_metric_samples, ops_metric_rollups IN ACCESS EXCLUSIVE MODE")
	deleteSamples := strings.Index(down, "DELETE FROM ops_metric_samples WHERE metric_key IN")
	deleteRollups := strings.Index(down, "DELETE FROM ops_metric_rollups WHERE metric_key IN")
	restoreSamples := strings.Index(down, "ADD CONSTRAINT ops_metric_samples_metric_key_check")
	restoreRollups := strings.Index(down, "ADD CONSTRAINT ops_metric_rollups_metric_key_check")
	for name, pos := range map[string]int{"lock": lock, "delete samples": deleteSamples,
		"delete rollups": deleteRollups, "restore samples": restoreSamples, "restore rollups": restoreRollups} {
		require.NotEqual(t, -1, pos, "down must contain %s", name)
	}
	assert.Less(t, lock, deleteSamples)
	assert.Less(t, lock, deleteRollups)
	assert.Less(t, deleteSamples, restoreSamples)
	assert.Less(t, deleteRollups, restoreRollups)
	for _, key := range migration000183NewKeys {
		assert.Contains(t, down, "'"+key+"'", "down must delete rows for %s", key)
	}

	assert.Contains(t, readme, "| 000183 | billing_ops_metrics |")

	// The Go catalog and the DB CHECK admit the same keys. Without this, a key
	// added to opsmetrics with no migration passes every test and is refused
	// by the CHECK on its first write.
	goKeys := make([]string, 0, opsmetrics.CatalogSize())
	for _, def := range opsmetrics.Catalog() {
		goKeys = append(goKeys, string(def.Key))
	}
	sort.Strings(goKeys)
	assert.Equal(t, catalogKeys, goKeys,
		"opsmetrics.Catalog() must equal the key set the latest metrics migration admits")
}

// TestMigration000183_UpDownReUp proves the constraints gate the keys in both
// directions, in both metric tables, against a real database.
func TestMigration000183_UpDownReUp(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	const nodeID = "cvn_cccccccccccccccc"
	insertSample := func(key string) error {
		_, err := tx.Exec(`
			INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
			VALUES ($1, $2, '2026-10-21 12:00:00+00'::timestamptz, 1)`, nodeID, key)
		return err
	}
	insertRollup := func(key string) error {
		_, err := tx.Exec(`
			INSERT INTO ops_metric_rollups
				(node_id, metric_key, bucket_start, min_value, max_value, avg_value, last_value, sample_count)
			VALUES ($1, $2, '2026-10-21 12:00:00+00'::timestamptz, 1, 1, 1, 1, 1)`, nodeID, key)
		return err
	}
	// attemptRejected runs the insert inside a savepoint so an expected CHECK
	// violation does not abort the surrounding transaction.
	attemptRejected := func(insert func(string) error, key string) error {
		_, err := tx.Exec(`SAVEPOINT migration000183_rejection`)
		require.NoError(t, err)
		insertErr := insert(key)
		_, err = tx.Exec(`ROLLBACK TO SAVEPOINT migration000183_rejection`)
		require.NoError(t, err)
		_, err = tx.Exec(`RELEASE SAVEPOINT migration000183_rejection`)
		require.NoError(t, err)
		return insertErr
	}

	upSQL := migration000183SQL(t, "up")
	downSQL := migration000183SQL(t, "down")

	// Establish the up state explicitly: older migration tests re-apply their own
	// narrower CHECKs with raw SQL, so the live constraint depends on test order.
	// The up migration is DROP then ADD, so re-applying it is idempotent.
	_, err = tx.Exec(upSQL)
	require.NoError(t, err, "failed to establish the up state")

	for _, key := range migration000183NewKeys {
		require.NoError(t, insertSample(key), "the applied constraint must accept %s in samples", key)
		require.NoError(t, insertRollup(key), "the applied constraint must accept %s in rollups", key)
	}

	_, err = tx.Exec(downSQL)
	require.NoError(t, err, "the down migration must not fail on the rows inserted above")

	for _, key := range migration000183NewKeys {
		for table, insert := range map[string]func(string) error{
			"ops_metric_samples": insertSample, "ops_metric_rollups": insertRollup,
		} {
			var remaining int
			require.NoError(t, tx.QueryRow(
				`SELECT COUNT(*) FROM `+table+` WHERE metric_key = $1`, key).Scan(&remaining))
			assert.Zero(t, remaining, "rollback must clear %s rows carrying %s", table, key)
			require.Error(t, attemptRejected(insert, key),
				"the restored 72-key constraint must reject %s in %s", key, table)
		}
	}
	// The restored constraint still admits the previous catalog's newest key.
	require.NoError(t, insertSample("server_voice_terminal_outbox_queue_rescheduled_total"))

	_, err = tx.Exec(upSQL)
	require.NoError(t, err)
	for _, key := range migration000183NewKeys {
		require.NoError(t, insertSample(key), "the reapplied constraint must accept %s again", key)
		require.NoError(t, insertRollup(key), "the reapplied constraint must accept %s in rollups again", key)
	}
}
