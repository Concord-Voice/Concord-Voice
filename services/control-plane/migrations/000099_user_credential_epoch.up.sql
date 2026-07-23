-- #2201: durable per-user credential epoch — the source of truth for bulk
-- access-token revocation on destructive password/key recovery. NULL means
-- "no epoch marker yet": tokens without a cred_epoch claim stay valid until
-- the user's first destructive flow, so this deploy invalidates nothing.
-- Value is 32 hex chars from 16 CSPRNG bytes, rotated in the same transaction
-- as credential/key-material changes. No default, no backfill, no index (only
-- ever read by primary key).
ALTER TABLE users ADD COLUMN credential_epoch TEXT;
