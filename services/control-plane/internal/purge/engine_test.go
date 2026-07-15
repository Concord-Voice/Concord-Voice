package purge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The identifier allow-list is the ONLY thing standing between fmt.Sprintf-built SQL
// and an injected table/column name, so its accept/reject behavior is locked here.

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

func TestTimeCastFor_MatchesColumnTypes(t *testing.T) {
	// The per-table cast must track each table's created_at column type (M2).
	require.Equal(t, "timestamp", timeCastFor("messages"))
	require.Equal(t, "timestamptz", timeCastFor("dm_messages"))
}
