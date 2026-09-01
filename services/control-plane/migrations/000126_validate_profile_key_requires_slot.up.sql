-- Migration 000120 classified only live legacy profile rows. Historical
-- replacements can leave soft-deleted deterministic rows, and validation must
-- classify those rows too before checking the reverse key/slot invariant.
UPDATE media_files
SET profile_slot = CASE
    WHEN storage_key = 'avatars/' || uploader_id::text THEN 'avatar'
    WHEN storage_key = 'banners/' || uploader_id::text THEN 'banner'
END
WHERE media_tier = 1
  AND profile_slot IS NULL
  AND storage_key IN (
      'avatars/' || uploader_id::text,
      'banners/' || uploader_id::text
  );

ALTER TABLE media_files
    VALIDATE CONSTRAINT media_files_profile_key_requires_slot_check;
