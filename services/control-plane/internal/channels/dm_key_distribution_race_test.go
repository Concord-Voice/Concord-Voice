package channels

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMKeyDistributionDoesNotMintAfterMemberRemoval(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	actor, target := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, true, $2)`, conversationID, actor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversationID, actor, target)
	require.NoError(t, err)

	removalCtx, cancelRemoval := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRemoval()
	removalTx, err := db.BeginTx(removalCtx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := removalTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback removal transaction: %v", rollbackErr)
		}
	}()
	require.NoError(t, removalTx.QueryRowContext(removalCtx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, actor).Scan(&actor))
	var removalTxID int64
	require.NoError(t, removalTx.QueryRowContext(removalCtx, `SELECT txid_current()`).Scan(&removalTxID))

	h := &Handler{db: db, log: logger.New("test")}
	type distributionResult struct {
		count int
		err   error
	}
	successorVersion := 2
	distributionDone := make(chan distributionResult, 1)
	go func() {
		distributed, distributionErr := h.distributeDMKeys(
			removalCtx, actor.String(), "", conversationID.String(),
			map[string]string{target.String(): "attacker-wrap"},
			map[string]int{target.String(): 2}, &successorVersion,
		)
		distributionDone <- distributionResult{count: distributed, err: distributionErr}
	}()

	dbtest.WaitForRowLockWaiter(t, db, removalTxID)
	require.NoError(t, dm.LockDMVoiceParticipantSetTx(removalCtx, removalTx, conversationID))
	_, err = removalTx.ExecContext(removalCtx,
		`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, actor)
	require.NoError(t, err)
	_, err = removalTx.ExecContext(removalCtx, `
		INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by)
		VALUES ($1, 1, 2, 'member_removed', $2)`, conversationID, actor)
	require.NoError(t, err)
	require.NoError(t, removalTx.Commit())

	select {
	case result := <-distributionDone:
		assert.Zero(t, result.count, "removed actor must not distribute any key")
		require.Error(t, result.err)
		assert.True(t, errors.Is(result.err, errDMKeyDistributorNotParticipant), "removed actor must fail closed")
	case <-time.After(3 * time.Second):
		t.Fatal("DM key distribution did not finish after member removal committed")
	}
	assert.Zero(t, countDMKeysAtVersion(t, db, conversationID, 2), "removed actor must not mint an epoch-2 wrap")
}

func TestDMKeyDistributionSkipsRecipientRemovedWhileDistributionWaits(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	actor, recipient := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, false, $2)`, conversationID, actor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversationID, actor, recipient)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	removalTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := removalTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback removal transaction: %v", rollbackErr)
		}
	}()
	var locked uuid.UUID
	require.NoError(t, removalTx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR UPDATE`, recipient).Scan(&locked))
	var removalTxID int64
	require.NoError(t, removalTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&removalTxID))

	h := &Handler{db: db, log: logger.New("test")}
	done := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, distErr := h.distributeDMKeys(ctx, actor.String(), "", conversationID.String(),
			map[string]string{
				actor.String():     "current-actor-wrap",
				recipient.String(): "removed-recipient-wrap",
			}, nil, nil)
		done <- struct {
			count int
			err   error
		}{count, distErr}
	}()
	dbtest.WaitForRowLockWaiter(t, db, removalTxID)
	require.NoError(t, dm.LockDMVoiceParticipantSetTx(ctx, removalTx, conversationID))
	_, err = removalTx.ExecContext(ctx,
		`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, recipient)
	require.NoError(t, err)
	require.NoError(t, removalTx.Commit())

	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Equal(t, 1, result.count, "valid recipients must continue while removed recipients are skipped")
	case <-time.After(15 * time.Second):
		t.Fatal("distribution did not complete after recipient removal committed")
	}
	assert.Equal(t, 1, countDMKeysAtVersion(t, db, conversationID, 1))
	var removedRecipientKeys int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM dm_channel_keys
		WHERE conversation_id = $1 AND user_id = $2 AND key_version = 1
	`, conversationID, recipient).Scan(&removedRecipientKeys))
	assert.Zero(t, removedRecipientKeys, "distribution must not insert a key row for a removed recipient")
}

func TestDMKeyDistributionNotParticipantResponseIsIndistinguishable(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	(&Handler{}).respondKeyDistributionError(c, errDMKeyDistributorNotParticipant, uuid.NewString())

	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.JSONEq(t, `{"error":"Context not found or access denied"}`, recorder.Body.String())
}

