-- Safe under [internal]rules/migrations.md "No DROP COLUMN without confirmed
-- data migration" because the two columns are net-new in this migration's up.
-- IF EXISTS makes the down idempotent — re-running on a partially-rolled-back
-- schema (e.g., one column dropped manually during recovery) doesn't error.
ALTER TABLE users
    DROP COLUMN IF EXISTS trust_sso_security,
    DROP COLUMN IF EXISTS password_login_disabled;

DROP TABLE IF EXISTS user_sso_identities;
