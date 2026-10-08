//go:build integration

package purge

// Pin-aware purge in the engine (#3458, spec §4). A purge keeps pinned
// messages unless the plan includes them, and the keep is STICKY across
// batches (invariant I1): a row any statement of a DeleteSpec observed as
// pinned is not deleted by that spec, even if it is unpinned before a later
// batch. Skipped when DATABASE_URL is unset (CI sets it).

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func (f engineFixture) insertMessage(t *testing.T, agoSecs int) string {
	t.Helper()
	var id string
	require.NoError(t, f.db.QueryRow(`
		INSERT INTO messages (channel_id, user_id, content, created_at)
		VALUES ($1, $2, 'pin-keep', NOW() - make_interval(secs => $3)) RETURNING id`,
		f.channelID, f.authorID, agoSecs).Scan(&id))
	return id
}

func setPinned(t *testing.T, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id string, pinned bool) {
	t.Helper()
	query := `UPDATE messages SET pinned_at = NULL, pinned_by = NULL WHERE id = $1`
	if pinned {
		query = `UPDATE messages SET pinned_at = NOW(), pinned_by = user_id WHERE id = $1`
	}
	_, err := q.ExecContext(context.Background(), query, id)
	require.NoError(t, err)
}

func (f engineFixture) messageExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, id).Scan(&n))
	return n == 1
}

func (f engineFixture) auditIncludePinned(t *testing.T) bool {
	t.Helper()
	var included bool
	require.NoError(t, f.db.QueryRow(
		`SELECT include_pinned FROM message_purges WHERE context_id = $1`, f.channelID).Scan(&included))
	return included
}

