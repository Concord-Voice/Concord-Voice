-- 000076_redemption_code_issuance.down.sql
-- Reverses 000076 up. The indexes are dropped with the table (DROP TABLE removes
-- dependent indexes); explicit DROP INDEX is unnecessary but harmless. Net-new
-- table, no data migration — DROP is safe.
DROP TABLE IF EXISTS redemption_code_issuance;
