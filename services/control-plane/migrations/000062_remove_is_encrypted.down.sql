-- 000062_remove_is_encrypted.down.sql
--
-- Reverse the up. Re-add is_encrypted with DEFAULT TRUE (post-up semantic state,
-- not the original DEFAULT FALSE — rationale in parent spec §6: by this point
-- the only semantically-valid value is TRUE; defaulting to FALSE would silently
-- introduce plaintext-fallback rows, the exact regression #201 exists to
-- prevent). Restore the original media_files CHECK constraint exactly as
-- 000042 defined it.

BEGIN;

-- 1) Drop the media_tier-only constraint introduced by the up migration.
ALTER TABLE media_files DROP CONSTRAINT IF EXISTS valid_media_context;

-- 2) Re-add columns with DEFAULT TRUE.
ALTER TABLE channels         ADD COLUMN IF NOT EXISTS is_encrypted BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE messages         ADD COLUMN IF NOT EXISTS is_encrypted BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE dm_conversations ADD COLUMN IF NOT EXISTS is_encrypted BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE dm_messages      ADD COLUMN IF NOT EXISTS is_encrypted BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE media_files      ADD COLUMN IF NOT EXISTS is_encrypted BOOLEAN NOT NULL DEFAULT TRUE;

-- 3) Restore the original valid_media_context constraint exactly as 000042 defined it.
ALTER TABLE media_files ADD CONSTRAINT valid_media_context CHECK (
    (media_tier = 1
        AND is_encrypted = FALSE
        AND key_version IS NULL
        AND channel_id IS NULL
        AND conversation_id IS NULL
    ) OR
    (media_tier = 2
        AND is_encrypted = TRUE
        AND key_version IS NOT NULL
        AND (
            (channel_id IS NOT NULL AND conversation_id IS NULL) OR
            (channel_id IS NULL AND conversation_id IS NOT NULL)
        )
    )
);

COMMIT;
