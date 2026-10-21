-- 000179_subscriptions_per_source_live_indexes.down.sql
-- Guarded down for 000179 (000117 shape; schema-aware like 000170).
--
-- Restoring 000070's idx_subscriptions_user_active needs at most one live row
-- per user. Refuses, rather than picking a row to end, while any user holds
-- two: a live code row beside a live Stripe row, or two live Stripe rows. The
-- ACCESS EXCLUSIVE lock is taken BEFORE the check, so a row that commits while
-- this file waits for the lock is seen.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (178) as dirty before it runs this file, while the file's implicit
-- transaction rolled back and the schema is still at 179. Confirm that
-- idx_subscriptions_user_live_code exists, then `force 179` and boot. NEVER
-- `force 178`: a following down would run 000178's down against a schema
-- that still carries 000179. See [internal]rules/migrations.md.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_blocking_user BOOLEAN;
BEGIN
    IF to_regclass(format('%I.subscriptions', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    EXECUTE format(
        'LOCK TABLE %I.subscriptions IN ACCESS EXCLUSIVE MODE',
        migration_schema);

    EXECUTE format(
        'SELECT EXISTS (
             SELECT 1 FROM %I.subscriptions
              WHERE status IN (''active'', ''trialing'', ''past_due'')
              GROUP BY user_id
             HAVING count(*) > 1)',
        migration_schema)
        INTO has_blocking_user;
    IF has_blocking_user THEN
        RAISE EXCEPTION
            'cannot restore idx_subscriptions_user_active while a user holds more than one live subscriptions row'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format('DROP INDEX IF EXISTS %I.idx_subscriptions_user_id', migration_schema);
    EXECUTE format('DROP INDEX IF EXISTS %I.idx_subscriptions_stripe_live_period_end', migration_schema);
    EXECUTE format('DROP INDEX IF EXISTS %I.idx_subscriptions_user_live_stripe', migration_schema);
    EXECUTE format('DROP INDEX IF EXISTS %I.idx_subscriptions_stripe_subscription_id', migration_schema);
    EXECUTE format('DROP INDEX IF EXISTS %I.idx_subscriptions_user_live_code', migration_schema);
    EXECUTE format(
        'CREATE UNIQUE INDEX idx_subscriptions_user_active
             ON %I.subscriptions (user_id)
             WHERE status IN (''active'', ''trialing'', ''past_due'')',
        migration_schema);
END
$$;
