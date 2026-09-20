package database_test

import (
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration000142_AdmitsAndRollsBackTerminalOutboxMetrics(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migrationReadFile(t, "../../migrations/000142_server_voice_terminal_outbox_ops_metrics.up.sql")
	down := migrationReadFile(t, "../../migrations/000142_server_voice_terminal_outbox_ops_metrics.down.sql")
	keys := []string{
		"server_voice_terminal_outbox_captured_total",
		"server_voice_terminal_outbox_delivered_total",
		"server_voice_terminal_outbox_successor_suppressed_total",
		"server_voice_terminal_outbox_channel_suppressed_total",
		"server_voice_terminal_outbox_lock_retained_total",
		"server_voice_terminal_outbox_queue_rescheduled_total",
	}
	catalogKeys := make([]string, 0, len(opsmetrics.Catalog()))
	for _, definition := range opsmetrics.Catalog() {
		catalogKeys = append(catalogKeys, string(definition.Key))
	}
	sort.Strings(catalogKeys)
	require.Len(t, catalogKeys, 72)
	upSamples := migration000136ConstraintKeys(up, "ops_metric_samples_metric_key_check")
	upRollups := migration000136ConstraintKeys(up, "ops_metric_rollups_metric_key_check")
	require.Len(t, upSamples, len(catalogKeys), "migration 142 must parse a non-empty live catalog")
	require.Equal(t, catalogKeys, upSamples)
	require.Equal(t, catalogKeys, upRollups)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	_, err = tx.Exec(down)
	require.NoError(t, err)
	downDefinition := migration000142ConstraintDefinition(t, tx)
	for _, key := range keys {
		require.NotContains(t, downDefinition, key, "down migration must remove %q", key)
	}
	downRollupDefinition := migration000142ConstraintDefinitionNamed(t, tx, "ops_metric_rollups_metric_key_check")
	for _, key := range keys {
		require.NotContains(t, downRollupDefinition, key, "down migration must remove %q from rollups", key)
	}

	_, err = tx.Exec(up)
	require.NoError(t, err)
	upDefinition := migration000142ConstraintDefinition(t, tx)
	for _, key := range keys {
		require.Contains(t, upDefinition, key, "up migration must admit %q", key)
	}
	upRollupDefinition := migration000142ConstraintDefinitionNamed(t, tx, "ops_metric_rollups_metric_key_check")
	for _, key := range catalogKeys {
		require.Contains(t, upRollupDefinition, key, "up migration must admit %q in rollups", key)
	}
}

func migration000142ConstraintDefinition(t *testing.T, tx *sql.Tx) string {
	return migration000142ConstraintDefinitionNamed(t, tx, "ops_metric_samples_metric_key_check")
}

func migration000142ConstraintDefinitionNamed(t *testing.T, tx *sql.Tx, name string) string {
	t.Helper()
	var definition string
	row := tx.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, name)
	require.NoError(t, row.Scan(&definition))
	return strings.ToLower(definition)
}
