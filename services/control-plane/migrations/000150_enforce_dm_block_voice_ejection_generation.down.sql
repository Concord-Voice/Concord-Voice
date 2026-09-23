ALTER TABLE dm_block_voice_ejections
    ADD CONSTRAINT dm_block_voice_ejections_generation_not_null
    CHECK (generation IS NOT NULL) NOT VALID;

ALTER TABLE dm_block_voice_ejections
    ALTER COLUMN generation DROP NOT NULL;