func TestDMKeyDistributionAbsentActorFailsClosed(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	recipient := dbtest.CreateUser(t, db)
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, false, $2)`, conversationID, recipient)
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("test")}
	distributed, distErr := h.distributeDMKeys(context.Background(), uuid.NewString(), "", conversationID.String(),
		map[string]string{recipient.String(): "wrapped"}, nil, nil)
	require.ErrorIs(t, distErr, errDMKeyDistributorNotParticipant)
	assert.Zero(t, distributed)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h.respondKeyDistributionError(c, distErr, conversationID.String())
	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.JSONEq(t, `{"error":"Context not found or access denied"}`, recorder.Body.String())
}

func TestDMKeyDistributionWaitsForRecipientBeforeParticipantSet(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	actor, recipient := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, false, $2)`, conversationID, actor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversationID, actor, recipient)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	recipientTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := recipientTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback recipient transaction: %v", rollbackErr)
		}
	}()
	var locked uuid.UUID
	require.NoError(t, recipientTx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR UPDATE`, recipient).Scan(&locked))
	var recipientTxID int64
	require.NoError(t, recipientTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&recipientTxID))
	lockKey, err := dm.VoiceParticipantSetAdvisoryKey(conversationID)
	require.NoError(t, err)

	h := &Handler{db: db, log: logger.New("test")}
	done := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, distErr := h.distributeDMKeys(ctx, actor.String(), "", conversationID.String(),
			map[string]string{recipient.String(): "wrapped"}, nil, nil)
		done <- struct {
			count int
			err   error
		}{count, distErr}
	}()
	dbtest.WaitForRowLockWaiter(t, db, recipientTxID)
	require.False(t, grantedDistributionAdvisoryLock(t, db, lockKey),
		"distribution blocked on recipient FK before acquiring participant-set advisory")
	require.NoError(t, recipientTx.Commit())
	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Equal(t, 1, result.count)
	case <-time.After(15 * time.Second):
		t.Fatal("distribution did not complete after recipient lock released")
	}
}

func grantedDistributionAdvisoryLock(t *testing.T, db *sql.DB, key int64) bool {
	t.Helper()
	classID, objID := dbtest.AdvisoryKeyHalves(key)
	var granted bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_locks l
			WHERE l.locktype = 'advisory' AND l.granted
			  AND l.objsubid = 1 AND l.classid::bigint = $1 AND l.objid::bigint = $2
		)`, classID, objID).Scan(&granted))
	return granted
}

func TestDMKeyDistributionDoesNotDeadlockWithConversationCascade(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	actor, recipient := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, false, $2)`, conversationID, actor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversationID, actor, recipient)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'old', 1)`, conversationID, actor)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	parentTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := parentTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback parent transaction: %v", rollbackErr)
		}
	}()
	var locked uuid.UUID
	require.NoError(t, parentTx.QueryRowContext(ctx,
		`SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`, conversationID).Scan(&locked))
	var parentTxID int64
	require.NoError(t, parentTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&parentTxID))

	h := &Handler{db: db, log: logger.New("test")}
	distDone := make(chan error, 1)
	go func() {
		_, distErr := h.distributeDMKeys(ctx, actor.String(), "", conversationID.String(),
			map[string]string{recipient.String(): "successor"}, nil, intPtr(2))
		distDone <- distErr
	}()
	dbtest.WaitForRowLockWaiter(t, db, parentTxID)

	_, err = parentTx.ExecContext(ctx, `DELETE FROM dm_conversations WHERE id = $1`, conversationID)
	require.NoError(t, err, "conversation cascade must not deadlock with distribution")
	require.NoError(t, parentTx.Commit())
	select {
	case distErr := <-distDone:
		require.ErrorIs(t, distErr, errDMKeyDistributorNotParticipant)
	case <-time.After(15 * time.Second):
		t.Fatal("distribution did not finish after conversation cascade")
	}
	assert.Zero(t, countDMKeysAtVersion(t, db, conversationID, 2),
		"distribution must not insert a successor key after conversation deletion")
}

func intPtr(value int) *int { return &value }

func countDMKeysAtVersion(t *testing.T, db *sql.DB, conversationID uuid.UUID, version int) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1 AND key_version = $2`,
		conversationID, version,
	).Scan(&count))
	return count
}
