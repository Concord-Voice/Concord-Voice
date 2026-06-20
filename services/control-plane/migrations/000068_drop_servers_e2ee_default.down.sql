-- 000068_drop_servers_e2ee_default.down.sql
-- Reverse the up: re-add the column with the post-#201 default-TRUE convention
-- (mirrors 000062.down — by this point the only semantically-valid value is TRUE;
-- defaulting to FALSE would silently re-introduce a misleading opt-out signal).

BEGIN;

ALTER TABLE servers ADD COLUMN IF NOT EXISTS e2ee_default BOOLEAN NOT NULL DEFAULT TRUE;

COMMIT;
