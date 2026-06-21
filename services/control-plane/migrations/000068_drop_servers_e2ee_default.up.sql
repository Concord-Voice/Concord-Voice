-- 000068_drop_servers_e2ee_default.up.sql
-- #1647 Family 1 (#201 residual): drop the behavior-inert per-server e2ee_default
-- opt-out. Nothing reads this value to gate encryption (channel creation requires
-- wrapped keys unconditionally — services/control-plane/internal/channels/handlers.go),
-- so the drop is data-loss-safe. Defensive normalization first in case any FALSE
-- rows linger. Mirrors the 000062 (remove is_encrypted) pattern.

BEGIN;

-- 1) Defensive normalization (no-op on clean data; safety net for laggard rows).
UPDATE servers SET e2ee_default = TRUE WHERE e2ee_default = FALSE;

-- 2) Drop the column (no indexes/constraints/views reference it — added bare by 000013).
ALTER TABLE servers DROP COLUMN IF EXISTS e2ee_default;

COMMIT;
