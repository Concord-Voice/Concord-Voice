-- 000067_temp_sbac_overrides.up.sql
-- Adds temporary (move-granted) flagging to channel permission overrides (#487, ADR-0023).
-- A "temporary" override is granted when a user is moved into a voice channel they cannot
-- otherwise see; it is cleaned up authoritatively on departure (presence-bound, D3).
ALTER TABLE channel_permission_overrides
    ADD COLUMN is_temporary     BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN temporary_reason VARCHAR(32),          -- e.g. 'move_granted'
    ADD COLUMN granted_at       TIMESTAMPTZ;           -- audit only (presence-bound lifetime, D3)

-- Partial index supports the cleanup-on-leave lookup and the nightly orphan sweep.
-- NOTE: golang-migrate runs migrations inside a transaction, so CREATE INDEX CONCURRENTLY
-- cannot be used here (it is forbidden inside a transaction block). channel_permission_overrides
-- is a small pre-GA table, so a plain CREATE INDEX is acceptable per [internal]rules/migrations.md
-- (CONCURRENTLY is preferred only for large tables). For a large production table this index
-- should be created manually with CONCURRENTLY before the migration runs.
CREATE INDEX IF NOT EXISTS idx_cpo_temporary
    ON channel_permission_overrides (target_id, channel_id)
    WHERE is_temporary;
