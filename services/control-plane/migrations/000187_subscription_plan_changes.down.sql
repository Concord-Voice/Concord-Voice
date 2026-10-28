-- Do not discard a subscriber's billed price or a booked Stripe change.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_stripe_rows BOOLEAN;
BEGIN
    IF to_regclass(format('%I.subscriptions', migration_schema)) IS NULL THEN
        RETURN;
    END IF;
    EXECUTE format('LOCK TABLE %I.subscriptions IN ACCESS EXCLUSIVE MODE', migration_schema);
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.subscriptions WHERE source = ''stripe'')', migration_schema)
        INTO has_stripe_rows;
    IF has_stripe_rows THEN
        RAISE EXCEPTION 'cannot drop plan-change fields while Stripe subscriptions exist'
            USING ERRCODE = 'restrict_violation';
    END IF;
END
$$;

ALTER TABLE subscriptions
    DROP COLUMN pending_change_at,
    DROP COLUMN pending_billing_interval,
    DROP COLUMN pending_plan,
    DROP COLUMN stripe_schedule_id,
    DROP COLUMN stripe_unit_amount;
