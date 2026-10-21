package database_test

import (
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000181 (epic #2033 PR-1, Refs #1305): stripe_webhook_events, the
// webhook dedupe table. Its down is a plain, unguarded drop (spec section 5):
// Reconcile is idempotent, so the table only saves repeat work.

func migration000181UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000181_stripe_webhook_events.up.sql")
}

func migration000181DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000181_stripe_webhook_events.down.sql")
}

func migration000181InsertEvent(q migrationExecer, eventID, eventType string) error {
	_, err := q.Exec(
		`INSERT INTO stripe_webhook_events (event_id, event_type) VALUES ($1, $2)`,
		eventID, eventType)
	return err
}

func TestMigration000181_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	applied := migrationSchemaSnapshot(t, tx, "stripe_webhook_events")
	for _, line := range []string{
		"column event_id text - NO -",
		"column event_type text - NO -",
		"column processed_at timestamp with time zone - NO now()",
		"constraint stripe_webhook_events_pkey PRIMARY KEY (event_id)",
	} {
		assert.Contains(t, applied, line)
	}
	for _, prefix := range []string{
		"constraint stripe_webhook_events_event_id_check ",
		"constraint stripe_webhook_events_event_type_check ",
		"index idx_stripe_webhook_events_processed_at ",
	} {
		assert.NotEmpty(t, migrationSnapshotLine(applied, prefix), "missing after up: %s", prefix)
	}
	for _, line := range applied {
		assert.NotContains(t, line, "payload", "the dedupe table must never store an event payload")
	}

	_, err = tx.Exec(migration000181DownSQL(t))
	require.NoError(t, err)
	assert.Empty(t, migrationSchemaSnapshot(t, tx, "stripe_webhook_events"), "the down must remove the table and its index")

	_, err = tx.Exec(migration000181UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "stripe_webhook_events"))
}

// The down is deliberately unguarded: it drops the table even while it holds
// rows. This pins that decision so a later guard is a reviewed change.
func TestMigration000181_DownDropsRowsUnguarded(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)
	require.NoError(t, migration000181InsertEvent(tx, "evt_"+uuid.NewString(), "invoice.paid"))

	_, err = tx.Exec(migration000181DownSQL(t))
	require.NoError(t, err, "the dedupe table's down must not refuse")
	assert.Empty(t, migrationSchemaSnapshot(t, tx, "stripe_webhook_events"))
}

func TestMigration000181_Constraints(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	t.Run("accepts an evt_ id with a type of up to 128 characters", func(t *testing.T) {
		require.NoError(t, migration000181InsertEvent(db, "evt_"+uuid.NewString(), "customer.subscription.updated"))
		require.NoError(t, migration000181InsertEvent(db, "evt_"+uuid.NewString(), strings.Repeat("a", 128)))

		var stamped bool
		require.NoError(t, db.QueryRow(
			`SELECT bool_and(processed_at IS NOT NULL) FROM stripe_webhook_events`).Scan(&stamped))
		assert.True(t, stamped, "processed_at must default to the insert time")
	})

	t.Run("an event id is recorded once", func(t *testing.T) {
		eventID := "evt_" + uuid.NewString()
		require.NoError(t, migration000181InsertEvent(db, eventID, "invoice.paid"))
		migrationRequirePQError(t, migration000181InsertEvent(db, eventID, "invoice.paid"),
			"23505", "stripe_webhook_events_pkey")
	})

	for _, tc := range []struct{ name, eventID, eventType, constraint string }{
		{"an id from another Stripe object", "cus_" + uuid.NewString(), "invoice.paid", "stripe_webhook_events_event_id_check"},
		// The escaped underscore: an unescaped 'evt_%' would accept this.
		{"an id whose fourth character is not an underscore", "evtX" + uuid.NewString(), "invoice.paid", "stripe_webhook_events_event_id_check"},
		{"an empty id", "", "invoice.paid", "stripe_webhook_events_event_id_check"},
		{"a 129-character type", "evt_" + uuid.NewString(), strings.Repeat("a", 129), "stripe_webhook_events_event_type_check"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			migrationRequirePQError(t, migration000181InsertEvent(db, tc.eventID, tc.eventType), "23514", tc.constraint)
		})
	}

	t.Run("rejects a missing type", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO stripe_webhook_events (event_id) VALUES ($1)`, "evt_"+uuid.NewString())
		migrationRequirePQError(t, err, "23502", "")
	})
}
