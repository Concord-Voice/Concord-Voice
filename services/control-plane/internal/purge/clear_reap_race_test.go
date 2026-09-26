//go:build integration

package purge

// Deterministic-hook race coverage for the DM clear-reap engine terminal
// (#3462 spec §12.4, §10). Package purge (not purge_test) so these tests can
// set the unexported beforeClearLockHook / afterClearWatermarkHook seams on
// *Engine directly. No sleeps for ordering: every wait is either a channel
// handoff or a bounded pg_stat_activity poll via require.Eventually.
//
// Fixtures (seedClearReapConversation, clearReapFixture.clear/seedMessage/...,
// seedUploader) come from clear_reap_integration_test.go and reaper_sweep_test.go
// in this same package.

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForBlockedOnLock polls pg_locks until some OTHER backend is waiting on
// the conversation's row lock, or fails the test once deadline elapses. This
// is the deterministic replacement for a sleep: it proves the concurrent
// goroutine has actually reached its lock attempt before the caller proceeds.
// The predicate mirrors redteamWaitFor's (clear_reap_redteam_regression_test.go):
// scoped to the current database, and to the lock types a row-level FOR NO KEY
// UPDATE / FOR UPDATE wait actually uses, not the broader wait_event_type='Lock'
// (which also fires for e.g. advisory or relation locks unrelated to this test).
func waitForBlockedOnLock(t *testing.T, db *sql.DB, deadline time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		err := db.QueryRow(`SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE a.datname = current_database() AND NOT l.granted AND l.locktype IN ('transactionid','tuple')`).Scan(&n)
		if err != nil {
			t.Errorf("poll for a blocked backend: %v", err)
			return false
		}
		return n > 0
	}, deadline, 10*time.Millisecond, "expected a concurrent backend to be blocked on a lock")
}

// checkedRollback rolls back tx, tolerating only sql.ErrTxDone (the
// transaction already committed or was already rolled back), and failing the
// test on any other error instead of silently discarding it.
func checkedRollback(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("rollback: %v", err)
	}
}

// isDeadlockError reports whether err is Postgres SQLSTATE 40P01.
func isDeadlockError(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "40P01"
}

// isLockNotAvailableError reports whether err is Postgres SQLSTATE 55P03
// (lock_not_available), the one error this suite's 5s lock_timeout is expected
// to surface under heavy same-row contention. Anything else — a deadlock
// (40P01) most of all — is a real defect, not benign contention.
func isLockNotAvailableError(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "55P03"
}

// An AddMember that commits in the gap BEFORE the conversation lock is taken
// makes the batch see the new (uncleared) participant and refuse (I4). No
// hook fires during the lock/watermark read itself, since the write already
// landed beforehand.
func TestRunClearReapBatchRaceAddMemberBeforeTheLockBlocksTheReap(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	f.seedMessage(t, f.members[0], f.at(1))
	f.clear(t, f.members[0], f.at(9))
	f.clear(t, f.members[1], f.at(9))
	e := f.newEngine(5000)
	newMember := seedUploader(t, f.db)

	e.beforeClearLockHook = func() {
		_, err := f.db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, f.conversationID, newMember)
		require.NoError(t, err)
	}

	res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})

	require.NoError(t, err)
	assert.Equal(t, ClearReapResult{Outcome: ClearReapNotEligible}, res)
	assert.Equal(t, 1, f.countMessages(t), "the newcomer with no Clear range blocks the reap")
}

// An AddMember that starts DURING a held batch must serialize behind the
// conversation's FOR NO KEY UPDATE lock (§10): two concurrent FOR NO KEY
// UPDATE attempts on the same row conflict in Postgres, so the concurrent
// insert's own lock statement blocks until the reap batch commits. This
// replicates addMemberTx's SQL shape (handlers.go:1077-) rather than calling it,
// since package dm imports package purge and a direct call would cycle.
func TestRunClearReapBatchRaceAddMemberDuringAHeldBatchSerializes(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	f.seedMessage(t, f.members[0], f.at(1))
	f.clear(t, f.members[0], f.at(9))
	f.clear(t, f.members[1], f.at(9))
	e := f.newEngine(5000)
	newMember := seedUploader(t, f.db)

	memberAdded := make(chan struct{})
	var addErr error
	e.afterClearWatermarkHook = func(*sql.Tx) {
		go func() {
			defer close(memberAdded)
			tx, err := f.db.BeginTx(context.Background(), nil)
			if err != nil {
				addErr = err
				return
			}
			if _, err := tx.Exec(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, f.conversationID); err != nil {
				addErr = err
				checkedRollback(t, tx)
				return
			}
			if _, err := tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, f.conversationID, newMember); err != nil {
				addErr = err
				checkedRollback(t, tx)
				return
			}
			addErr = tx.Commit()
		}()
		waitForBlockedOnLock(t, f.db, 2*time.Second)
	}

	res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})
	require.NoError(t, err)
	assert.Equal(t, ClearReapReaped, res.Outcome, "the batch that held the lock first must complete")
	assert.Equal(t, 1, res.DeletedCount)

	<-memberAdded
	require.NoError(t, addErr, "the add-member statement must succeed once the reap batch commits")

	e.afterClearWatermarkHook = nil
	again, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})
	require.NoError(t, err)
	assert.Equal(t, ClearReapResult{Outcome: ClearReapNotEligible}, again, "the member added mid-batch must block the next one")
}

