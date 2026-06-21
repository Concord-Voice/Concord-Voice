-- 000071_redemption_codes.down.sql
-- Reverses 000071. Net-new table — DROP is safe.
DROP INDEX IF EXISTS idx_redemption_codes_batch;
DROP TABLE IF EXISTS redemption_codes;
