-- 000073_drop_users_e2ee_preference.down.sql
--
-- Reverse the up. Re-add e2ee_preference with DEFAULT TRUE (post-up semantic
-- state, not the original users DEFAULT — by this point under #201 the only
-- semantically-valid value is TRUE; mirrors the 000062 is_encrypted down
-- convention). NOT NULL DEFAULT TRUE on both tables.

BEGIN;

ALTER TABLE users                 ADD COLUMN IF NOT EXISTS e2ee_preference BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE pending_registrations ADD COLUMN IF NOT EXISTS e2ee_preference BOOLEAN NOT NULL DEFAULT TRUE;

COMMIT;
