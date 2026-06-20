-- DM Message Pinning: mirror server-message pin columns on dm_messages.
-- pinned_at is NULL for unpinned messages; non-NULL = pinned.
-- pinned_by records the participant who pinned it (FK to users, SET NULL on user deletion).
ALTER TABLE dm_messages ADD COLUMN pinned_at TIMESTAMPTZ;
ALTER TABLE dm_messages ADD COLUMN pinned_by UUID REFERENCES users(id) ON DELETE SET NULL;

-- Partial composite index for listing pinned messages in a conversation.
-- NOTE: golang-migrate runs migrations inside a transaction, so CREATE INDEX CONCURRENTLY
-- cannot be used here (it is not allowed inside a transaction block). For large production
-- tables this index should be created manually with CONCURRENTLY before the migration runs,
-- or the migration should be run during a low-traffic window.
CREATE INDEX idx_dm_messages_pinned
    ON dm_messages(conversation_id, pinned_at DESC)
    WHERE pinned_at IS NOT NULL;
