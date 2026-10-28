//go:build integration

package purge

// The expiry audit row's include_pinned (#3459 §9, AC8).
//
// The row is inserted INSIDE the batch transaction with
// include_pinned = E OR any pinned victim, where E is the parent's effective
// include-pinned value (a window is set AND pins expire with it). FALSE is the
// claim that must be earned: it needs E false AND no pinned victim. Every case
// below pairs with a sibling that differs in exactly the input the case names,
// so a writer that records the wrong half goes red in at least one of them.
// Skipped when DATABASE_URL is unset (CI sets it).

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/lib/pq"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// expiryAuditCutoff is the sweep cutoff every case plans with; due rows are
// stamped just before it.
var expiryAuditCutoff = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// expiryAuditScope is one expiry context under test: a channel or a DM
// conversation. Both are driven through the same cases because the parent read
// and the victim select are separate statements per table.
type expiryAuditScope struct {
	name        string
	db          *sql.DB
	contextType ContextType
	contextID   string
	serverID    *string
	// setPolicy sets the parent's window and include-pinned column directly.
	setPolicy func(t *testing.T, window *int, include bool)
	// insertDue inserts a message stamped due at expiryAuditCutoff-1s,
	// optionally pinned, and returns its id.
	insertDue func(t *testing.T, pinned bool) string
}

func (s expiryAuditScope) engine() *Engine {
	log := logger.NewWithWriter(io.Discard)
	return NewEngine(s.db, log, NewReaper(s.db, log, nil), 5000)
}

func (s expiryAuditScope) plan(ids ...string) ExpiryPlan {
	return ExpiryPlan{
		ContextType: s.contextType, ContextID: s.contextID, ServerID: s.serverID,
		CandidateIDs: ids, ExpiresBefore: expiryAuditCutoff,
	}
}

func (s expiryAuditScope) auditCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM message_purges WHERE context_id = $1`, s.contextID).Scan(&n))
	return n
}

func channelExpiryAuditScope(t *testing.T) expiryAuditScope {
	t.Helper()
	f := seedEngineFixture(t)
	return expiryAuditScope{
		name: "channel", db: f.db, contextType: ContextChannel, contextID: f.channelID, serverID: &f.serverID,
		setPolicy: func(t *testing.T, window *int, include bool) {
			t.Helper()
			_, err := f.db.Exec(`UPDATE channels SET expiration_window_seconds = $2, expiration_include_pinned = $3 WHERE id = $1`,
				f.channelID, window, include)
			require.NoError(t, err)
		},
		insertDue: func(t *testing.T, pinned bool) string {
			t.Helper()
			var id string
			require.NoError(t, f.db.QueryRow(`
				INSERT INTO messages (channel_id, user_id, content, expires_at, pinned_at, pinned_by, key_version)
				VALUES ($1, $2, $3, $4, CASE WHEN $5::boolean THEN NOW() END, CASE WHEN $5::boolean THEN $2::uuid END, 1)
				RETURNING id`,
				f.channelID, f.authorID, uuid.NewString(), expiryAuditCutoff.Add(-time.Second), pinned).Scan(&id))
			return id
		},
	}
}

func dmExpiryAuditScope(t *testing.T) expiryAuditScope {
	t.Helper()
	db := sweepTestDB(t)
	author := seedUploader(t, db)
	var conversationID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, is_personal, created_by)
		VALUES (false, false, $1) RETURNING id`, author).Scan(&conversationID))
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, conversationID); err != nil {
			t.Errorf("cleanup expiry audit conversation: %v", err)
		}
	})
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, author)
	require.NoError(t, err)
	return expiryAuditScope{
		name: "dm", db: db, contextType: ContextDM, contextID: conversationID,
		setPolicy: func(t *testing.T, window *int, include bool) {
			t.Helper()
			_, err := db.Exec(`UPDATE dm_conversations SET expiration_window_seconds = $2, expiration_include_pinned = $3 WHERE id = $1`,
				conversationID, window, include)
			require.NoError(t, err)
		},
		insertDue: func(t *testing.T, pinned bool) string {
			t.Helper()
			var id string
			require.NoError(t, db.QueryRow(`
				INSERT INTO dm_messages (conversation_id, user_id, content, type, expires_at, pinned_at, pinned_by, key_version)
				VALUES ($1, $2, $3, 'text', $4, CASE WHEN $5::boolean THEN NOW() END, CASE WHEN $5::boolean THEN $2::uuid END, 1)
				RETURNING id`,
				conversationID, author, uuid.NewString(), expiryAuditCutoff.Add(-time.Second), pinned).Scan(&id))
			return id
		},
	}
}

