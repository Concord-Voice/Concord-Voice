-- Add the profile storage shape guard without scanning media_files while the
-- ACCESS EXCLUSIVE lock is held.  Migration 000122 validates it separately.
ALTER TABLE media_files
    ADD CONSTRAINT media_files_tier1_profile_storage_shape_check CHECK (
        profile_slot IS NULL
        OR (
            media_tier = 1
            AND (
                (
                    profile_slot = 'avatar'
                    AND (
                        storage_key = 'avatars/' || uploader_id::text
                        OR storage_key ~ (
                            '^avatars/' || uploader_id::text ||
                            '/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$'
                        )
                    )
                )
                OR (
                    profile_slot = 'banner'
                    AND (
                        storage_key = 'banners/' || uploader_id::text
                        OR storage_key ~ (
                            '^banners/' || uploader_id::text ||
                            '/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$'
                        )
                    )
                )
            )
        )
    ) NOT VALID;

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

COMMENT ON CONSTRAINT media_files_tier1_profile_storage_shape_check ON media_files IS
    'Tier 1 profile metadata may reference only its matching legacy key or immutable UUID generation.';
COMMENT ON CONSTRAINT tier1_profile_upload_intents_immutable_key_shape_check
    ON tier1_profile_upload_intents IS
    'Pre-PutObject profile intents must use an immutable UUID generation under the matching user and slot prefix.';
