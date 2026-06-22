-- 000077_admin_auth.down.sql
-- Reverse 000077: drop the admin auth tables and the restricted role.
-- Drop tables first (FKs cascade within the set), then the role once nothing
-- depends on it.

DROP TABLE IF EXISTS admin_audit_log;
DROP TABLE IF EXISTS admin_webauthn_credentials;
DROP TABLE IF EXISTS admin_users;

-- The role is shared infra; drop it only if it still exists. Any GRANTs to it
-- were on the tables just dropped, so it has no remaining dependencies here.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'concord_admin_rt') THEN
        DROP ROLE concord_admin_rt;
    END IF;
END
$$;
