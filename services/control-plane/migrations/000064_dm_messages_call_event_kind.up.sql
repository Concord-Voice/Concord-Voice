-- 000064_dm_messages_call_event_kind.up.sql
-- Adds 'kind' discriminator + call_event_payload JSONB column to dm_messages
-- for DM voice call event persistence per spec
-- [internal]specs/2026-05-27-1209-dm-group-voice-calls-design.md §6.4.
--
-- Zero-downtime safe per [internal]rules/migrations.md:
--   - DEFAULT 'text' on the new column means no row rewrite on populated tables.
--   - Partial index on (conversation_id, kind) is created with the WHERE clause
--     to enable fast "list call events in this conversation" queries without
--     bloating the index with the dominant 'text' rows.
--
-- call_event_payload contents are a plaintext JSONB envelope holding
-- server-known call metadata (caller, callees, ring_id, started_at, ended_at,
-- status, duration_seconds). Concord's client-side E2EE wraps dm_messages.content
-- but call-event payloads are entirely server-derived, so encrypting them adds
-- no security benefit — see spec §6.4 (post-pivot 2026-05-28) for the rationale.
-- Defense against future DB leak is delegated to Postgres data-at-rest encryption.

ALTER TABLE dm_messages
  ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'text';

ALTER TABLE dm_messages
  ADD COLUMN IF NOT EXISTS call_event_payload JSONB;
-- NULL for kind != 'call_event'. Plaintext JSONB envelope for kind = 'call_event'.

CREATE INDEX IF NOT EXISTS idx_dm_messages_conversation_kind
  ON dm_messages(conversation_id, kind)
  WHERE kind != 'text';
-- Partial index for non-'text' rows (currently only 'call_event'). Enables
-- "show me only call events in this conversation" queries without scanning
-- the dominant text-message rows.