// waitForBlockedVictimSelect waits until a backend is blocked on a row lock
// while running the channel victim SELECT, so a test can act at exactly that
// point.
func waitForBlockedVictimSelect(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		err := db.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE 'SELECT id FROM messages%FOR UPDATE'`).Scan(&n)
		return err == nil && n > 0
	}, 10*time.Second, 20*time.Millisecond, "the victim SELECT never blocked")
}

func runPurgeAsync(t *testing.T, e *Engine, p Plan) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := e.Run(context.Background(), p)
		done <- err
	}()
	return done
}

func TestPurge_IncludePinnedFlagAndAudit(t *testing.T) {
	t.Run("off: pins are kept and the audit reads FALSE", func(t *testing.T) {
		f := seedEngineFixture(t)
		pinned := f.insertMessage(t, 30)
		setPinned(t, f.db, pinned, true)
		plain := f.insertMessage(t, 20)

		_, err := f.newEngine(5000).Run(context.Background(), f.channelPlan())
		require.NoError(t, err)

		require.True(t, f.messageExists(t, pinned), "a pinned message survives a purge that does not include pins")
		require.False(t, f.messageExists(t, plain))
		require.False(t, f.auditIncludePinned(t))
	})

	t.Run("on: pins are deleted and the audit reads TRUE", func(t *testing.T) {
		f := seedEngineFixture(t)
		pinned := f.insertMessage(t, 30)
		setPinned(t, f.db, pinned, true)
		f.insertMessage(t, 20)
		p := f.channelPlan()
		p.IncludePinned = true

		_, err := f.newEngine(5000).Run(context.Background(), p)
		require.NoError(t, err)

		require.Zero(t, f.countMessages(t))
		require.True(t, f.auditIncludePinned(t))
	})
}

// A pin seen by batch 2 keeps the row through batch 3, after the unpin.
func TestPurge_KeepsPinWhoseUnpinLandsBetweenBatches(t *testing.T) {
	f := seedEngineFixture(t)
	m1 := f.insertMessage(t, 40)
	m2 := f.insertMessage(t, 30)
	r := f.insertMessage(t, 20)
	m4 := f.insertMessage(t, 10)
	e := f.newEngine(1)
	batch := 0
	e.afterBatchHook = func() {
		batch++
		switch batch {
		case 1:
			setPinned(t, f.db, r, true)
		case 2:
			setPinned(t, f.db, r, false)
		}
	}

	_, err := e.Run(context.Background(), f.channelPlan())
	require.NoError(t, err)

	require.True(t, f.messageExists(t, r), "unpinning between batches must not expose a row the purge saw pinned")
	for _, id := range []string{m1, m2, m4} {
		require.False(t, f.messageExists(t, id))
	}
}

// The capture BEFORE the victim SELECT keeps a row that was pinned when the
// batch started and unpinned while the SELECT waited on a lock.
func TestPurge_BeforeCaptureKeepsRowUnpinnedWhileSelectBlocked(t *testing.T) {
	f := seedEngineFixture(t)
	r1 := f.insertMessage(t, 40)
	r2 := f.insertMessage(t, 30)
	setPinned(t, f.db, r2, true)
	m3 := f.insertMessage(t, 20)

	side, err := f.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = side.Rollback() })
	_, err = side.Exec(`SELECT id FROM messages WHERE id = $1 FOR UPDATE`, r1)
	require.NoError(t, err)

	done := runPurgeAsync(t, f.newEngine(1), f.channelPlan())
	waitForBlockedVictimSelect(t, f.db)
	setPinned(t, side, r2, false)
	require.NoError(t, side.Commit())
	require.NoError(t, <-done)

	require.True(t, f.messageExists(t, r2), "the before-capture must keep a row pinned when the batch began")
	require.False(t, f.messageExists(t, r1))
	require.False(t, f.messageExists(t, m3))
}

// The capture AFTER the victim SELECT keeps a row whose pin committed while
// the SELECT waited on it (the EPQ recheck skips it; only the after-capture
// records it for later batches).
func TestPurge_AfterCaptureKeepsRowPinnedWhileSelectBlocked(t *testing.T) {
	f := seedEngineFixture(t)
	r := f.insertMessage(t, 40)
	m2 := f.insertMessage(t, 30)

	side, err := f.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = side.Rollback() })
	setPinned(t, side, r, true)

	e := f.newEngine(1)
	batch := 0
	e.afterBatchHook = func() {
		batch++
		if batch == 1 {
			setPinned(t, f.db, r, false)
		}
	}
	done := runPurgeAsync(t, e, f.channelPlan())
	waitForBlockedVictimSelect(t, f.db)
	require.NoError(t, side.Commit())
	require.NoError(t, <-done)

	require.True(t, f.messageExists(t, r), "the after-capture must keep a row whose pin landed during the SELECT")
	require.False(t, f.messageExists(t, m2))
}

// The kept-set parameter is NULL-proof: neither NULL nor an empty array may
// turn the predicate into "select nothing".
func TestSelectBatch_NullAndEmptyKeptSetStillSelect(t *testing.T) {
	f := seedEngineFixture(t)
	plain := f.insertMessage(t, 10)
	for name, kept := range map[string]any{"NULL": nil, "empty": pq.Array([]string{})} {
		t.Run(name, func(t *testing.T) {
			var ids []string
			rows, err := f.db.Query(deleteQueries["messages"].selectBatch,
				f.channelID, nil, nil, 10, pq.Array([]string(nil)), false, kept)
			require.NoError(t, err)
			for rows.Next() {
				var id string
				require.NoError(t, rows.Scan(&id))
				ids = append(ids, id)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.Equal(t, []string{plain}, ids)
		})
	}
}

// Expiry still deletes pinned rows: it is not pin-aware until #3459. The Clear
// reap follows #3458 §18.2: a pin survives every Clear while any participant
// remains, because each of them can still see it
// (TestRunClearReapBatchKeepsPinnedMessagesBelowWatermark); with no participant
// left nobody can, and it goes with the rest.
func TestPurge_SystemPathsStillDeletePins(t *testing.T) {
	t.Run("expiry deletes a pinned expired message", func(t *testing.T) {
		f := seedEngineFixture(t)
		cutoff := time.Now().UTC().Truncate(time.Microsecond)
		var id string
		require.NoError(t, f.db.QueryRow(`
			INSERT INTO messages (channel_id, user_id, content, expires_at, pinned_at, pinned_by)
			VALUES ($1, $2, 'expired-pin', $3, NOW(), $2) RETURNING id`,
			f.channelID, f.authorID, cutoff.Add(-time.Second)).Scan(&id))

		res, err := f.newEngine(5000).RunExpiryBatch(context.Background(), ExpiryPlan{
			ContextType: ContextChannel, ContextID: f.channelID, ServerID: &f.serverID,
			CandidateIDs: []string{id}, ExpiresBefore: cutoff,
		})
		require.NoError(t, err)
		require.Equal(t, 1, res.DeletedCount)
		require.False(t, f.messageExists(t, id))
	})

	t.Run("the clear reap deletes a pinned message when no participant remains", func(t *testing.T) {
		f := seedClearReapConversation(t, true, false, 2)
		id := f.seedMessage(t, f.members[0], f.at(1))
		_, err := f.db.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = user_id WHERE id = $1`, id)
		require.NoError(t, err)
		_, err = f.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1`, f.conversationID)
		require.NoError(t, err)

		f.reap(t, f.newEngine(5000))
		require.False(t, f.messageExists(t, id), "no participant remains to see the pin, so the reap removes it")
	})
}
