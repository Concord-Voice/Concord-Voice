package database_test

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migration000113MetricLiteral must admit the `presence_` prefix, which no
// earlier catalog migration used. The 000091 regex silently matched nothing for
// a key it did not anticipate, which would have made an equality assertion pass
// by comparing two empty sets — extend the alternation whenever a key with a new
// prefix is added.
var migration000113MetricLiteral = regexp.MustCompile(
	`'(?:host|service|http|websocket|channel|dm|ops|media|registered|pending|users|active|presence)_[a-z0-9_]+'`)

const migration000113NewKey = "presence_audience_suppressed_total"

// TestMigration000113_FilesAndSchemaLock pins the SQL against the 62-key set
// 000113 itself admitted.
//
// FROZEN by 000135, following the 000086 and 000091 precedent: the newest
// catalog migration owns the LIVE-catalog pin and older ones freeze their own
// list. Left reading the live catalog, this test would fail the moment any
// later migration admitted a key — asserting that a 2026 SQL file already
// contains something added after it was written.
func TestMigration000113_FilesAndSchemaLock(t *testing.T) {
	up := migration000113SQL(t, "up")
	down := migration000113SQL(t, "down")
	readme := migrationReadFile(t, filepath.Join("..", "..", "migrations", "README.md"))

	catalogKeys := migration000113FrozenKeys()
	require.Contains(t, catalogKeys, migration000113NewKey,
		"the frozen list must carry the key this migration admits, or the freeze was taken wrong")
	assert.Equal(t, catalogKeys, migration000113ConstraintKeys(up, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, catalogKeys, migration000113ConstraintKeys(up, "ops_metric_rollups_metric_key_check"))

	// The down constraint is the catalog minus exactly this migration's key.
	previous := make([]string, 0, len(catalogKeys)-1)
	for _, key := range catalogKeys {
		if key != migration000113NewKey {
			previous = append(previous, key)
		}
	}
	assert.Equal(t, previous, migration000113ConstraintKeys(down, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, previous, migration000113ConstraintKeys(down, "ops_metric_rollups_metric_key_check"))

	// Rollback ordering: lock, then delete the retired rows, then restore the
	// narrower constraint. Any other order lets a concurrent insert from a
	// pre-rollback binary fail the ALTER on data this migration thought it removed.
	lock := strings.Index(down, "LOCK TABLE ops_metric_samples, ops_metric_rollups IN ACCESS EXCLUSIVE MODE")
	deleteSamples := strings.Index(down, "DELETE FROM ops_metric_samples")
	deleteRollups := strings.Index(down, "DELETE FROM ops_metric_rollups")
	restore := strings.Index(down, "ADD CONSTRAINT ops_metric_samples_metric_key_check")
	require.NotEqual(t, -1, lock)
	require.NotEqual(t, -1, deleteSamples)
	require.NotEqual(t, -1, deleteRollups)
	require.NotEqual(t, -1, restore)
	assert.Less(t, lock, deleteSamples)
	assert.Less(t, lock, deleteRollups)
	assert.Less(t, deleteSamples, restore)
	assert.Less(t, deleteRollups, restore)

	assert.Contains(t, readme, "| 000113 | presence_audience_suppressed_metric |")
}

// TestMigration000113_UpDownReUp proves the constraint actually gates the key in
// both directions against a real database, rather than merely reading as if it does.
func TestMigration000113_UpDownReUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	const nodeID = "cvn_aaaaaaaaaaaaaaaa"

	insert := func(ts2 string, value int) error {
		_, err := ts.DB.ExecContext(ctx, `
			INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
			VALUES ($1, $2, $3::timestamptz, $4)
		`, nodeID, migration000113NewKey, ts2, value)
		if err != nil {
			return fmt.Errorf("insert ops_metric_samples row at %s: %w", ts2, err)
		}
		return nil
	}

	downSQL := migration000113SQL(t, "down")
	upSQL := migration000113SQL(t, "up")

	// Establish the up state EXPLICITLY rather than trusting what SetupTestServer
	// left behind. This is not defensive padding: TestMigration000091_UpDownReUp
	// re-applies 000091's up SQL, which rewrites this very constraint back to its
	// 61-key form — and because it does so with raw SQL, schema_migrations still
	// reads 113 while the constraint has silently regressed. Whether this test
	// sees 61 or 62 keys therefore depends on test ORDER within the package. The
	// up migration is a DROP CONSTRAINT followed by an ADD, so re-applying it is
	// idempotent and cheap.
	_, err := ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err, "failed to establish the up state")

	// Self-clean before AND after. ops_metric_samples is not covered by the
	// package's table truncation, and the primary key is (node_id, metric_key,
	// ts) — all three fixed here — so a row left behind by an earlier failed run
	// makes every later run fail on a duplicate key rather than on the constraint
	// this test is about. Diagnosing that costs more than preventing it.
	clearRows := func() {
		_, err := ts.DB.ExecContext(ctx,
			`DELETE FROM ops_metric_samples WHERE node_id = $1 AND metric_key = $2`,
			nodeID, migration000113NewKey)
		require.NoError(t, err)
	}
	clearRows()
	t.Cleanup(clearRows)

	require.NoError(t, insert("2026-08-28 12:00:00+00", 1),
		"the applied 62-key constraint must accept this migration's key")

	reapplied := false
	t.Cleanup(func() {
		if !reapplied {
			_, err := ts.DB.ExecContext(ctx, upSQL)
			require.NoError(t, err, "failed to restore the schema for subsequent tests")
		}
	})

	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err, "the down migration must not fail on the row inserted above")

	var remaining int
	require.NoError(t, ts.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ops_metric_samples WHERE metric_key = $1`, migration000113NewKey).Scan(&remaining))
	assert.Zero(t, remaining, "rollback must clear rows carrying the retired key")

	require.Error(t, insert("2026-08-28 13:00:00+00", 2),
		"the restored 61-key constraint must reject this migration's key")

	_, err = ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err)
	reapplied = true
	require.NoError(t, insert("2026-08-28 14:00:00+00", 3),
		"the reapplied 62-key constraint must accept this migration's key again")
}

func migration000113SQL(t *testing.T, direction string) string {
	t.Helper()
	return migrationReadFile(t, filepath.Join("..", "..", "migrations",
		"000113_presence_audience_suppressed_metric."+direction+".sql"))
}

func migration000113ConstraintKeys(contents, constraint string) []string {
	startMarker := "ADD CONSTRAINT " + constraint + " CHECK (metric_key IN ("
	start := strings.Index(contents, startMarker)
	if start == -1 {
		return nil
	}
	body := contents[start+len(startMarker):]
	end := strings.Index(body, "));")
	if end == -1 {
		return nil
	}
	matches := migration000113MetricLiteral.FindAllString(body[:end], -1)
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		seen[strings.Trim(match, "'")] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// migration000113FrozenKeys is the catalog AS OF 000113 — the exact 62 keys this
// migration's own CHECK admits, sorted.
//
// Deliberately a literal rather than a read of opsmetrics.Catalog(). The live
// catalog is now 64 keys and will keep growing; this SQL file is finished and
// will not. Deriving it would make this test assert that a file written in 2026
// already contains every key added since, which is false by construction and is
// exactly the failure 000135 would otherwise have caused here.
//
// Nothing may be added to this list. A new catalog key belongs in the newest
// migration's test, which owns the live pin.
func migration000113FrozenKeys() []string {
	keys := []string{
		"active_sessions_current",
		"active_users_15d",
		"active_users_24h",
		"active_users_30d",
		"active_users_7d",
		"channel_messages_total",
		"dm_messages_total",
		"host_cpu_percent",
		"host_disk_percent",
		"host_load_1m",
		"host_memory_percent",
		"http_client_errors_total",
		"http_requests_total",
		"http_server_errors_total",
		"media_camera_publishers_current",
		"media_egress_cumulative_bytes",
		"media_egress_current_bps",
		"media_egress_peak_bps",
		"media_participant_hours_audio",
		"media_participant_hours_screenshare",
		"media_participant_hours_webcam",
		"media_participants_audio_current",
		"media_participants_screenshare_current",
		"media_participants_webcam_current",
		"media_peak_video_publishers_per_room",
		"media_rooms_current",
		"media_screen_publishers_current",
		"media_uploads_total",
		"ops_snapshot_rejections_total",
		"pending_registrations_current",
		"presence_audience_suppressed_total",
		"registered_users_current",
		"service_control_plane_cpu_percent",
		"service_control_plane_healthy",
		"service_control_plane_memory_bytes",
		"service_control_plane_running",
		"service_coturn_cpu_percent",
		"service_coturn_healthy",
		"service_coturn_memory_bytes",
		"service_coturn_running",
		"service_media_plane_cpu_percent",
		"service_media_plane_healthy",
		"service_media_plane_memory_bytes",
		"service_media_plane_running",
		"service_minio_cpu_percent",
		"service_minio_healthy",
		"service_minio_memory_bytes",
		"service_minio_running",
		"service_nats_cpu_percent",
		"service_nats_healthy",
		"service_nats_memory_bytes",
		"service_nats_running",
		"service_postgres_cpu_percent",
		"service_postgres_healthy",
		"service_postgres_memory_bytes",
		"service_postgres_running",
		"service_redis_cpu_percent",
		"service_redis_healthy",
		"service_redis_memory_bytes",
		"service_redis_running",
		"users_online_current",
		"websocket_connections_current",
	}
	sort.Strings(keys)
	return keys
}
