-- 000069_age_verification.down.sql
-- Reverses the up migration (DROP INDEX → drop the three users columns → DROP TABLE).
-- Rollback caveat: rolling back while users are disabled silently re-enables them (the
-- column is gone). Acceptable for an unreleased migration, but stated explicitly here.

DROP INDEX IF EXISTS idx_users_disabled;
ALTER TABLE users DROP COLUMN IF EXISTS disabled_at;
ALTER TABLE users DROP COLUMN IF EXISTS disabled_reason;
ALTER TABLE users DROP COLUMN IF EXISTS disabled;
DROP TABLE IF EXISTS age_verification_records;
