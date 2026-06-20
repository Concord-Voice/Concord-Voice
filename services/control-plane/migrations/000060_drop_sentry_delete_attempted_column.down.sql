-- services/control-plane/migrations/000060_drop_sentry_delete_attempted_column.down.sql
--
-- Restore the sentry_delete_attempted column with its original constraints
-- from migration 000059 (BOOLEAN NOT NULL DEFAULT FALSE). Existing rows
-- added between 000060.up and 000060.down receive the DEFAULT FALSE value
-- on column re-add — this is acceptable: the historical "did Sentry get
-- called?" data point is unrecoverable, but the column structure is
-- restored so any downstream code that depends on the schema still works.

ALTER TABLE account_deletions
    ADD COLUMN sentry_delete_attempted BOOLEAN NOT NULL DEFAULT FALSE;
