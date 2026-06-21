-- 000065_dm_messages_call_event_use_type_column.up.sql
--
-- Corrects migration 000064: the `kind` column added there was redundant
-- with the existing `type` column on dm_messages (added by 000026, whose
-- schema comment already mentions "'user', 'system' (call events, etc.)").
--
-- This migration:
--   1. Drops the partial index idx_dm_messages_conversation_kind (depends
--      on the kind column being dropped next).
--   2. Drops the kind column entirely.
--   3. Adds a new partial index on (conversation_id, type) filtered for
--      type = 'call_event', supporting fast "list call events" queries.
--
-- The call_event_payload JSONB column (also added by 000064) is RETAINED
-- because it's still needed — call event metadata (ring_id, caller, started_at,
-- ended_at, status, duration_seconds, participants) lives there.
--
-- Per spec section 6.4 (updated post-pivot 2026-05-28): the JSONB is stored
-- plaintext, not encrypted. Concord's E2EE is client-side (dm_messages.content
-- holds opaque ciphertext from the client); the server has no encryption
-- helper to mirror. Call event payloads are entirely server-known data
-- (user IDs + timestamps + status) — encrypting server-known data would add
-- zero security benefit. Defense against future DB leak is delegated to the
-- broader Postgres data-at-rest encryption layer.
--
-- Discriminator value for call events: type = 'call_event' (10 chars, fits
-- the existing varchar(20) bound). Application code (insertCallEvent helper
-- in dm/call_events.go, added by Task B5) MUST use this exact string.
--
-- NOTE on non-CONCURRENT index creation: golang-migrate runs each migration
-- inside a transaction, so CREATE INDEX CONCURRENTLY (which forbids
-- transactions) is not usable here. The partial-index predicate matches an
-- empty set at apply time (no `type = 'call_event'` rows exist pre-Task-B5),
-- so the table-level ACCESS EXCLUSIVE lock is effectively instantaneous. If
-- this migration is deferred to a deploy after call-event INSERTs already
-- run in production, switch to a two-migration split (CONCURRENT creation
-- outside transaction + a separate transactional metadata update). Documented
-- here per Copilot #1231 review feedback so future operators see the
-- tradeoff explicitly.

DROP INDEX IF EXISTS idx_dm_messages_conversation_kind;
ALTER TABLE dm_messages DROP COLUMN IF EXISTS kind;

CREATE INDEX IF NOT EXISTS idx_dm_messages_conversation_type_callevent
  ON dm_messages(conversation_id, type)
  WHERE type = 'call_event';
