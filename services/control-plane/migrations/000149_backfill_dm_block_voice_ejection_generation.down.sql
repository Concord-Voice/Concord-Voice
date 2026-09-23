ALTER TABLE dm_block_voice_ejections
    DROP CONSTRAINT IF EXISTS dm_block_voice_ejections_generation_not_null;

ALTER TABLE dm_block_voice_ejections
    ALTER COLUMN generation DROP DEFAULT;
