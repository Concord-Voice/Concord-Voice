-- 000067_temp_sbac_overrides.down.sql
-- Reverses the up migration: drops the partial index and the 3 temp-SBAC columns.
DROP INDEX IF EXISTS idx_cpo_temporary;
ALTER TABLE channel_permission_overrides
    DROP COLUMN IF EXISTS granted_at,
    DROP COLUMN IF EXISTS temporary_reason,
    DROP COLUMN IF EXISTS is_temporary;