// expiryAudit reads the audit row a run reports by id.
func expiryAudit(t *testing.T, db *sql.DB, purgeID string) (includePinned bool, status string, deleted int) {
	t.Helper()
	require.NotEmpty(t, purgeID, "the run reported no audit row")
	require.NoError(t, db.QueryRow(
		`SELECT include_pinned, status, deleted_count FROM message_purges WHERE id = $1 AND reason = $2`,
		purgeID, ExpiryReason).Scan(&includePinned, &status, &deleted))
	return includePinned, status, deleted
}

func intPtr(v int) *int { return &v }

func TestExpiryAudit_IncludePinned(t *testing.T) {
	for _, sc := range []struct {
		name     string
		newScope func(*testing.T) expiryAuditScope
	}{{"channel", channelExpiryAuditScope}, {"dm", dmExpiryAuditScope}} {
		newScope := sc.newScope
		t.Run(sc.name, func(t *testing.T) {
			t.Run("a keep context deleting only unpinned rows records FALSE", func(t *testing.T) {
				s := newScope(t)
				s.setPolicy(t, intPtr(3600), false)
				a, b := s.insertDue(t, false), s.insertDue(t, false)

				res, err := s.engine().RunExpiryBatch(context.Background(), s.plan(a, b))
				require.NoError(t, err)
				require.Equal(t, 2, res.DeletedCount)
				include, status, deleted := expiryAudit(t, s.db, res.PurgeID)
				assert.False(t, include, "no pin was deleted and pins are kept")
				assert.Equal(t, "completed", status)
				assert.Equal(t, 2, deleted)
			})

			t.Run("a context with no timer deleting only unpinned rows records FALSE", func(t *testing.T) {
				// A cleared timer leaves the column at its stored value (TRUE by
				// default) and rows stamped before the clear can still be due. E reads
				// the column only while a window is set.
				s := newScope(t)
				s.setPolicy(t, nil, true)
				a := s.insertDue(t, false)

				res, err := s.engine().RunExpiryBatch(context.Background(), s.plan(a))
				require.NoError(t, err)
				require.Equal(t, 1, res.DeletedCount)
				include, _, _ := expiryAudit(t, s.db, res.PurgeID)
				assert.False(t, include)
			})

			t.Run("a pinned victim in a keep context records TRUE (D-C late pin)", func(t *testing.T) {
				// Pinned AFTER it came due: the stamp stays (C-NORES), so the sweep
				// still selects it and the row deletes a pin the context otherwise
				// keeps.
				s := newScope(t)
				s.setPolicy(t, intPtr(3600), false)
				plain, latePin := s.insertDue(t, false), s.insertDue(t, true)

				res, err := s.engine().RunExpiryBatch(context.Background(), s.plan(plain, latePin))
				require.NoError(t, err)
				require.Equal(t, 2, res.DeletedCount)
				include, _, _ := expiryAudit(t, s.db, res.PurgeID)
				assert.True(t, include, "a pin was deleted: FALSE would claim it was kept")
			})

			t.Run("include mode deleting only unpinned rows records TRUE", func(t *testing.T) {
				s := newScope(t)
				s.setPolicy(t, intPtr(3600), true)
				a := s.insertDue(t, false)

				res, err := s.engine().RunExpiryBatch(context.Background(), s.plan(a))
				require.NoError(t, err)
				require.Equal(t, 1, res.DeletedCount)
				include, _, _ := expiryAudit(t, s.db, res.PurgeID)
				assert.True(t, include, "pins in this context were eligible for the sweep")
			})

			t.Run("a zero-victim batch still writes its row", func(t *testing.T) {
				s := newScope(t)
				s.setPolicy(t, intPtr(3600), false)

				res, err := s.engine().RunExpiryBatch(context.Background(), s.plan(uuid.NewString()))
				require.NoError(t, err)
				require.Zero(t, res.DeletedCount)
				include, status, deleted := expiryAudit(t, s.db, res.PurgeID)
				assert.False(t, include)
				assert.Equal(t, "completed", status)
				assert.Zero(t, deleted)
			})

			t.Run("a toggle-on cannot land between the victim select and the insert", func(t *testing.T) {
				// The batch holds the parent FOR SHARE from before the victim select to
				// COMMIT, so a policy write (FOR NO KEY UPDATE) issued at the hook waits
				// for the batch instead of changing E under it. The write here gives up
				// after 200ms; it must time out (55P03), and the audit then records the
				// E the batch actually ran under: FALSE, no pin deleted. Before the
				// parent lock existed this write landed and the row read TRUE.
				s := newScope(t)
				s.setPolicy(t, intPtr(3600), false)
				a := s.insertDue(t, false)
				e := s.engine()
				hookCalls := 0
				var toggleErr error
				e.afterExpirySelectHook = func() {
					hookCalls++
					toggleErr = toggleIncludeWithLockTimeout(t, s)
				}

				res, err := e.RunExpiryBatch(context.Background(), s.plan(a))
				require.NoError(t, err)
				require.Equal(t, 1, hookCalls, "the injection point was never reached")
				require.Equal(t, 1, res.DeletedCount)
				var pqErr *pq.Error
				require.ErrorAs(t, toggleErr, &pqErr, "the policy write must wait on the batch's parent lock")
				assert.Equal(t, pq.ErrorCode("55P03"), pqErr.Code)
				include, _, _ := expiryAudit(t, s.db, res.PurgeID)
				assert.False(t, include, "the toggle never landed, so the batch ran under E=false with no pinned victim")
			})
		})
	}
}

