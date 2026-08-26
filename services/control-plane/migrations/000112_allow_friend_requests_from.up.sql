-- #1240: per-user gate on who may send a friend request.
--
-- Non-volatile default, so PG16 records it in attmissingval — no table rewrite
-- and no backfill. golang-migrate wraps each file in one transaction, so
-- CONCURRENTLY is forbidden here; correct, since no index is created.
--
-- No index: a three-value enum on a table read only by primary-key probe is
-- write cost with no read benefit.
--
-- ADD CONSTRAINT has no IF NOT EXISTS. Running this against a tree where the
-- column already exists but the constraint does not will fail loudly, which is
-- the desired fail-closed behaviour (same posture as 000110). Do not soften it
-- with a DO $$ block.
ALTER TABLE privacy_settings
    ADD COLUMN IF NOT EXISTS allow_friend_requests_from VARCHAR(16) NOT NULL DEFAULT 'everyone';

ALTER TABLE privacy_settings
    ADD CONSTRAINT privacy_settings_allow_friend_requests_from_check
    CHECK (allow_friend_requests_from IN ('everyone', 'mutual_servers', 'nobody'));
