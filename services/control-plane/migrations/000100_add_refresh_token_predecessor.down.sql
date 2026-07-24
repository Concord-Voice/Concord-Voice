DROP INDEX IF EXISTS idx_refresh_tokens_predecessor_id;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS predecessor_id;
