ALTER TABLE tier1_profile_upload_intents
    DROP CONSTRAINT IF EXISTS tier1_profile_upload_intents_immutable_key_shape_check;

ALTER TABLE media_files
    DROP CONSTRAINT IF EXISTS media_files_tier1_profile_storage_shape_check;
