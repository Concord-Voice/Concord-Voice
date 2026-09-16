package database_test

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migration000136MetricLiteral inherits the prefix alternation unchanged: both
// keys this migration admits begin with `presence_` or `websocket_`, and the
// catalog has carried both prefixes since 000086.
//
// That is luck, not design, and the hazard fails SILENTLY. A prefix this regex
// does not anticipate matches nothing, so the constraint set reads as empty and
// an equality assertion passes by comparing two empty lists -- 000091's regex did
// exactly that. Extend the alternation whenever a key with a new prefix is added;
// the `require.Len` below is the backstop that turns that miss into a failure
// instead of a false green.
var migration000136MetricLiteral = regexp.MustCompile(
	`'(?:host|service|http|websocket|channel|dm|ops|media|registered|pending|users|active|presence)_[a-z0-9_]+'`)

// The two keys this migration admits. #3328 separated two failure modes that had
// been riding one client timer; these make the separation countable rather than
// only described.
var migration000136NewKeys = []string{
	"presence_ttl_lapsed_total",
	"websocket_abnormal_closes_total",
}

// TestMigration000136_FilesAndSchemaLock pins the SQL against the Go catalog.
//
// 000136 is now the newest catalog migration, so it OWNS the live-catalog
// assertion that 000135 used to carry; 000135's own list is frozen to its 64
// keys. This is the 000086 -> 000091 -> 000113 -> 000135 handover protocol, one
// step on. When a later migration admits another key, freeze this list and move
// the live pin there -- do not add the new key here.
func TestMigration000136_FilesAndSchemaLock(t *testing.T) {
	up := migration000136SQL(t, "up")
	down := migration000136SQL(t, "down")
	readme := migrationReadFile(t, filepath.Join("..", "..", "migrations", "README.md"))

	catalogKeys := migration000136CatalogKeys()
	for _, key := range migration000136NewKeys {
		require.Contains(t, catalogKeys, key,
			"the Go catalog must carry the key this migration admits, or the two have drifted")
	}

	upSamples := migration000136ConstraintKeys(up, "ops_metric_samples_metric_key_check")
	// Guards the silent-empty failure the regex comment describes: an
	// unanticipated prefix yields nil here, and nil == nil would otherwise make
	// every assertion below pass while pinning nothing at all.
	require.Len(t, upSamples, len(catalogKeys),
		"parsed no usable key set from the up SQL -- check migration000136MetricLiteral covers every prefix")

	assert.Equal(t, catalogKeys, upSamples)
	assert.Equal(t, catalogKeys, migration000136ConstraintKeys(up, "ops_metric_rollups_metric_key_check"))

	// The down constraint is the catalog minus exactly this migration's keys.
	retired := make(map[string]struct{}, len(migration000136NewKeys))
	for _, key := range migration000136NewKeys {
		retired[key] = struct{}{}
	}
	previous := make([]string, 0, len(catalogKeys)-len(migration000136NewKeys))
	for _, key := range catalogKeys {
		if _, ok := retired[key]; !ok {
			previous = append(previous, key)
		}
	}
	require.Len(t, previous, len(catalogKeys)-len(migration000136NewKeys))
	assert.Equal(t, previous, migration000136ConstraintKeys(down, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, previous, migration000136ConstraintKeys(down, "ops_metric_rollups_metric_key_check"))

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

	assert.Contains(t, readme, "| 000136 | presence_liveness_ops_metrics |")

	// The header RANGE moves here with the live pin, because this test is the one
	// that knows it is newest. A row assertion cannot see it: the list grows
	// correctly while the heading above it goes stale, which is how it sat at
	// 000133 while 000134 and 000135 both had rows (#3094).
	//
	// Derived from the files on disk rather than written as a literal, because a
	// literal here would be the same defect one layer up -- it would need editing
	// by whoever adds 000137, which is exactly the step that gets missed.
	assert.Contains(t, readme,
		fmt.Sprintf("## Existing Migrations (000001–%s)", migrationNewestNumber(t)),
		"README header range must name the newest migration on disk")
}

// TestMigration000136_UpDownReUp proves the constraint actually gates the keys in
// both directions against a real database, rather than merely reading as if it does.
func TestMigration000136_UpDownReUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	const nodeID = "cvn_bbbbbbbbbbbbbbbb"

	insert := func(key, ts2 string, value int) error {
		_, err := ts.DB.ExecContext(ctx, `
			INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
			VALUES ($1, $2, $3::timestamptz, $4)
		`, nodeID, key, ts2, value)
		if err != nil {
			return fmt.Errorf("insert ops_metric_samples row at %s: %w", ts2, err)
		}
		return nil
	}

	downSQL := migration000136SQL(t, "down")
	upSQL := migration000136SQL(t, "up")

	// Establish the up state EXPLICITLY rather than trusting what SetupTestServer
	// left behind. TestMigration000091/000113/000135_UpDownReUp each re-apply their
	// own up SQL, rewriting this very constraint back to a NARROWER form with raw
	// SQL -- so schema_migrations still reads 136 while the constraint has silently
	// regressed, and whether this test sees 62, 64 or 66 keys depends on test ORDER
	// within the package. The up migration is a DROP CONSTRAINT followed by an ADD,
	// so re-applying it is idempotent and cheap.
	_, err := ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err, "failed to establish the up state")

	// Self-clean before AND after. ops_metric_samples is not covered by the
	// package's table truncation, and the primary key is (node_id, metric_key, ts)
	// -- all three fixed here -- so a row left behind by an earlier failed run makes
	// every later run fail on a duplicate key rather than on the constraint this
	// test is about. Diagnosing that costs more than preventing it.
	clearRows := func() {
		for _, key := range migration000136NewKeys {
			_, err := ts.DB.ExecContext(ctx,
				`DELETE FROM ops_metric_samples WHERE node_id = $1 AND metric_key = $2`,
				nodeID, key)
			require.NoError(t, err)
		}
	}
	clearRows()
	t.Cleanup(clearRows)

	// BOTH keys, not just the first. One key proves the constraint moved; it does
	// not prove it moved far enough, and a generator that emitted only one of a
	// two-key pair is precisely the drift this file exists to catch.
	for _, key := range migration000136NewKeys {
		require.NoError(t, insert(key, "2026-09-16 12:00:00+00", 1),
			"the applied 66-key constraint must accept %s", key)
	}

	reapplied := false
	t.Cleanup(func() {
		if !reapplied {
			_, err := ts.DB.ExecContext(ctx, upSQL)
			require.NoError(t, err, "failed to restore the schema for subsequent tests")
		}
	})

	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err, "the down migration must not fail on the rows inserted above")

	for _, key := range migration000136NewKeys {
		var remaining int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ops_metric_samples WHERE metric_key = $1`, key).Scan(&remaining))
		assert.Zero(t, remaining, "rollback must clear rows carrying the retired key %s", key)

		require.Error(t, insert(key, "2026-09-16 13:00:00+00", 2),
			"the restored 64-key constraint must reject %s", key)
	}

	_, err = ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err)
	reapplied = true
	for _, key := range migration000136NewKeys {
		require.NoError(t, insert(key, "2026-09-16 14:00:00+00", 3),
			"the reapplied 66-key constraint must accept %s again", key)
	}
}

func migration000136SQL(t *testing.T, direction string) string {
	t.Helper()
	return migrationReadFile(t, filepath.Join("..", "..", "migrations",
		"000136_presence_liveness_ops_metrics."+direction+".sql"))
}

func migration000136ConstraintKeys(contents, constraint string) []string {
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
	matches := migration000136MetricLiteral.FindAllString(body[:end], -1)
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

func migration000136CatalogKeys() []string {
	definitions := opsmetrics.Catalog()
	keys := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		keys = append(keys, string(definition.Key))
	}
	sort.Strings(keys)
	return keys
}
