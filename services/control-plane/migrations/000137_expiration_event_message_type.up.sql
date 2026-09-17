-- 000137_expiration_event_message_type.up.sql
--
-- Adds a message 'type' discriminator to the channel messages table (it had
-- none — 000026 gave dm_messages its 'type' column years earlier, with
-- 'call_event' established as a non-'user' value by 000064/000065) and a
-- dedicated 'expiration_event_payload' JSONB column to BOTH messages and
-- dm_messages, so a message-expiration policy change (set / change / clear)
-- can persist as a durable, attributed system row in the normal timeline.
-- See [internal]specs/2026-09-16-1351-expiration-purge-presentation-design.md §3.6.
--
-- Follows the 000064/000065 precedent exactly: a TEXT discriminator plus a
-- plaintext JSONB payload column for server-authored metadata the client
-- never decrypts (useMessageFetch skips the E2EE pass for any non-'user'
-- type; this row's type = 'expiration_event').
--
-- A NEW payload column, not a reuse of call_event_payload: call_event_payload
-- is a call-shaped envelope (ring_id, caller_user_id, participant_user_ids,
-- started_at, ended_at, status, duration_seconds) with its own documented
-- single-writer contract ("All call_event_payload INSERTs MUST go through
-- insertCallEvent" — internal/dm/call_events.go). Storing an unrelated
-- expiration-change shape (kind, actor, previous/new window, changed_at)
-- under that name would violate that contract and mislead any future reader
-- who infers the column's shape from its name. JSONB's schemalessness does
-- not excuse the naming collision.
--
-- No partial "list expiration events" index, unlike 000065's
-- idx_dm_messages_conversation_type_callevent: that index exists to support a
-- call-history query with no expiration-event analogue today — per §3.6,
-- expiration-event rows are read only as part of the ordinary timeline scroll
-- (channel: idx_messages_channel_id / idx_messages_created_at; DM:
-- idx_dm_messages_conversation), which already covers every type value
-- equally. Add one later if an equivalent query appears, naming it at that
-- time (mirrors the 000114/000115 storage_backend precedent).
--
-- Zero-downtime safe per [internal]rules/migrations.md: every ADD COLUMN below
-- carries a constant DEFAULT or no default at all (never a volatile
-- expression), so PostgreSQL 11+ applies it as a metadata-only operation with
-- no table rewrite and no ACCESS EXCLUSIVE scan.

ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS type VARCHAR(20) NOT NULL DEFAULT 'user';
-- 'user' for an ordinary message; 'expiration_event' for a durable
-- message-expiration policy-change row (written by the handlers in
-- internal/channels/expiration_events.go). Mirrors dm_messages.type (000026),
-- whose column comment already documents 'user' / 'system' (call events,
-- etc.) — messages now carries the same shape now that it needs its own
-- non-'user' value.

ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS expiration_event_payload JSONB;
-- NULL for type != 'expiration_event'. Plaintext JSONB envelope for
-- type = 'expiration_event' — server-authored metadata (kind, actor,
-- previous/new window_seconds, changed_at) the client never decrypts.
-- Concord's client-side E2EE wraps messages.content; this payload is
-- entirely server-known, so it stores plaintext, matching
-- call_event_payload's rationale (000064).

ALTER TABLE dm_messages
  ADD COLUMN IF NOT EXISTS expiration_event_payload JSONB;
-- dm_messages already carries the 'type' discriminator (000026); this
-- migration only adds its expiration-event payload column. The DM writer
-- uses type = 'expiration_event', the same discriminator value as the
-- channel side above.

COMMENT ON COLUMN messages.type IS 'Message kind discriminator: ''user'' for an ordinary message, ''expiration_event'' for a durable expiration policy-change system row.';
COMMENT ON COLUMN messages.expiration_event_payload IS 'Plaintext JSONB envelope for type=''expiration_event'' rows; NULL otherwise. Never decrypted client-side.';
COMMENT ON COLUMN dm_messages.expiration_event_payload IS 'Plaintext JSONB envelope for type=''expiration_event'' rows; NULL otherwise. Never decrypted client-side.';
