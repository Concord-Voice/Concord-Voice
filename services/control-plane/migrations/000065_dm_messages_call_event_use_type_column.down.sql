-- 000065_dm_messages_call_event_use_type_column.down.sql
-- Reverses the call-event-uses-type-column correction. Restores migration 000064's
-- shape: re-creates the kind column + the kind-based partial index. Also
-- backfills kind for any call_event rows written under 000065 so the
-- kind-based partial index remains useful after rollback (Copilot/migration
-- reviewer #1231 feedback).

DROP INDEX IF EXISTS idx_dm_messages_conversation_type_callevent;

ALTER TABLE dm_messages
  ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'text';

-- Backfill kind from type for rows written while 000065 was applied. Without
-- this, all rows have kind='text' (the DEFAULT) including call_event rows,
-- which would empty the kind-based partial index re-created below.
UPDATE dm_messages SET kind = 'call_event' WHERE type = 'call_event' AND kind = 'text';

CREATE INDEX IF NOT EXISTS idx_dm_messages_conversation_kind
  ON dm_messages(conversation_id, kind)
  WHERE kind != 'text';
