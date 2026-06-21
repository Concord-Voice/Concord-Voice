-- Message Pinning: nullable metadata columns for pin state.
-- pinned_at is NULL for unpinned messages; non-NULL = pinned.
-- pinned_by records who pinned it (FK to users, SET NULL on user deletion).
ALTER TABLE messages ADD COLUMN pinned_at TIMESTAMPTZ;
ALTER TABLE messages ADD COLUMN pinned_by UUID REFERENCES users(id) ON DELETE SET NULL;

-- Partial composite index for listing pinned messages in a channel.
-- For large production tables, run the index creation CONCURRENTLY outside a transaction.
CREATE INDEX idx_messages_pinned ON messages(channel_id, pinned_at DESC) WHERE pinned_at IS NOT NULL;
