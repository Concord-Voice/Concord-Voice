DROP INDEX IF EXISTS idx_refresh_tokens_revoked;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS revoked_at;
