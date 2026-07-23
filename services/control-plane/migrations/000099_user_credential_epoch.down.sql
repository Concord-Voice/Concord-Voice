-- Symmetric reversal of 000099. Safe: verification treats a claim-bearing
-- token against a NULL column as "no epoch marker" and accepts, so dropping
-- the column disarms the fence without stranding any session.
ALTER TABLE users DROP COLUMN credential_epoch;
