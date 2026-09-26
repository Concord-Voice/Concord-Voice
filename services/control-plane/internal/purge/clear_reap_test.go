package purge

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmvisibility"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The engine is built with a nil *sql.DB, so any plan that reached BeginTx would
// panic: a returned errClearReapPlan proves validation ran before database contact.
func TestRunClearReapBatchRejectsBadPlansBeforeTouchingTheDatabase(t *testing.T) {
	e := NewEngine(nil, logger.NewWithWriter(io.Discard), nil, 0)
	for _, id := range []string{"", "not-a-uuid", uuid.Nil.String()} {
		res, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: id})
		require.ErrorIs(t, err, errClearReapPlan, "conversation id %q", id)
		assert.Equal(t, ClearReapResult{}, res)
	}
}

// The plan-validation error is static text: it must never echo the caller's
// input back, because a caller-supplied conversation id is an ID (I6).
func TestRunClearReapBatchPlanErrorCarriesNoInput(t *testing.T) {
	e := NewEngine(nil, logger.NewWithWriter(io.Discard), nil, 0)
	bad := "not-a-uuid-" + uuid.NewString()
	_, err := e.RunClearReapBatch(context.Background(), ClearReapPlan{ConversationID: bad})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), bad)
}

// Structural locks on the fixed SQL, cheap enough to hold without a database.
// The behavioural locks live in clear_reap_integration_test.go.
func TestClearReapQueriesShape(t *testing.T) {
	assert.Contains(t, clearReapLockConversation, "FROM dm_conversations")
	assert.Contains(t, clearReapLockConversation, "FOR NO KEY UPDATE")

	// W is derived from the single shared definition, never a local copy.
	assert.Contains(t, clearReapWatermark, dmvisibility.ClearWatermarkLateral)

	for name, q := range map[string]string{"below": clearReapSelectBelow, "all": clearReapSelectAll} {
		// dm_messages only: no channel variant exists, so a channel reap is
		// structurally impossible.
		assert.Contains(t, q, "FROM dm_messages", name)
		assert.NotContains(t, q, "FROM messages", name)
		assert.Contains(t, q, "conversation_id = $1", name)
		assert.Contains(t, q, "ORDER BY created_at, id", name)
		// SKIP LOCKED is what keeps the reap out of expiry's lock order (§10).
		assert.Contains(t, q, "FOR UPDATE SKIP LOCKED", name)
		// I10: pinned rows below W are reaped like any other row.
		assert.NotContains(t, strings.ToLower(q), "pinned", name)
	}
	assert.Contains(t, clearReapSelectBelow, "created_at < $2::timestamptz")

	// The audit shape is fixed: no actor, target, server or lower bound, and the
	// row is written completed in the batch transaction (D1).
	assert.Contains(t, clearReapAudit, "VALUES (NULL, $1, $2, NULL, NULL, NULL, $3, $4, 'completed', $5, NOW())")
	assert.Equal(t, "clear", ClearReason)
}
