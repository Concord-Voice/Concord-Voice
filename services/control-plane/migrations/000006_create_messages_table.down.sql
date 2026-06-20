-- Drop indexes
DROP INDEX IF EXISTS idx_messages_created_at;
DROP INDEX IF EXISTS idx_messages_user_id;
DROP INDEX IF EXISTS idx_messages_channel_id;

-- Drop messages table
DROP TABLE IF EXISTS messages;
