-- #2428: bind grace-period refresh recovery to the exact predecessor->successor
-- lineage instead of a time window. predecessor_id records, at rotation time,
-- the id of the source token this token replaces. Nullable + no backfill:
-- pre-existing rows get NULL, which the lineage lookup reads as "no successor"
-- (fail-closed 401) -- correct, since no historical token has a recorded successor
-- to recover to. Plain CREATE INDEX (golang-migrate wraps each file in a tx, so
-- CONCURRENTLY is illegal); partial WHERE predecessor_id IS NOT NULL keeps it
-- tiny (only rotation/recovery successors carry a value). No self-FK: it would
-- force an ON DELETE dance with the 90-day revoked-token purge for no benefit --
-- a dangling id is harmless behind the lookup's `revoked_at IS NULL` filter.
ALTER TABLE refresh_tokens ADD COLUMN predecessor_id UUID;
CREATE INDEX idx_refresh_tokens_predecessor_id
  ON refresh_tokens (predecessor_id) WHERE predecessor_id IS NOT NULL;
