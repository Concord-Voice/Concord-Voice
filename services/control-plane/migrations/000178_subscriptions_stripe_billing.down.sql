-- 000178_subscriptions_stripe_billing.down.sql
-- Guarded down for 000178 (000117 shape; schema-aware like 000170, so it acts
-- on the schema its up ran in, never a hardcoded public).
--
-- Refuses while any row is Stripe-sourced or carries a status 000178 added.
-- Dropping the columns would silently discard a Stripe row's plan, interval,
-- founders rate and cancel_at, and the restored 000083 status CHECK cannot
-- hold a new status anyway. The ACCESS EXCLUSIVE lock is taken BEFORE the
-- checks, so a row that commits while this file waits for the lock is seen.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (177) as dirty before it runs this file, while the file's implicit
-- transaction rolled back and the schema is still at 178. Confirm that
-- subscriptions.plan exists, then `force 178` and boot. NEVER `force 177`: a
-- following down would run 000177's guarded feedback-screenshot down. See
-- [internal]rules/migrations.md.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_blocking_row BOOLEAN;
BEGIN
    IF to_regclass(format('%I.subscriptions', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    EXECUTE format(
        'LOCK TABLE %I.subscriptions IN ACCESS EXCLUSIVE MODE',
        migration_schema);

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.subscriptions WHERE source = ''stripe'')',
        migration_schema)
        INTO has_blocking_row;
    IF has_blocking_row THEN
        RAISE EXCEPTION
            'cannot roll back 000178 while a subscriptions row is Stripe-sourced'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.subscriptions
                         WHERE status IN (''incomplete_expired'', ''unpaid'', ''paused''))',
        migration_schema)
        INTO has_blocking_row;
    IF has_blocking_row THEN
        RAISE EXCEPTION
            'cannot roll back 000178 while a subscriptions row has status incomplete_expired, unpaid or paused'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format(
        'ALTER TABLE %I.subscriptions DROP CONSTRAINT IF EXISTS subscriptions_stripe_period_check',
        migration_schema);
    EXECUTE format(
        'ALTER TABLE %I.subscriptions DROP CONSTRAINT IF EXISTS subscriptions_stripe_identity_check',
        migration_schema);
    EXECUTE format(
        'ALTER TABLE %I.subscriptions DROP CONSTRAINT subscriptions_status_check',
        migration_schema);
    EXECUTE format(
        'ALTER TABLE %I.subscriptions ADD CONSTRAINT subscriptions_status_check
             CHECK (status IN (''active'', ''trialing'', ''past_due'', ''canceled'', ''incomplete'', ''expired''))',
        migration_schema);
    EXECUTE format(
        'ALTER TABLE %I.subscriptions
             DROP COLUMN IF EXISTS cancel_at,
             DROP COLUMN IF EXISTS founders_rate,
             DROP COLUMN IF EXISTS billing_interval,
             DROP COLUMN IF EXISTS plan',
        migration_schema);
END
$$;
