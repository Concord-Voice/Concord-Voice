-- CHECK expressions accept NULL as non-violating.  Make the reverse
-- invariant explicit so a profile-shaped key cannot hide behind a NULL slot.
ALTER TABLE media_files
    ADD CONSTRAINT media_files_profile_key_requires_slot_check CHECK (
        media_tier <> 1
        OR storage_key !~ (
            '^(avatars|banners)/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}' ||
            '(/[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})?$'
        )
        OR profile_slot IS NOT NULL
    ) NOT VALID;
