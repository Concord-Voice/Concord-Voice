-- Restore 000121's NOT VALID state before its down migration removes the
-- constraints.  This keeps rollback safe if validation exposed a deployment
-- incompatibility: the guard remains enforced for future writes.
ALTER TABLE tier1_profile_upload_intents
    DROP CONSTRAINT IF EXISTS tier1_profile_upload_intents_immutable_key_shape_check;

ALTER TABLE tier1_profile_upload_intents
    ADD CONSTRAINT tier1_profile_upload_intents_immutable_key_shape_check CHECK (
        storage_key ~ (
            CASE profile_slot
                WHEN 'avatar' THEN '^avatars/'
                WHEN 'banner' THEN '^banners/'
            END
            || user_id::text ||
            '/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$'
        )
    ) NOT VALID;

ALTER TABLE media_files
    DROP CONSTRAINT IF EXISTS media_files_tier1_profile_storage_shape_check;

ALTER TABLE media_files
    ADD CONSTRAINT media_files_tier1_profile_storage_shape_check CHECK (
        profile_slot IS NULL
        OR (
            media_tier = 1
            AND (
                (profile_slot = 'avatar' AND (
                    storage_key = 'avatars/' || uploader_id::text
                    OR storage_key ~ ('^avatars/' || uploader_id::text || '/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$')
                ))
                OR (profile_slot = 'banner' AND (
                    storage_key = 'banners/' || uploader_id::text
                    OR storage_key ~ ('^banners/' || uploader_id::text || '/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$')
                ))
            )
        )
    ) NOT VALID;