// A failure before COMMIT leaves no audit row and no deletion, reports no
// PurgeID, and does not run the partial-count salvage: there is no row to
// salvage a count onto, and the salvage's read of the absent id logs a warning
// that would read as a lost audit (#3459 §9).
func TestExpiryAudit_FailureBeforeCommitLeavesNoRowAndNoDeletion(t *testing.T) {
	s := channelExpiryAuditScope(t)
	s.setPolicy(t, intPtr(3600), false)
	const marker = "expiry-audit-forced-failure"
	var id string
	require.NoError(t, s.db.QueryRow(`
		INSERT INTO messages (channel_id, user_id, content, expires_at, key_version)
		VALUES ($1, (SELECT owner_id FROM servers WHERE id = $4), $2, $3, 1)
		RETURNING id`,
		s.contextID, marker, expiryAuditCutoff.Add(-time.Second), *s.serverID).Scan(&id))

	// A BEFORE DELETE trigger fails the delete statement, which runs AFTER the
	// audit insert in the same transaction: the row must go with it.
	_, err := s.db.Exec(`
		CREATE OR REPLACE FUNCTION test_reject_expiry_audit_delete() RETURNS trigger AS $$
		BEGIN
			IF OLD.content = 'expiry-audit-forced-failure' THEN RAISE EXCEPTION 'forced expiry delete failure'; END IF;
			RETURN OLD;
		END;
		$$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS test_reject_expiry_audit_delete ON messages;
		CREATE TRIGGER test_reject_expiry_audit_delete
		BEFORE DELETE ON messages
		FOR EACH ROW EXECUTE FUNCTION test_reject_expiry_audit_delete()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, cleanupErr := s.db.Exec(`DROP TRIGGER IF EXISTS test_reject_expiry_audit_delete ON messages; DROP FUNCTION IF EXISTS test_reject_expiry_audit_delete()`); cleanupErr != nil {
			t.Errorf("cleanup expiry audit delete trigger: %v", cleanupErr)
		}
	})

	var logs bytes.Buffer
	log := logger.NewWithWriter(&logs)
	e := NewEngine(s.db, log, NewReaper(s.db, log, nil), 5000)
	res, runErr := e.RunExpiryBatch(context.Background(), s.plan(id))

	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "forced expiry delete failure", "the failure must be the injected delete failure, not an earlier one")
	assert.Empty(t, res.PurgeID, "there is no audit row to name")
	assert.Zero(t, res.DeletedCount)
	assert.Zero(t, s.auditCount(t), "the audit row rolls back with the batch it describes")
	var remaining int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, id).Scan(&remaining))
	assert.Equal(t, 1, remaining, "nothing was deleted")
	assert.NotContains(t, logs.String(), "failed to read durable count",
		"the partial-count salvage must not run before a commit: it has no row to read")
}

// reconcileExpiryCommit settles a COMMIT that reported an error from the known
// audit id. The real rolled-back arm runs end to end in
// TestEngineRunExpiryBatch_CommitFailureRollsBackAndDoesNotEnqueue; a COMMIT the
// server applied but never acknowledged cannot be produced on demand, so the
// other two arms are driven directly.
func TestReconcileExpiryCommit(t *testing.T) {
	commitErr := errors.New("connection reset by peer")

	t.Run("a present row means the batch committed: its durable count is returned", func(t *testing.T) {
		s := channelExpiryAuditScope(t)
		purgeID := uuid.NewString()
		_, err := s.db.Exec(`
			INSERT INTO message_purges (id, context_type, context_id, server_id, range_to, reason, include_pinned, status, deleted_count)
			VALUES ($1, 'channel', $2, $3, $4, $5, FALSE, 'in_progress', 3)`,
			purgeID, s.contextID, *s.serverID, expiryAuditCutoff, ExpiryReason)
		require.NoError(t, err)

		res, err := s.engine().reconcileExpiryCommit(context.Background(), purgeID, commitErr)
		require.ErrorIs(t, err, commitErr)
		assert.Equal(t, purgeID, res.PurgeID)
		assert.Equal(t, 3, res.DeletedCount)
	})

	t.Run("an absent row after a transport failure preserves the error and claims nothing", func(t *testing.T) {
		s := channelExpiryAuditScope(t)
		res, err := s.engine().reconcileExpiryCommit(context.Background(), uuid.NewString(), commitErr)
		require.ErrorIs(t, err, commitErr)
		assert.Equal(t, Result{}, res)
		// A terminal read can time out after earlier absent reads, leaving the outcome unknown.
	})

	t.Run("a COMMIT still landing after the first read is found, not reported absent", func(t *testing.T) {
		// A COMMIT the client gave up on can still be applying on the server: the row
		// becomes visible after the first read. The read-back waits it out within the
		// salvage budget (#3459 Codex review).
		s := channelExpiryAuditScope(t)
		purgeID := uuid.NewString()
		landed := make(chan error, 1)
		go func() {
			time.Sleep(3 * expiryReconcileRetryInterval)
			_, err := s.db.Exec(`
				INSERT INTO message_purges (id, context_type, context_id, server_id, range_to, reason, include_pinned, status, deleted_count)
				VALUES ($1, 'channel', $2, $3, $4, $5, FALSE, 'in_progress', 2)`,
				purgeID, s.contextID, *s.serverID, expiryAuditCutoff, ExpiryReason)
			landed <- err
		}()

		res, err := s.engine().reconcileExpiryCommit(context.Background(), purgeID, commitErr)
		require.NoError(t, <-landed)
		require.ErrorIs(t, err, commitErr)
		assert.Equal(t, purgeID, res.PurgeID)
		assert.Equal(t, 2, res.DeletedCount)
	})

	t.Run("a COMMIT the server refused rolled back: one read, no waiting", func(t *testing.T) {
		s := channelExpiryAuditScope(t)
		refused := &pq.Error{Code: "40001", Message: "could not serialize access"}
		started := time.Now()
		res, err := s.engine().reconcileExpiryCommit(context.Background(), uuid.NewString(), refused)
		require.ErrorIs(t, err, refused)
		assert.NotContains(t, err.Error(), "outcome unknown", "a server refusal proves rollback")
		assert.Equal(t, Result{}, res)
		assert.Less(t, time.Since(started), expiryReconcileRetryInterval,
			"a refusal the server answered is settled by the first read")
	})

	t.Run("a blocked audit read leaves the outcome unknown and claims nothing", func(t *testing.T) {
		s := channelExpiryAuditScope(t)
		purgeID := uuid.NewString()
		ctx := t.Context()
		blocker, err := s.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, blocker.Rollback()) }()
		_, err = blocker.ExecContext(ctx, `LOCK TABLE message_purges IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)
		var rows int
		require.NoError(t, blocker.QueryRowContext(ctx,
			`SELECT count(*) FROM message_purges WHERE id = $1`, purgeID).Scan(&rows))
		require.Zero(t, rows, "the row is absent, but the reconciliation read cannot prove it")

		res, err := s.engine().reconcileExpiryCommit(context.Background(), purgeID, commitErr)
		require.ErrorIs(t, err, commitErr)
		assert.Contains(t, err.Error(), "outcome unknown")
		assert.Equal(t, Result{}, res)
	})

	t.Run("an unreadable row leaves the outcome unknown and claims nothing", func(t *testing.T) {
		closed, err := sql.Open("postgres", testdb.DatabaseURL())
		require.NoError(t, err)
		require.NoError(t, closed.Close())
		log := logger.NewWithWriter(io.Discard)
		e := NewEngine(closed, log, NewReaper(closed, log, nil), 5000)

		res, err := e.reconcileExpiryCommit(context.Background(), uuid.NewString(), commitErr)
		require.ErrorIs(t, err, commitErr)
		assert.Contains(t, err.Error(), "outcome unknown")
		assert.Equal(t, Result{}, res)
	})
}

