-- Reverse #89 recovery columns
ALTER TABLE users DROP COLUMN IF EXISTS recovery_hardened;
ALTER TABLE users DROP COLUMN IF EXISTS recovery_only_methods;
