UPDATE dm_block_voice_ejections
SET generation = gen_random_uuid()
WHERE generation IS NULL;

ALTER TABLE dm_block_voice_ejections
    ALTER COLUMN generation SET DEFAULT gen_random_uuid();

ALTER TABLE dm_block_voice_ejections
    ADD CONSTRAINT dm_block_voice_ejections_generation_not_null
    CHECK (generation IS NOT NULL) NOT VALID;
