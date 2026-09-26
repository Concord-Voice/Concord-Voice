-- Per-server "Enforce MFA On Dangerous Actions" toggle (#3453, epic #3452).
-- When TRUE, the permission resolver withholds the dangerous bits from any
-- member without an inline MFA factor (TOTP enabled AND confirmed, or
-- WebAuthn), and the dangerous-action gates (#3454, #3455) demand an MFA
-- confirmation inside their own transaction.
--
-- Default OFF: nothing changes for any server until an owner or Administrator
-- turns it on through PUT /api/v1/servers/:id/mfa-enforcement. A constant
-- DEFAULT is metadata-only on PostgreSQL 16 (no table rewrite), the 000039
-- precedent. No index: the flag is always read by primary key.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS enforce_mfa_dangerous_actions BOOLEAN NOT NULL DEFAULT FALSE;
