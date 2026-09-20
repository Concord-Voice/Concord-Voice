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

// migration000135MetricLiteral inherits 000113's prefix alternation unchanged:
// both keys this migration admits begin with `media_`, which the catalog has
// carried since 000086.
//
// That is luck, not design, and the hazard is worth restating because it fails
// SILENTLY. A prefix this regex does not anticipate matches nothing, so the
// constraint set reads as empty and an equality assertion passes by comparing
// two empty lists — 000091's regex did exactly that. Extend the alternation
// whenever a key with a new prefix is added; the `require.Len` below is the
// backstop that turns that miss into a failure instead of a false green.
var migration000135MetricLiteral = regexp.MustCompile(
	`'(?:host|service|http|websocket|channel|dm|ops|media|registered|pending|users|active|presence)_[a-z0-9_]+'`)

// The two keys this migration admits. #3094 accepted both consequences in
// writing; these make them countable rather than merely recorded.
var migration000135NewKeys = []string{
	"media_camera_layering_gate_flips_total",
	"media_camera_pressure_demands_total",
}

// TestMigration000135_FilesAndSchemaLock pins the SQL against the 64-key set
// 000135 itself admitted.
//
// FROZEN by 000136, following the 000086 -> 000091 -> 000113 -> 000135 protocol:
// the newest catalog migration owns the LIVE-catalog pin and older ones freeze
// their own list. Left reading the live catalog, this test would fail the moment
// any later migration admitted a key -- asserting that a 2026 SQL file already
// contains something added after it was written.
func TestMigration000135_FilesAndSchemaLock(t *testing.T) {
	up := migration000135SQL(t, "up")
	down := migration000135SQL(t, "down")
	readme := migrationReadFile(t, filepath.Join("..", "..", "migrations", "README.md"))

	catalogKeys := migration000135FrozenKeys()
	for _, key := range migration000135NewKeys {
		require.Contains(t, catalogKeys, key,
			"the frozen list must carry the key this migration admits, or the freeze was taken wrong")
	}

	upSamples := migration000135ConstraintKeys(up, "ops_metric_samples_metric_key_check")
	// Guards the silent-empty failure the regex comment describes: an
	// unanticipated prefix yields nil here, and nil == nil would otherwise make
	// every assertion below pass while pinning nothing at all.
	require.Len(t, upSamples, len(catalogKeys),
		"parsed no usable key set from the up SQL — check migration000135MetricLiteral covers every prefix")

	assert.Equal(t, catalogKeys, upSamples)
	assert.Equal(t, catalogKeys, migration000135ConstraintKeys(up, "ops_metric_rollups_metric_key_check"))

	// The down constraint is the catalog minus exactly this migration's keys.
	retired := make(map[string]struct{}, len(migration000135NewKeys))
	for _, key := range migration000135NewKeys {
		retired[key] = struct{}{}
	}
	previous := make([]string, 0, len(catalogKeys)-len(migration000135NewKeys))
	for _, key := range catalogKeys {
		if _, ok := retired[key]; !ok {
			previous = append(previous, key)
		}
	}
	require.Len(t, previous, len(catalogKeys)-len(migration000135NewKeys))
	assert.Equal(t, previous, migration000135ConstraintKeys(down, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, previous, migration000135ConstraintKeys(down, "ops_metric_rollups_metric_key_check"))

	// Rollback ordering: lock, then delete the retired rows, then restore the
	// narrower constraint. Any other order lets a concurrent insert from a
	// pre-rollback binary fail the ALTER on data this migration thought it removed.
	lock := strings.Index(down, "LOCK TABLE ops_metric_samples, ops_metric_rollups IN ACCESS EXCLUSIVE MODE")
	deleteSamples := strings.Index(down, "FROM ops_metric_samples WHERE metric_key IN")
	deleteRollups := strings.Index(down, "FROM ops_metric_rollups WHERE metric_key IN")
	restore := strings.Index(down, "ADD CONSTRAINT ops_metric_samples_metric_key_check")
	require.NotEqual(t, -1, lock)
	require.NotEqual(t, -1, deleteSamples)
	require.NotEqual(t, -1, deleteRollups)
	require.NotEqual(t, -1, restore)
	assert.Less(t, lock, deleteSamples)
	assert.Less(t, lock, deleteRollups)
	assert.Less(t, deleteSamples, restore)
	assert.Less(t, deleteRollups, restore)

	assert.Contains(t, readme, "| 000135 | camera_layering_ops_metrics |")

}

// TestMigration000135_UpDownReUp proves the constraint actually gates the keys in
// both directions against a real database, rather than merely reading as if it does.
func TestMigration000135_UpDownReUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	const nodeID = "cvn_aaaaaaaaaaaaaaaa"

	insert := func(key, ts2 string, value int) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
			VALUES ($1, $2, $3::timestamptz, $4)
		`, nodeID, key, ts2, value)
		if err != nil {
			return fmt.Errorf("insert ops_metric_samples row at %s: %w", ts2, err)
		}
		return nil
	}

	downSQL := migration000135SQL(t, "down")
	upSQL := migration000135SQL(t, "up")

	// Establish the up state EXPLICITLY rather than trusting what SetupTestServer
	// left behind — the same hazard 000113's test documents, now one migration
	// deeper. TestMigration000091_UpDownReUp re-applies 000091's up SQL and
	// TestMigration000113_UpDownReUp re-applies 000113's, each rewriting this very
	// constraint back to a NARROWER form (52/61 and 61/62 keys) with raw SQL — so
	// schema_migrations still reads 135 while the constraint has silently
	// regressed, and whether this test sees 62 or 64 keys depends on test ORDER
	// within the package. The up migration is a DROP CONSTRAINT followed by an
	// ADD, so re-applying it is idempotent and cheap.
	_, err = tx.ExecContext(ctx, upSQL)
	require.NoError(t, err, "failed to establish the up state")

	// Keep the catalog roundtrip inside one transaction so rollback restores the
	// live schema after the historical constraint is exercised.
	// BOTH keys, not just the first. One key proves the constraint moved; it does
	// not prove it moved far enough, and a generator that emitted only one of a
	// two-key pair is precisely the drift this file exists to catch.
	for _, key := range migration000135NewKeys {
		require.NoError(t, insert(key, "2026-09-11 12:00:00+00", 1),
			"the applied 64-key constraint must accept %s", key)
	}

	_, err = tx.ExecContext(ctx, downSQL)
	require.NoError(t, err, "the down migration must not fail on the rows inserted above")

	for _, key := range migration000135NewKeys {
		var remaining int
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ops_metric_samples WHERE metric_key = $1`, key).Scan(&remaining))
		assert.Zero(t, remaining, "rollback must clear rows carrying the retired key %s", key)

		_, err = tx.ExecContext(ctx, `SAVEPOINT migration000135_rejection`)
		require.NoError(t, err)
		require.Error(t, insert(key, "2026-09-11 13:00:00+00", 2),
			"the restored 62-key constraint must reject %s", key)
		_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT migration000135_rejection`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT migration000135_rejection`)
		require.NoError(t, err)
	}

	_, err = tx.ExecContext(ctx, upSQL)
	require.NoError(t, err)
	for _, key := range migration000135NewKeys {
		require.NoError(t, insert(key, "2026-09-11 14:00:00+00", 3),
			"the reapplied 64-key constraint must accept %s again", key)
	}
}