// A Send and a Clear started DURING a held batch use the same technique and
// the same conflict (both go through dmblock.LockNoKeyUpdate / storeHiddenRange
// under the conversation's FOR NO KEY UPDATE lock, per §10): both block until
// the reap commits, then both succeed.
func TestRunClearReapBatchRaceSendAndClearDuringAHeldBatchSerialize(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	a, b := f.members[0], f.members[1]
	f.seedMessage(t, a, f.at(1))
	f.clear(t, a, f.at(9))
	f.clear(t, b, f.at(9))
	e := f.newEngine(5000)

	done := make(chan struct{})
	var sendErr, clearErr error
	var newMessageID string
	e.afterClearWatermarkHook = func(*sql.Tx) {
		go func() {
			defer close(done)
			tx, err := f.db.BeginTx(context.Background(), nil)
			if err != nil {
				sendErr = err
				return
			}
			if _, err := tx.Exec(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, f.conversationID); err != nil {
				sendErr = err
				checkedRollback(t, tx)
				return
			}
			if err := tx.QueryRow(`INSERT INTO dm_messages (conversation_id, user_id, content, created_at)
				VALUES ($1, $2, 'race-send', $3) RETURNING id`, f.conversationID, a, f.at(20)).Scan(&newMessageID); err != nil {
				sendErr = err
				checkedRollback(t, tx)
				return
			}
			// A Clear from b, replacing its prior range, in the same held transaction
			// (mirrors storeHiddenRange's delete-then-reinsert under the same lock).
			if _, err := tx.Exec(`DELETE FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2 AND includes_own AND hidden_from = '-infinity'`, b, f.conversationID); err != nil {
				clearErr = err
				checkedRollback(t, tx)
				return
			}
			if _, err := tx.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
				VALUES ($1, $2, '-infinity', $3, true)`, b, f.conversationID, f.at(30)); err != nil {
				clearErr = err
				checkedRollback(t, tx)
				return
			}
			sendErr = tx.Commit()
		}()
		waitForBlockedOnLock(t, f.db, 2*time.Second)
	}

	res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})
	require.NoError(t, err)
	assert.Equal(t, ClearReapReaped, res.Outcome)
	assert.Equal(t, 1, res.DeletedCount)

	<-done
	require.NoError(t, sendErr, "the concurrent send+clear transaction must succeed once the reap commits")
	require.NoError(t, clearErr)
	assert.True(t, f.messageExists(t, newMessageID), "the message sent during the held batch must survive it")
}

// A transaction holding message rows FOR UPDATE in expiry's (expires_at, id)
// order does not stop the reap: SKIP LOCKED means the victim select never
// waits on it (§10, "removing SKIP LOCKED is a falsification mutant"). The
// held rows are excluded from this batch and reaped on a later pass once
// released.
func TestRunClearReapBatchRaceSkipsExpiryHeldRowsInsteadOfWaiting(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	for i := 1; i <= 5; i++ {
		f.seedMessage(t, f.members[0], f.at(i))
	}
	f.clear(t, f.members[0], f.between(9))
	f.clear(t, f.members[1], f.between(9))

	holder, err := f.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	holderRows, err := holder.Query(`SELECT id FROM dm_messages WHERE conversation_id = $1
		ORDER BY expires_at NULLS LAST, id LIMIT 2 FOR UPDATE`, f.conversationID)
	require.NoError(t, err)
	var held []string
	for holderRows.Next() {
		var id string
		require.NoError(t, holderRows.Scan(&id))
		held = append(held, id)
	}
	require.NoError(t, holderRows.Err())
	require.NoError(t, holderRows.Close())
	require.Len(t, held, 2)
	t.Cleanup(func() { checkedRollback(t, holder) })

	e := f.newEngine(5000)
	start := time.Now()
	res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, ClearReapReaped, res.Outcome)
	assert.Equal(t, 3, res.DeletedCount, "the two held rows must be skipped, not waited for")
	assert.Less(t, elapsed, 2*time.Second, "SKIP LOCKED must never wait on a held row")
	for _, id := range held {
		assert.True(t, f.messageExists(t, id), "a held row must survive this batch")
	}

	require.NoError(t, holder.Rollback())
	again, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})
	require.NoError(t, err)
	assert.Equal(t, 2, again.DeletedCount, "the released rows are reaped on the next pass")
	assert.Zero(t, f.countMessages(t))
}

// Fifty concurrent iterations of RunExpiryBatch and RunClearReapBatch over the
// SAME conversation produce zero deadlocks (40P01): the reap's SKIP LOCKED
// select means it is never a party to a wait-for cycle (§10).
func TestRunClearReapBatchRaceConcurrentLoopWithExpiryProducesNoDeadlocks(t *testing.T) {
	f := seedClearReapConversation(t, false, false, 2)
	const total = 200
	future := f.at(100000) // far beyond any candidate created_at, so W never blocks the reap
	f.clear(t, f.members[0], future)
	f.clear(t, f.members[1], future)
	for i := 0; i < total; i++ {
		id := f.seedMessage(t, f.members[i%2], f.at(i))
		if i%2 == 0 {
			_, err := f.db.Exec(`UPDATE dm_messages SET expires_at = $2 WHERE id = $1`, id, f.at(-1))
			require.NoError(t, err)
		}
	}

	reapEngine := f.newEngine(5)
	expiryEngine := f.newEngine(5)
	const iterations = 50
	var wg sync.WaitGroup
	var reapErrs, expiryErrs []error
	var reapDeleted, expiryDeleted int
	var mu sync.Mutex

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			res, err := reapEngine.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: f.conversationID})
			mu.Lock()
			if err != nil {
				reapErrs = append(reapErrs, err)
			} else {
				reapDeleted += res.DeletedCount
			}
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			candidateIDs := expiredCandidateIDs(t, f.db, f.conversationID)
			if len(candidateIDs) == 0 {
				continue
			}
			res, err := expiryEngine.RunExpiryBatch(context.Background(), ExpiryPlan{
				ContextType:   ContextDM,
				ContextID:     f.conversationID,
				CandidateIDs:  candidateIDs,
				ExpiresBefore: time.Now(),
			})
			mu.Lock()
			if err != nil {
				expiryErrs = append(expiryErrs, err)
			} else {
				expiryDeleted += res.DeletedCount
			}
			mu.Unlock()
		}
	}()
	wg.Wait()

	// The allow-list is exactly one SQLSTATE: 55P03 (lock_not_available), which
	// the 5s lock_timeout is expected to surface under this much concurrency on
	// one row. Anything else — 40P01 most of all — is a real defect and must
	// still fail the test, not merely skip the deadlock-specific assertion.
	for _, err := range reapErrs {
		assert.False(t, isDeadlockError(err), "clear reap must never deadlock: %v", err)
		assert.True(t, isLockNotAvailableError(err), "clear reap must fail only with lock_not_available (55P03), got: %v", err)
	}
	for _, err := range expiryErrs {
		assert.False(t, isDeadlockError(err), "expiry must never deadlock against the reap: %v", err)
		assert.True(t, isLockNotAvailableError(err), "expiry must fail only with lock_not_available (55P03), got: %v", err)
	}

	// Positive control: the loop above only proves errors stayed within the
	// allow-list, which would also hold if every call failed and nothing was
	// ever deleted. Assert real forward progress happened.
	var remaining int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, f.conversationID).Scan(&remaining))
	assert.True(t, remaining == 0 || reapDeleted+expiryDeleted > 0,
		"the concurrent loop must actually delete rows, not merely avoid deadlocking: remaining=%d reapDeleted=%d expiryDeleted=%d",
		remaining, reapDeleted, expiryDeleted)
}

// expiredCandidateIDs discovers the reap-plan-free equivalent of what a real
// expiry sweep would find: message ids past their expiry, rechecked-under-lock
// by RunExpiryBatch itself.
func expiredCandidateIDs(t *testing.T, db *sql.DB, conversationID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM dm_messages WHERE conversation_id = $1 AND expires_at IS NOT NULL AND expires_at < NOW() LIMIT 50`, conversationID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}
