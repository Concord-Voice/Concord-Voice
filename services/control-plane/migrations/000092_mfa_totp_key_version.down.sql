-- Symmetric reversal of 000092. Lossless only while every row is at version 1
-- (i.e. before the first rotation): after a rotation the stamps are
-- load-bearing decrypt metadata, so run cmd/mfa-rekey to converge all rows to
-- one key and fold the keyring back to a single MFA_ENCRYPTION_KEY BEFORE
-- applying this down migration.
ALTER TABLE user_mfa_totp DROP COLUMN key_version;
