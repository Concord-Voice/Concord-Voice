package stepup

// Regression for review of #3509 (Codex P2): SweepExpiredTokens deleted every
// expired token in ONE statement, so a sweep that ran out of its context —
// which the hourly cleanup job shares across all of its tasks — rolled back
// everything it had done and made no progress at all. It must delete in
// bounded batches, committing each, so a sweep cut short keeps what it
// drained.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSweepExpiredTokens_CutShort_KeepsWhatItDrained: 2,500 of one user's
// tokens expired an hour ago and one, the stall, a second ago, and deleting
// the stall — the only row with its purpose — sleeps far past the sweep's
// deadline. The sweep must stop at its
// deadline having committed every batch before the stall's — so at most one
// batch, the one the deadline cut short, rolls back — report what it
// drained, and leave the stall.
func TestSweepExpiredTokens_CutShort_KeepsWhatItDrained(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	const old = 2500
	_, err := db.Exec(`
		INSERT INTO step_up_tokens (token_hash, user_id, factor, purpose, credential_epoch, expires_at, created_at)
		SELECT sha256(convert_to($1::text || g::text, 'UTF8')), $1::uuid, 'password', 'message.delete', NULL,
		       now() - interval '1 hour', now() - interval '61 minutes'
		FROM generate_series(1, $2) g`, userID, old)
	require.NoError(t, err)
	stall := hashToken("stall-" + userID)
	_, err = db.Exec(`
		INSERT INTO step_up_tokens (token_hash, user_id, factor, purpose, credential_epoch, expires_at, created_at)
		VALUES ($1, $2, 'password', 'test.sweep_stall', NULL, now() - interval '1 second', now() - interval '61 seconds')`,
		stall, userID)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE OR REPLACE FUNCTION test_sweep_stall() RETURNS trigger AS $$
		BEGIN PERFORM pg_sleep(30); RETURN OLD; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER test_sweep_stall BEFORE DELETE ON step_up_tokens
		FOR EACH ROW WHEN (OLD.purpose = 'test.sweep_stall') EXECUTE FUNCTION test_sweep_stall()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS test_sweep_stall ON step_up_tokens; DROP FUNCTION IF EXISTS test_sweep_stall()`)
		_, _ = db.Exec(`DELETE FROM step_up_tokens WHERE user_id = $1`, userID)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n, err := SweepExpiredTokens(ctx, db)

	assert.Error(t, err, "a sweep cut short by its context says so")
	remaining := tokenRows(t, db, userID)
	assert.Less(t, remaining, old+1, "the batches before the stall's were drained and committed")
	assert.LessOrEqual(t, remaining, sweepBatchSize, "only the batch the deadline cut short rolled back")
	assert.GreaterOrEqual(t, n, int64(old+1-remaining), "and the sweep reports what it drained")
	var stalled int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM step_up_tokens WHERE token_hash = $1`, stall).Scan(&stalled))
	assert.Equal(t, 1, stalled, "the batch the deadline cut short rolled back")
}
