package database_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration000134_ClampsFutureDMStampsIdempotently covers #3205's data
// repair for the DM rail.
//
// The rail asymmetry is the whole reason this migration exists: a far-future
// voice_participants row self-heals (the observed lease holds it, or #2907's
// reconciler reaps it once the lease ages out), while a far-future
// dm_voice_participants row is refused by every fence and reachable by nothing.
// The test therefore asserts three separate things, not one:
//
//  1. a future-stamped row is clamped to no later than the transaction clock;
//  2. a past-stamped row is left ALONE -- the predicate is `>`, and a migration
//     that rewrote every row would destroy ordering evidence it has no business
//     touching;
//  3. a second application is a no-op, which the self-negating predicate gives
//     for free but which nothing else in the repo would notice breaking.
//
// Runs inside a rolled-back transaction, as the 000133 test does, so it leaves
// no state behind on a shared database.
func TestMigration000134_ClampsFutureDMStampsIdempotently(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	up := migrationReadFile(t, "../../migrations/000134_clamp_future_dm_voice_lifecycle_stamps.up.sql")

	alice := ts.CreateTestUser(t, "migration-134-alice")
	bob := ts.CreateTestUser(t, "migration-134-bob")
	conversation := ts.CreateDMConversation(t, alice.ID, bob.ID)

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback migration 000134 transaction: %v", rollbackErr)
		}
	})

	// A stamp far enough ahead that no plausible clock skew reaches it, and one
	// deliberately in the past that the migration must not touch.
	poisoned := time.Now().AddDate(1, 0, 0).UTC()
	untouched := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO dm_voice_participants (conversation_id, user_id, lifecycle_event_at)
		 VALUES ($1, $2, $3), ($1, $4, $5)`,
		conversation, alice.ID, poisoned, bob.ID, untouched)
	require.NoError(t, err)

	readStamp := func(userID string) time.Time {
		var at time.Time
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT lifecycle_event_at FROM dm_voice_participants
			  WHERE conversation_id = $1 AND user_id = $2`,
			conversation, userID).Scan(&at))
		return at
	}

	require.True(t, readStamp(alice.ID).After(time.Now()),
		"setup: the poisoned row must actually be future-stamped before the repair")

	txNow := txClock(t, tx)
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err)

	firstPass := readStamp(alice.ID)
	assert.False(t, firstPass.After(txNow),
		"the poisoned stamp was not clamped: %s is still ahead of the transaction clock %s",
		firstPass, txNow)
	assert.True(t, readStamp(bob.ID).Equal(untouched),
		"a past-stamped row was rewritten; the predicate must be strictly greater-than")

	// Idempotent: the predicate is self-negating, so a second pass matches
	// nothing and the first pass's value survives byte-for-byte.
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err)
	assert.True(t, readStamp(alice.ID).Equal(firstPass),
		"a second application moved the stamp: %s -> %s", firstPass, readStamp(alice.ID))
	assert.True(t, readStamp(bob.ID).Equal(untouched),
		"a second application disturbed the past-stamped row")
}
