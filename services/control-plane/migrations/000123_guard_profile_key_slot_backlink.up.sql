-- Close the reverse direction of the profile invariant.  A legacy or
-- immutable avatar/banner-shaped key must not be admitted as an unclassified
-- Tier-1 row, including by an old mixed-version writer.  000124 validates the
-- historical rows separately.
ALTER TABLE media_files
    ADD CONSTRAINT media_files_profile_key_slot_backlink_check CHECK (
        media_tier <> 1
        OR storage_key !~ (
            '^(avatars|banners)/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}' ||
            '(/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})?$'
        )
        OR (
            (profile_slot = 'avatar' AND storage_key ~ (
                '^avatars/' || uploader_id::text ||
                '(/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})?$'
            ))
            OR (profile_slot = 'banner' AND storage_key ~ (
                '^banners/' || uploader_id::text ||
                '(/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})?$'
            ))
        )
    ) NOT VALID;

COMMENT ON CONSTRAINT media_files_profile_key_slot_backlink_check ON media_files IS
    'Profile-shaped Tier 1 keys require a non-null slot matching the owner and avatar/banner prefix.';
