package voice

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// currentDMVoiceMembers is reached by private ingress after participant-set
// admission. A conversation parent lock must be acquired before its child
// membership FOR KEY SHARE, otherwise deletion can race the membership read.
func TestCurrentDMVoiceMembersWaitsForConversationParent(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, member := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, false, $2)`, conversationID, creator)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, member)
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

	memberTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := memberTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback member transaction: %v", rollbackErr)
		}
	}()
	subscriber := &NATSSubscriber{}
	done := make(chan error, 1)
	go func() {
		_, memberErr := subscriber.currentDMVoiceMembers(ctx, memberTx, conversationID, []uuid.UUID{member})
		done <- memberErr
	}()
	dbtest.WaitForRowLockWaiter(t, db, parentTxID)
	require.NoError(t, parentTx.Commit())
	select {
	case memberErr := <-done:
		require.NoError(t, memberErr)
	case <-time.After(15 * time.Second):
		t.Fatal("private ingress membership read did not complete after parent release")
	}
}
