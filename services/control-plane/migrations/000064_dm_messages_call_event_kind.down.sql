-- 000064_dm_messages_call_event_kind.down.sql
-- Reverses 000064_dm_messages_call_event_kind.up.sql.
--
-- Drops in reverse-dependency order: index first, then JSONB column, then
-- the TEXT discriminator column. All operations are idempotent (IF EXISTS).

DROP INDEX IF EXISTS idx_dm_messages_conversation_kind;
ALTER TABLE dm_messages DROP COLUMN IF EXISTS call_event_payload;
ALTER TABLE dm_messages DROP COLUMN IF EXISTS kind;
