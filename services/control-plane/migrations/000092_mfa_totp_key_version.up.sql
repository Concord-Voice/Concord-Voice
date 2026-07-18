-- #2307: versioned keyring for MFA_ENCRYPTION_KEY rotation.
-- Stamps which keyring version sealed each row's TOTP secret. Existing rows
-- were sealed under the deployment's current MFA_ENCRYPTION_KEY, which every
-- deployment runs as version 1 until its first rotation — DEFAULT 1 is
-- semantically exact for the backfill, not a guess. PG16 stores the default in
-- the catalog (no table rewrite); the table holds one row per enrolled user.
ALTER TABLE user_mfa_totp ADD COLUMN key_version SMALLINT NOT NULL DEFAULT 1;
