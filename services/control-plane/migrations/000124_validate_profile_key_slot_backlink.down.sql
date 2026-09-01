-- Restore the 000123 guard as NOT VALID before 000123 down removes it.
ALTER TABLE media_files
    DROP CONSTRAINT IF EXISTS media_files_profile_key_slot_backlink_check;

ALTER TABLE media_files
    ADD CONSTRAINT media_files_profile_key_slot_backlink_check CHECK (
        media_tier <> 1
        OR storage_key !~ (
            '^(avatars|banners)/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}' ||
            '(/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})?$'
        )
        OR (
            (profile_slot = 'avatar' AND storage_key ~ ('^avatars/' || uploader_id::text || '(/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})?$'))
            OR (profile_slot = 'banner' AND storage_key ~ ('^banners/' || uploader_id::text || '(/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})?$'))
        )
    ) NOT VALID;