// migrationNewestNumber returns the highest migration number present on disk,
// as its zero-padded six-digit prefix. Zero padding makes the string compare
// equivalent to a numeric one for every value this repository will reach.
func migrationNewestNumber(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "found no migrations on disk - the glob is wrong, not the tree")
	newest := ""
	for _, path := range paths {
		base := filepath.Base(path)
		if len(base) < 6 {
			continue
		}
		if number := base[:6]; number > newest {
			newest = number
		}
	}
	require.NotEmpty(t, newest, "no migration filename yielded a six-digit prefix")
	return newest
}

func migration000135SQL(t *testing.T, direction string) string {
	t.Helper()
	return migrationReadFile(t, filepath.Join("..", "..", "migrations",
		"000135_camera_layering_ops_metrics."+direction+".sql"))
}

func migration000135ConstraintKeys(contents, constraint string) []string {
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
	matches := migration000135MetricLiteral.FindAllString(body[:end], -1)
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

// migration000135FrozenKeys is the catalog AS OF 000135 -- the exact 64 keys this
// migration's own CHECK admits, sorted.
//
// Deliberately a literal rather than a live catalog read. The live catalog is now
// 66 keys and will keep growing; this SQL file is finished and will not. Deriving
// it would make this test assert that a file written in 2026 already contains
// every key added since, which is false by construction and is exactly the
// failure 000136 would otherwise have caused here.
//
// Nothing may be added to this list. A new catalog key belongs in the newest
// migration's test, which owns the live pin.
func migration000135FrozenKeys() []string {
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
		"media_camera_layering_gate_flips_total",
		"media_camera_pressure_demands_total",
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
