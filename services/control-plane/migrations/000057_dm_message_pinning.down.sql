DROP INDEX IF EXISTS idx_dm_messages_pinned;
ALTER TABLE dm_messages DROP COLUMN IF EXISTS pinned_by;
ALTER TABLE dm_messages DROP COLUMN IF EXISTS pinned_at;
