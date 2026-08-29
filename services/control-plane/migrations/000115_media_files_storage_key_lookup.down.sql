-- Reverse the orphan-reaper lookup index. Removing it does not break the
-- reaper's correctness, only its cost: the existence check falls back to a
-- sequential scan of media_files.
DROP INDEX IF EXISTS idx_media_files_storage_key_all;