// toggleIncludeWithLockTimeout tries to turn the scope's include-pinned value on
// from its own connection, giving up after 200ms on a lock wait.
func toggleIncludeWithLockTimeout(t *testing.T, s expiryAuditScope) error {
	t.Helper()
	query := `UPDATE channels SET expiration_include_pinned = TRUE WHERE id = $1`
	if s.contextType != ContextChannel {
		query = `UPDATE dm_conversations SET expiration_include_pinned = TRUE WHERE id = $1`
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout = '200ms'`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, query, s.contextID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// A keep-pins policy write holds its parent FOR NO KEY UPDATE while it parks
// the stamps of pins it keeps. An expiry batch that starts inside that window
// must wait for it, then find the pin parked and delete nothing. Before the
// batch locked its parent it locked the pin first and deleted it, while the
// policy write went on to accept keep-pins (#3459 Codex review). The control is
// the plain case: the same pin with no policy write in flight is deleted.
func TestExpiryBatch_WaitsForAKeepPinsWriteHoldingTheParent(t *testing.T) {
	for _, sc := range []struct {
		name     string
		newScope func(*testing.T) expiryAuditScope
	}{{"channel", channelExpiryAuditScope}, {"dm", dmExpiryAuditScope}} {
		newScope := sc.newScope
		t.Run(sc.name, func(t *testing.T) {
			t.Run("control: no write in flight, the due pin is deleted", func(t *testing.T) {
				s := newScope(t)
				s.setPolicy(t, intPtr(3600), true)
				pin := s.insertDue(t, true)
				res, err := s.engine().RunExpiryBatch(context.Background(), s.plan(pin))
				require.NoError(t, err)
				assert.Equal(t, 1, res.DeletedCount)
			})

			t.Run("a batch started under the write's parent lock deletes nothing", func(t *testing.T) {
				s := newScope(t)
				s.setPolicy(t, intPtr(3600), true)
				pin := s.insertDue(t, true)

				lockParent := `SELECT id FROM channels WHERE id = $1 FOR NO KEY UPDATE`
				keepPins := `UPDATE channels SET expiration_include_pinned = FALSE WHERE id = $1`
				park := `UPDATE messages SET held_expires_at = expires_at, expires_at = NULL WHERE id = $1`
				if s.contextType != ContextChannel {
					lockParent = `SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`
					keepPins = `UPDATE dm_conversations SET expiration_include_pinned = FALSE WHERE id = $1`
					park = `UPDATE dm_messages SET held_expires_at = expires_at, expires_at = NULL WHERE id = $1`
				}
				ctx := context.Background()
				writer, err := s.db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { _ = writer.Rollback() }()
				var locked string
				require.NoError(t, writer.QueryRowContext(ctx, lockParent, s.contextID).Scan(&locked))

				type outcome struct {
					res Result
					err error
				}
				done := make(chan outcome, 1)
				go func() {
					res, err := s.engine().RunExpiryBatch(ctx, s.plan(pin))
					done <- outcome{res, err}
				}()
				select {
				case got := <-done:
					t.Fatalf("the batch finished while the policy write held the parent: deleted %d, err %v", got.res.DeletedCount, got.err)
				case <-time.After(300 * time.Millisecond):
				}

				_, err = writer.ExecContext(ctx, keepPins, s.contextID)
				require.NoError(t, err)
				_, err = writer.ExecContext(ctx, park, pin)
				require.NoError(t, err)
				require.NoError(t, writer.Commit())

				select {
				case got := <-done:
					require.NoError(t, got.err)
					assert.Zero(t, got.res.DeletedCount, "the pin was parked before the batch could lock it")
				case <-time.After(10 * time.Second):
					t.Fatal("the batch never finished after the policy write committed")
				}
				var stillThere bool
				q := `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`
				if s.contextType != ContextChannel {
					q = `SELECT EXISTS (SELECT 1 FROM dm_messages WHERE id = $1)`
				}
				require.NoError(t, s.db.QueryRow(q, pin).Scan(&stillThere))
				assert.True(t, stillThere, "a pin kept by an accepted policy must not be deleted")
			})
		})
	}
}
