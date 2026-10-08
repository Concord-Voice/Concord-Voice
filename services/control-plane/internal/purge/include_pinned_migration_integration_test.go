//go:build integration

package purge_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// includePinnedUp is migration 000164 (#3458; renumbered from 000163 when
// main took that number). Renumber here if it moves again at merge time.
const includePinnedUp uint = 164

// TestMessagePurgeIncludePinnedMigrationRoundTrip pins 000164: the column is
// metadata-only (no table rewrite), historical purges read TRUE because every
// purge before it deleted pinned messages, and a down then up restores the
// schema exactly.
func TestMessagePurgeIncludePinnedMigrationRoundTrip(t *testing.T) {
	db, m := newDisposableClearReasonMigrationDB(t)
	require.NoError(t, m.Migrate(priorMigrationVersion(t, includePinnedUp)), "migrate to the pre-000164 schema")
	constraints := messagePurgesConstraints(t, db)

	var historical string
	require.NoError(t, db.QueryRow(`INSERT INTO message_purges (context_type, context_id, reason, status)
		VALUES ('dm', $1, 'manual', 'completed') RETURNING id`, uuid.NewString()).Scan(&historical))
	filenode := messagePurgesFilenode(t, db)

	require.NoError(t, m.Migrate(includePinnedUp), "000164 must apply")
	require.Equal(t, filenode, messagePurgesFilenode(t, db), "a constant-default ADD COLUMN must not rewrite message_purges")
	first := includePinnedColumn(t, db)
	require.Equal(t, "boolean|NO|true", first)
	var included bool
	require.NoError(t, db.QueryRow(`SELECT include_pinned FROM message_purges WHERE id = $1`, historical).Scan(&included))
	require.True(t, included, "every purge before 000164 deleted pinned messages")

	// A purge that kept pins must still say so after a rollback and re-apply:
	// re-adding the column defaults it to TRUE, a false audit record. The
	// evidence is written with the purge, so the down has nothing to copy.
	var kept, deleted string
	require.NoError(t, db.QueryRow(`INSERT INTO message_purges (context_type, context_id, reason, status, include_pinned)
		VALUES ('dm', $1, 'manual', 'completed', FALSE) RETURNING id`, uuid.NewString()).Scan(&kept))
	require.NoError(t, db.QueryRow(`INSERT INTO message_purges (context_type, context_id, reason, status)
		VALUES ('dm', $1, 'manual', 'completed') RETURNING id`, uuid.NewString()).Scan(&deleted))
	require.True(t, hasKeptPinsEvidence(t, db, kept), "a purge that keeps pins is recorded as it is written")
	require.False(t, hasKeptPinsEvidence(t, db, deleted), "a purge that deletes pins is not")

	require.NoError(t, m.Steps(-1), "000164 down must apply")
	require.Equal(t, "", includePinnedColumn(t, db), "the column is gone after down")
	require.Equal(t, constraints, messagePurgesConstraints(t, db), "down leaves pg_constraint as it was")
	require.True(t, hasKeptPinsEvidence(t, db, kept), "the evidence outlives the down")

	require.NoError(t, m.Steps(1), "000164 re-applies")
	require.Equal(t, first, includePinnedColumn(t, db), "up after down restores the same column")
	require.NoError(t, db.QueryRow(`SELECT include_pinned FROM message_purges WHERE id = $1`, kept).Scan(&included))
	require.False(t, included, "a purge that kept pins must not be relabelled by a down and up")
	require.NoError(t, db.QueryRow(`SELECT include_pinned FROM message_purges WHERE id = $1`, historical).Scan(&included))
	require.True(t, included, "a purge that deleted pins stays TRUE")
	require.NoError(t, db.QueryRow(`SELECT include_pinned FROM message_purges WHERE id = $1`, deleted).Scan(&included))
	require.True(t, included, "a purge that deleted pins after 000164 stays TRUE")
}

// A purge in flight when the 000164 down starts must still be in the evidence.
// The trigger writes it in the purge's own transaction and the down waits for
// that to commit, so the down copies nothing and holds its lock for
// milliseconds, not a scan of the whole audit table (#3552 review).
func TestMessagePurgeIncludePinnedDownKeepsConcurrentEvidence(t *testing.T) {
	db, m := newDisposableClearReasonMigrationDB(t)
	require.NoError(t, m.Migrate(includePinnedUp), "000164 must apply")

	writer, err := db.Begin()
	require.NoError(t, err)
	var inFlight string
	require.NoError(t, writer.QueryRow(`INSERT INTO message_purges (context_type, context_id, reason, status, include_pinned)
		VALUES ('dm', $1, 'manual', 'completed', FALSE) RETURNING id`, uuid.NewString()).Scan(&inFlight))

	down := make(chan error, 1)
	go func() { down <- m.Steps(-1) }()
	waitForMessagePurgesLockWaiter(t, db)
	require.NoError(t, writer.Commit())
	require.NoError(t, <-down, "000164 down must apply")

	require.NoError(t, m.Steps(1), "000164 re-applies")
	var included bool
	require.NoError(t, db.QueryRow(`SELECT include_pinned FROM message_purges WHERE id = $1`, inFlight).Scan(&included))
	require.False(t, included, "a kept-pins purge that committed during the down must not be relabelled")
}

// waitForMessagePurgesLockWaiter returns once a session waits for a lock on
// message_purges. The budget counts database round trips, not time.
func waitForMessagePurgesLockWaiter(t *testing.T, db *sql.DB) {
	t.Helper()
	for range 10000 {
		var waiting bool
		require.NoError(t, db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE c.relname = 'message_purges' AND NOT l.granted)`).Scan(&waiting))
		if waiting {
			return
		}
	}
	t.Fatal("no session ever waited for a lock on message_purges")
}

func hasKeptPinsEvidence(t *testing.T, db *sql.DB, purgeID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM message_purges_kept_pins WHERE purge_id = $1)`, purgeID).Scan(&exists))
	return exists
}

func messagePurgesFilenode(t *testing.T, db *sql.DB) string {
	t.Helper()
	var node string
	require.NoError(t, db.QueryRow(`SELECT pg_relation_filenode('message_purges')::text`).Scan(&node))
	return node
}

// includePinnedColumn returns "type|nullable|default", or "" when the column
// does not exist.
func includePinnedColumn(t *testing.T, db *sql.DB) string {
	t.Helper()
	var shape string
	err := db.QueryRow(`
		SELECT data_type || '|' || is_nullable || '|' || COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_name = 'message_purges' AND column_name = 'include_pinned'`).Scan(&shape)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return shape
}

func messagePurgesConstraints(t *testing.T, db *sql.DB) string {
	t.Helper()
	var defs string
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE(string_agg(conname || ' ' || pg_get_constraintdef(oid) || ' validated=' || convalidated, E'\n' ORDER BY conname), '')
		FROM pg_constraint WHERE conrelid = 'message_purges'::regclass`).Scan(&defs))
	return defs
}
