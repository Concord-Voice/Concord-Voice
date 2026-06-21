-- services/control-plane/migrations/000060_drop_sentry_delete_attempted_column.up.sql
--
-- Drop the sentry_delete_attempted column from account_deletions. This
-- column recorded whether the privacy-erasure handler invoked Sentry's
-- events-by-user deletion API before deleting the account. The Sentry
-- integration is being removed entirely (#758, parent #756) — earlier
-- in the same PR the audit-row INSERT was changed to omit the column,
-- so it is permanently false for all new rows once the application
-- update reaches production. The field has no forward-looking meaning.
--
-- account_deletions is a low-write audit table (one row per account erasure),
-- so the ACCESS EXCLUSIVE lock that ALTER TABLE ... DROP COLUMN takes is held
-- for negligible time. PostgreSQL has no CONCURRENTLY variant for DROP COLUMN
-- (unlike CREATE INDEX), so the lock-window-vs-write-rate argument is the
-- relevant safety check here.

ALTER TABLE account_deletions DROP COLUMN sentry_delete_attempted;
