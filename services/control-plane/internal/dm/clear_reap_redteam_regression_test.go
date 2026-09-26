//go:build integration

package dm

// Regression tests for the #3462 Phase-4 red-team findings. Each started life
// as a passing exploit PoC; here the assertions are inverted, so every test
// passes only while its defence holds.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

var redteamUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// redteamClear records a Clear through the production writer the Clear handler uses.
func redteamClear(t *testing.T, db *sql.DB, conversationID, userID string, cutoff time.Time) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			t.Errorf("rollback clear: %v", rbErr)
		}
	}()
	require.NoError(t, InsertClearRange(context.Background(), tx, userID, conversationID, cutoff))
	require.NoError(t, tx.Commit())
}

func redteamClearAll(t *testing.T, db *sql.DB, conversationID string, cutoff time.Time) {
	t.Helper()
	for _, userID := range participantIDs(t, db, conversationID) {
		redteamClear(t, db, conversationID, userID, cutoff)
	}
}

func redteamSeedMessages(t *testing.T, db *sql.DB, conversationID string, n int, at time.Time) []string {
	t.Helper()
	var author string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, conversationID).Scan(&author))
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var id string
		require.NoError(t, db.QueryRow(`INSERT INTO dm_messages (conversation_id, user_id, content, created_at)
			VALUES ($1, $2, 'ciphertext', $3) RETURNING id`, conversationID, author, at.Add(time.Duration(i)*time.Millisecond)).Scan(&id))
		ids = append(ids, id)
	}
	return ids
}

func redteamWaitFor(t *testing.T, db *sql.DB, what, query string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		require.NoError(t, db.QueryRow(query).Scan(&n))
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// redteamPool opens a single-connection pool whose session default isolation
// emulates ALTER ROLE/DATABASE ... SET default_transaction_isolation.
func redteamPool(t *testing.T, repeatableReadDefault bool) *sql.DB {
	t.Helper()
	pool, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(0)
	t.Cleanup(func() {
		if closeErr := pool.Close(); closeErr != nil {
			t.Errorf("close pool: %v", closeErr)
		}
	})
	if repeatableReadDefault {
		_, err = pool.Exec(`SET SESSION default_transaction_isolation = 'repeatable read'`)
		require.NoError(t, err)
	}
	return pool
}

// TestClearReapPinsReadCommittedAgainstARepeatableReadDefault is red-team H1:
// the batch must read W after its parent-lock wait. Under a REPEATABLE READ
// default, an unpinned transaction evaluates W in its pre-wait snapshot and
// misses an AddMember (the real addMemberTx) that committed during the wait,
// then deletes history the newcomer can read.
func TestClearReapPinsReadCommittedAgainstARepeatableReadDefault(t *testing.T) {
	for _, tc := range []struct {
		name           string
		repeatableRead bool
	}{
		{"server default", false},
		{"repeatable read session default", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			log := logger.NewWithWriter(io.Discard)
			enginePool := redteamPool(t, tc.repeatableRead)
			engine := purge.NewEngine(enginePool, log, purge.NewReaper(enginePool, log, nil), 5000)

			conv := seedHiddenEmptyConversation(t, db, false, true, 2)
			before := redteamSeedMessages(t, db, conv, 3, time.Now().Add(-time.Hour))
			redteamClearAll(t, db, conv, time.Now())
			newcomer := dbtest.CreateUser(t, db).String()
			h := &Handler{db: db, log: log}

			// Stall the real addMemberTx after it has locked the parent and
			// inserted the newcomer: its next statement reads dm_channel_keys.
			blocker, err := db.Begin()
			require.NoError(t, err)
			_, err = blocker.Exec(`LOCK TABLE dm_channel_keys IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			addDone := make(chan error, 1)
			go func() { _, addErr := h.addMemberTx(conv, newcomer); addDone <- addErr }()
			redteamWaitFor(t, db, "addMemberTx blocked holding the parent lock", `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				WHERE a.datname = current_database() AND NOT l.granted AND l.locktype = 'relation' AND l.relation = 'dm_channel_keys'::regclass`)

			type outcome struct {
				res purge.ClearReapResult
				err error
			}
			reapDone := make(chan outcome, 1)
			go func() {
				res, reapErr := engine.RunClearReapBatch(context.Background(), purge.ClearReapPlan{ConversationID: conv})
				reapDone <- outcome{res, reapErr}
			}()
			redteamWaitFor(t, db, "reap queued on the parent lock", `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				WHERE a.datname = current_database() AND NOT l.granted AND l.locktype IN ('transactionid','tuple')`)

			require.NoError(t, blocker.Rollback()) // AddMember completes and commits the newcomer
			require.NoError(t, <-addDone)
			got := <-reapDone
			require.NoError(t, got.err)

			var surviving int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = ANY($1::uuid[])`, pq.Array(before)).Scan(&surviving))
			assert.Equal(t, purge.ClearReapNotEligible, got.res.Outcome, "a newcomer without a Clear range must block the reap")
			assert.Equal(t, 3, surviving, "the reap must not delete history the newcomer can read")
		})
	}
}

type redteamFailingDeleter struct{}

// DeleteObject fails the way storage.Client.DeleteObject does: its error
// wraps the storage key, which embeds the file ID. A fake that dropped the key
// let the first version of this test pass while production still leaked it.
func (redteamFailingDeleter) DeleteObject(_ context.Context, key string) error {
	return fmt.Errorf("storage: failed to delete object %q: %w", key, errors.New("object store unavailable"))
}

