ALTER TABLE dm_block_voice_ejections
    VALIDATE CONSTRAINT dm_block_voice_ejections_generation_not_null;

ALTER TABLE dm_block_voice_ejections
    ALTER COLUMN generation SET NOT NULL;

ALTER TABLE dm_block_voice_ejections
    DROP CONSTRAINT dm_block_voice_ejections_generation_not_null;
