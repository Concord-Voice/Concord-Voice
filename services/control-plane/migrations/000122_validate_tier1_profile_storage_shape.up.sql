-- Validation is isolated from the short constraint-installation lock in 000121.
ALTER TABLE media_files
    VALIDATE CONSTRAINT media_files_tier1_profile_storage_shape_check;

ALTER TABLE tier1_profile_upload_intents
    VALIDATE CONSTRAINT tier1_profile_upload_intents_immutable_key_shape_check;
