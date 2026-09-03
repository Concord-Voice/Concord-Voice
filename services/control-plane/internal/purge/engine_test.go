package purge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
)

// The identifier allow-list keeps each DeleteSpec aligned with its fixed query template.

func TestDeleteBatched_RejectsUnknownQuerySet(t *testing.T) {
	_, err := (&Engine{}).deleteBatched(context.Background(), "purge", Plan{}, DeleteSpec{MessagesTable: "unknown"})
	require.EqualError(t, err, `purge: query set for "unknown" not found`)
}

func TestDeleteMessagesTx_EmptyIDsIsNoOp(t *testing.T) {
	affected, refs, err := (&Engine{}).deleteMessagesTx(context.Background(), nil, deleteQueries["messages"], nil)
	require.NoError(t, err)
	require.Zero(t, affected)
	require.Nil(t, refs)
}

func TestDeleteOne_RejectsInvalidIdentifiersBeforeDatabaseAccess(t *testing.T) {
	err := (&Engine{}).DeleteOne(context.Background(), "message", DeleteSpec{MessagesTable: "unknown"})
	require.EqualError(t, err, `purge: illegal delete-spec identifiers "unknown"/""/""`)
}

func TestEngineEnqueueBlobDeletes_ForwardsToReaper(t *testing.T) {
	reaper := NewReaper(nil, nil, nil)
	engine := &Engine{reaper: reaper}
	ref := media.BlobRef{Key: "attachments/test"}

	engine.EnqueueBlobDeletes([]media.BlobRef{ref})

	select {
	case got := <-reaper.jobs:
		require.Equal(t, ref, got)
	default:
		t.Fatal("blob ref was not forwarded to the reaper")
	}
}

func TestEngineEnqueueBlobDeletes_AllowsMissingEngineOrReaper(_ *testing.T) {
	var nilEngine *Engine
	nilEngine.EnqueueBlobDeletes(nil)
	(&Engine{}).EnqueueBlobDeletes(nil)
}

func TestValidateIdentifiers_RejectsUnknownTable(t *testing.T) {
	err := validateIdentifiers(DeleteSpec{
		MessagesTable:    "users",
		ScopeColumn:      "channel_id",
		AttachmentsTable: "message_attachments",
	})
	require.Error(t, err)
}

func TestValidateIdentifiers_AcceptsMessages(t *testing.T) {
	require.NoError(t, validateIdentifiers(DeleteSpec{
		MessagesTable:    "messages",
		ScopeColumn:      "channel_id",
		AttachmentsTable: "message_attachments",
	}))
	require.NoError(t, validateIdentifiers(DeleteSpec{
		MessagesTable:    "dm_messages",
		ScopeColumn:      "conversation_id",
		AttachmentsTable: "dm_message_attachments",
	}))
}

func TestValidateIdentifiers_RejectsMismatchedScopeColumn(t *testing.T) {
	// Correct table, but a scope column belonging to the OTHER table must be rejected —
	// a coherent-but-wrong combination is still an escape from the allow-list.
	err := validateIdentifiers(DeleteSpec{
		MessagesTable:    "messages",
		ScopeColumn:      "conversation_id",
		AttachmentsTable: "message_attachments",
	})
	require.Error(t, err)
}

func TestValidateIdentifiers_RejectsMismatchedAttachmentsTable(t *testing.T) {
	err := validateIdentifiers(DeleteSpec{
		MessagesTable:    "dm_messages",
		ScopeColumn:      "conversation_id",
		AttachmentsTable: "message_attachments",
	})
	require.Error(t, err)
}

func TestDeleteQueriesUseColumnTypes(t *testing.T) {
	for _, table := range []string{"messages", "dm_messages"} {
		q := deleteQueries[table]
		require.Contains(t, q.selectBatch, "$2::timestamptz")
		require.Contains(t, q.selectBatch, "ORDER BY created_at, id")
		require.Contains(t, q.selectBatch, "FOR UPDATE")
		require.Contains(t, q.selectOne, "FOR UPDATE")
		require.Contains(t, q.selectAttachedMedia, "ORDER BY mf.id")
		require.Contains(t, q.selectAttachedMedia, "FOR UPDATE")
		require.Contains(t, q.deleteParents, "DELETE FROM "+table)
	}
	require.Contains(t, deleteQueries["messages"].selectAttachedMedia, "message_attachments")
	require.Contains(t, deleteQueries["dm_messages"].selectAttachedMedia, "dm_message_attachments")
	require.Contains(t, deleteQueries["messages"].selectBatch, "FROM messages")
	require.Contains(t, deleteQueries["dm_messages"].selectBatch, "FROM dm_messages")
}
