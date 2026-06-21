-- Composite index for efficient message pagination queries (ORDER BY created_at DESC).
CREATE INDEX IF NOT EXISTS idx_messages_channel_created ON messages(channel_id, created_at DESC);
