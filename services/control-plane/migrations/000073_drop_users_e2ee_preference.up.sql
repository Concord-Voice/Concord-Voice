-- 000073_drop_users_e2ee_preference.up.sql
--
-- #1648 (#201 residual cleanup, Family 2): drop the fully-vestigial per-user
-- e2ee_preference column from users (added 000001) and pending_registrations
-- (added 000058). The field was sourced from Register.tsx's e2eeEnabled, which
-- was hardcoded TRUE ("E2EE is mandatory"); no code ever branched on it. Under
-- the E2EE-everywhere invariant (#201) it should not exist.
--
-- Behavior-inert: removal changes no auth/registration behavior. The defensive
-- UPDATE step is a safety net for any laggard FALSE rows (none expected — the
-- only writer hardcoded TRUE), mirroring the 000062 is_encrypted drop pattern.

BEGIN;

-- 1) Defensive normalization (no-op on clean data; safety net for laggards).
UPDATE users                 SET e2ee_preference = TRUE WHERE e2ee_preference = FALSE;
UPDATE pending_registrations SET e2ee_preference = TRUE WHERE e2ee_preference = FALSE;

-- 2) Drop the columns. No indexes/constraints/views reference e2ee_preference
--    (verified by grep sweep across migrations/).
ALTER TABLE users                 DROP COLUMN IF EXISTS e2ee_preference;
ALTER TABLE pending_registrations DROP COLUMN IF EXISTS e2ee_preference;

COMMIT;
