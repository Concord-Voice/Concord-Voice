-- 000137_expiration_event_message_type.down.sql
-- Reverses 000137_expiration_event_message_type.up.sql.
--
-- The system rows are DELETED before the columns are dropped, and that ordering
-- is the whole point of this file rather than an incidental detail.
--
-- Dropping messages.type alone would leave every type='expiration_event' row in
-- place with its discriminator gone. Those rows carry content = '' by
-- construction (the meaningful data lives entirely in the payload column, which
-- this migration also drops), so after a rollback they read back as ordinary
-- user messages with empty content: a blank row in the middle of the timeline
-- for every channel and conversation whose expiration policy was ever changed.
-- Worse, the renderer's dispatch branch requires BOTH the discriminator and the
-- payload, so such a row falls through to the ordinary message renderer, which
-- attempts an E2EE decrypt pass on a '' that was never encrypted.
--
-- Deleting them is safe in a way that deleting user data would not be: these
-- rows are SERVER-AUTHORED and hold no user content. Everything they record —
-- the current window, who set it, when — is either still on the policy columns
-- (000127-000129, untouched here) or reconstructible from the audit trail. The
-- history of *changes* is lost on rollback; the current *state* is not.
--
-- Both tables are cleaned, not just the one that loses its discriminator.
-- dm_messages.type predates this migration (000026) and survives the rollback,
-- so a DM row would keep type='expiration_event' while losing the payload the
-- renderer also requires — the same fall-through by a different route.
--
-- All operations are idempotent; a re-run deletes nothing and drops nothing.

DELETE FROM dm_messages WHERE type = 'expiration_event';

-- Guarded because this file claims to be re-runnable and the bare form is not:
-- messages.type is dropped further down, so a SECOND run would reference a column
-- that no longer exists and abort, leaving the rest of the rollback unapplied.
-- dm_messages.type needs no guard -- it predates this migration (000026) and
-- survives the rollback.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'messages' AND column_name = 'type'
  ) THEN
    DELETE FROM messages WHERE type = 'expiration_event';
  END IF;
END $$;

ALTER TABLE dm_messages DROP COLUMN IF EXISTS expiration_event_payload;
ALTER TABLE messages DROP COLUMN IF EXISTS expiration_event_payload;
ALTER TABLE messages DROP COLUMN IF EXISTS type;
