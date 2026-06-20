-- Track when MFA was first enabled so pre-MFA sessions can be challenged on next refresh.
ALTER TABLE users ADD COLUMN mfa_enabled_at TIMESTAMPTZ;
