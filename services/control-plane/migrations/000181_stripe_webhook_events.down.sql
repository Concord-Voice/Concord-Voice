-- 000181_stripe_webhook_events.down.sql
-- Plain drop, unguarded on purpose (spec section 5). The table is dedupe only
-- and Reconcile is idempotent, so a lost row costs at most a repeated fetch.
--
-- This down never refuses. A refused 000182 down, however, leaves version 181
-- recorded as dirty: force 182 then, NEVER 181, or the next down runs this
-- file and silently discards the dedupe history. See
-- [internal]rules/migrations.md.
DROP INDEX IF EXISTS idx_stripe_webhook_events_processed_at;
DROP TABLE IF EXISTS stripe_webhook_events;
