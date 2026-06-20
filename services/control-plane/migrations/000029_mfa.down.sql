-- Reverse #89: MFA framework
DROP TABLE IF EXISTS user_mfa_webauthn;
DROP TABLE IF EXISTS user_mfa_totp;
-- NOTE: recovery_only_methods and recovery_hardened are dropped in 000030 down
ALTER TABLE users DROP COLUMN IF EXISTS mfa_methods;
ALTER TABLE users DROP COLUMN IF EXISTS mfa_enabled;
