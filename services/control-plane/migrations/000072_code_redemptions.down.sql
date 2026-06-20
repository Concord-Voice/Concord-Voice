-- 000072_code_redemptions.down.sql
-- Reverses 000072. The UNIQUE(code_id,user_id) implicit index drops with the table.
DROP TABLE IF EXISTS code_redemptions;