type redteamFailingResolver struct{}

func (redteamFailingResolver) ResolveDeleter(*string) (media.ObjectDeleter, error) {
	return redteamFailingDeleter{}, nil
}

// TestClearReapFailedBlobDeleteLogsNoFileID is red-team H2 (I6, CWE-532): a
// reaped attachment whose object delete fails must not put its storage key,
// which embeds the file ID, into the log.
func TestClearReapFailedBlobDeleteLogsNoFileID(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	logs := &retirementLogBuffer{}
	log := logger.NewWithWriter(logs)
	reaper := purge.NewReaper(db, log, redteamFailingResolver{})
	engine := purge.NewEngine(db, log, reaper, 5000)

	conv := seedHiddenEmptyConversation(t, db, false, true, 2)
	msg := redteamSeedMessages(t, db, conv, 1, time.Now().Add(-time.Hour))[0]
	var author string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, conv).Scan(&author))
	fileID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, key_version,
		conversation_id, mime_type, file_size, storage_key)
		VALUES ($1, $2, 'file', 2, 1, $3, 'application/octet-stream', 1, $4)`, fileID, author, conv, "attachments/"+fileID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, 0)`, msg, fileID)
	require.NoError(t, err)
	redteamClearAll(t, db, conv, time.Now())

	require.NoError(t, NewClearReapSweeper(db, engine, log).RunPreflight(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { reaper.StartWorker(ctx); close(done) }()
	redteamWaitFor(t, db, "worker attempted the delete",
		`SELECT count(*) FROM media_files WHERE reap_attempts > 0 AND id = '`+fileID+`'`)
	cancel()
	<-done

	out := logs.String()
	require.Contains(t, out, "blob delete failed", "the failed delete must still be logged (positive control)")
	assert.NotContains(t, out, fileID, "a reaped attachment's file ID reached the log")
	assert.NotRegexp(t, redteamUUID, out)
}

type redteamErrEngine struct{ err error }

func (e redteamErrEngine) RunClearReapBatch(context.Context, purge.ClearReapPlan) (purge.ClearReapResult, error) {
	return purge.ClearReapResult{}, e.err
}

// TestClearReapPerBatchDeadlineIsCandidateLocal is red-team H3 (spec D4): a
// per-batch deadline that fires mid-statement surfaces from lib/pq as SQLSTATE
// 57014, not context.DeadlineExceeded. It must stay candidate-local so one slow
// batch cannot abort the pre-bind preflight.
func TestClearReapPerBatchDeadlineIsCandidateLocal(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	parent := context.Background()
	batchCtx, cancel := context.WithTimeout(parent, 150*time.Millisecond)
	tx, err := db.BeginTx(batchCtx, nil)
	require.NoError(t, err)
	_, deadlineErr := tx.ExecContext(batchCtx, `SELECT pg_sleep(5)`)
	cancel()
	if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
		t.Logf("rollback after deadline: %v", rbErr)
	}
	require.Error(t, deadlineErr)
	var pqErr *pq.Error
	require.ErrorAs(t, deadlineErr, &pqErr, "the deadline must surface as a pq.Error for this test to mean anything")
	require.Equal(t, "57014", string(pqErr.Code))

	wrapped := errors.Join(errors.New("purge: select clear reap victims"), deadlineErr)
	assert.True(t, isCandidateLocalClearReapError(parent, wrapped), "a per-batch deadline must be candidate-local")

	calls := 0
	s := &ClearReapSweeper{engine: redteamErrEngine{err: wrapped}, log: logger.NewWithWriter(io.Discard)}
	s.discover = func(context.Context, string) ([]string, error) {
		calls++
		if calls == 1 {
			return []string{uuid.NewString()}, nil
		}
		return nil, nil
	}
	assert.NoError(t, s.RunPreflight(parent), "one slow batch must not abort the pre-bind preflight")

	cancelled, stop := context.WithCancel(parent)
	stop()
	assert.False(t, isCandidateLocalClearReapError(cancelled, wrapped), "parent cancellation (shutdown) stays fatal")
}

// TestEngineRunRefusesSystemReasonsEndToEnd is red-team H4, which held: a
// handler-built Plan carrying a system reason writes no evidence and deletes
// nothing.
func TestEngineRunRefusesSystemReasonsEndToEnd(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	log := logger.NewWithWriter(io.Discard)
	engine := purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000)
	conv := seedHiddenEmptyConversation(t, db, false, false, 2)
	redteamSeedMessages(t, db, conv, 2, time.Now().Add(-time.Hour))
	var actor string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, conv).Scan(&actor))
	for _, reason := range []string{purge.ClearReason, purge.ExpiryReason} {
		_, runErr := engine.Run(context.Background(), purge.Plan{
			ContextType: purge.ContextDM, ContextID: conv, ActorID: actor, Reason: reason,
			Deletes: []purge.DeleteSpec{{MessagesTable: "dm_messages", ScopeColumn: "conversation_id", ScopeID: conv, AttachmentsTable: "dm_message_attachments"}},
		})
		require.Error(t, runErr, "reason %q must be refused", reason)
	}
	var forged, remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_purges WHERE context_id = $1 AND reason IN ('clear','expiry')`, conv).Scan(&forged))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, conv).Scan(&remaining))
	assert.Zero(t, forged, "no forged system-reason evidence")
	assert.Equal(t, 2, remaining, "no deletion under a forged reason")
}
