-- 000062_remove_is_encrypted.up.sql
--
-- #201 Child C: drop is_encrypted from 5 tables (channels, messages,
-- dm_conversations, dm_messages, media_files). Child A (#1031) and Child B
-- (#1042) have removed all client and Go-handler references to the column.
-- Production data snapshot 2026-05-15: zero is_encrypted = FALSE rows across
-- all tables. The defensive UPDATE step is a safety net for any laggard rows
-- that landed during the Child B → #1026 rollout window.
--
-- media_files carries an additional dependency: the valid_media_context CHECK
-- constraint (000042 lines 36-51) references is_encrypted. Drop the constraint
-- before dropping the column, then re-add a semantically-equivalent constraint
-- expressed in terms of media_tier alone (the constraint's structural intent
-- — distinguishing tier 1 server-readable media from tier 2 E2EE attachments
-- — survives intact because media_tier already carries that distinction).

BEGIN;

-- 1) Defensive normalization (no-op on clean data; safety net for laggards).
UPDATE channels         SET is_encrypted = TRUE WHERE is_encrypted = FALSE;
UPDATE messages         SET is_encrypted = TRUE WHERE is_encrypted = FALSE;
UPDATE dm_conversations SET is_encrypted = TRUE WHERE is_encrypted = FALSE;
UPDATE dm_messages      SET is_encrypted = TRUE WHERE is_encrypted = FALSE;
UPDATE media_files      SET is_encrypted = TRUE WHERE is_encrypted = FALSE;

-- 2) Drop the CHECK constraint on media_files that references is_encrypted.
ALTER TABLE media_files DROP CONSTRAINT IF EXISTS valid_media_context;

-- 3) Drop columns. Other 4 tables have no indexes/constraints/views referencing
--    is_encrypted (verified by reading 000006, 000013, 000026 plus a grep
--    sweep across migrations/).
ALTER TABLE channels         DROP COLUMN IF EXISTS is_encrypted;
ALTER TABLE messages         DROP COLUMN IF EXISTS is_encrypted;
ALTER TABLE dm_conversations DROP COLUMN IF EXISTS is_encrypted;
ALTER TABLE dm_messages      DROP COLUMN IF EXISTS is_encrypted;
ALTER TABLE media_files      DROP COLUMN IF EXISTS is_encrypted;

-- 4) Re-add the media_files tier invariant using media_tier alone.
--    tier 1 (media_tier = 1): authenticated server-readable media, no per-channel
--      E2EE context, no key_version.
--    tier 2 (media_tier = 2): E2EE attachment, requires key_version, requires
--      exactly one of channel_id / conversation_id.
ALTER TABLE media_files ADD CONSTRAINT valid_media_context CHECK (
    (media_tier = 1
        AND key_version IS NULL
        AND channel_id IS NULL
        AND conversation_id IS NULL
    ) OR
    (media_tier = 2
        AND key_version IS NOT NULL
        AND (
            (channel_id IS NOT NULL AND conversation_id IS NULL) OR
            (channel_id IS NULL AND conversation_id IS NOT NULL)
        )
    )
);

COMMIT;
