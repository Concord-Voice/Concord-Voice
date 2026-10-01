package keyrotation

import (
	"context"
	"database/sql"
)

// rowQueryer is satisfied by *sql.DB and *sql.Tx.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Each context's newest issued epoch: the higher of its newest wrapped epoch
// and its newest revocation successor, and 1 before any key exists. A
// rotation records its successor in the revocation ledger in the same
// transaction that claims it, so no honest writer can hold a key above this.
const (
	issuedChannelEpochQuery = `SELECT GREATEST(
		COALESCE((SELECT MAX(key_version) FROM channel_keys WHERE channel_id = $1), 1),
		COALESCE((SELECT MAX(successor_epoch) FROM key_revocations WHERE channel_id = $1), 1))`
	issuedDMEpochQuery = `SELECT GREATEST(
		COALESCE((SELECT MAX(key_version) FROM dm_channel_keys WHERE conversation_id = $1), 1),
		COALESCE((SELECT MAX(successor_epoch) FROM dm_key_revocations WHERE conversation_id = $1), 1))`
)

// IssuedChannelEpoch returns the newest key epoch the channel has issued.
// A ciphertext write labelled above it names an epoch nobody holds: no
// member can decrypt it, and every reader that sees the label fetches a key
// that does not exist (#2822).
func IssuedChannelEpoch(ctx context.Context, q rowQueryer, channelID string) (int, error) {
	var epoch int
	err := q.QueryRowContext(ctx, issuedChannelEpochQuery, channelID).Scan(&epoch)
	return epoch, err
}

// IssuedDMEpoch is IssuedChannelEpoch for a DM conversation.
func IssuedDMEpoch(ctx context.Context, q rowQueryer, conversationID string) (int, error) {
	var epoch int
	err := q.QueryRowContext(ctx, issuedDMEpochQuery, conversationID).Scan(&epoch)
	return epoch, err
}
