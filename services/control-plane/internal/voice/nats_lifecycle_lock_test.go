package voice

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockServerVoiceLifecycleTx_SerializesSameSender(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	senderID := uuid.New()
	first, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := first.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback first transaction: %v", rollbackErr)
		}
	})
	require.NoError(t, LockServerVoiceLifecycleTx(ctx, first, senderID))
	lockKey, err := voiceLifecycleAdvisoryKey(presence.CategoryServerVoice, senderID)
	require.NoError(t, err)

	secondDone := make(chan error, 1)
	go func() {
		second, beginErr := db.BeginTx(ctx, nil)
		if beginErr != nil {
			secondDone <- beginErr
			return
		}
		defer func() {
			if rollbackErr := second.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback second transaction: %v", rollbackErr)
			}
		}()
		if lockErr := LockServerVoiceLifecycleTx(ctx, second, senderID); lockErr != nil {
			secondDone <- lockErr
			return
		}
		secondDone <- second.Commit()
	}()

	dbtest.WaitForAdvisoryLockWaiter(t, db, lockKey)
	select {
	case err := <-secondDone:
		t.Fatalf("second transaction acquired lifecycle lock before first released it: %v", err)
	default:
	}
	require.NoError(t, first.Commit())

	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("second transaction did not acquire lifecycle lock after first committed")
	}
}

func TestLockServerVoiceLifecycleTx_NilTransaction(t *testing.T) {
	assert.Error(t, LockServerVoiceLifecycleTx(context.Background(), nil, uuid.New()))
}
