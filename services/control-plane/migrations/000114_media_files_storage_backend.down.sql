-- ADR-0038 / #2759: revert per-object storage backend placement.
--
-- No CHECK constraint or index was added by the up migration, so dropping
-- the column is the entire reversal.
ALTER TABLE media_files DROP COLUMN IF EXISTS storage_backend;
