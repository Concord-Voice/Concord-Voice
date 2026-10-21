-- 000180_stripe_customers.down.sql
-- Guarded down for 000180 (000117 shape; schema-aware like 000170).
--
-- Refuses while any row exists. A row is the only local record of which Stripe
-- Customer belongs to which user; dropping it would orphan the Customer from
-- both Reconcile and account erasure. The ACCESS EXCLUSIVE lock is taken
-- BEFORE the check, so a mapping that commits while this file waits is seen.
-- In a development database with throwaway test-mode Customers, empty the
-- table first (see [internal]rules/migrations.md); never do that in production.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (179) as dirty before it runs this file, while the file's implicit
-- transaction rolled back and the schema is still at 180. Confirm the table
-- exists, then `force 180` and boot. NEVER `force 179`.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_mapping BOOLEAN;
BEGIN
    IF to_regclass(format('%I.stripe_customers', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with inserts and the drop.
    BEGIN
        EXECUTE format(
            'LOCK TABLE %I.stripe_customers IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent rollback removed it while this one waited for the lock.
            RETURN;
    END;

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.stripe_customers)',
        migration_schema)
        INTO has_mapping;
    IF has_mapping THEN
        RAISE EXCEPTION
            'cannot drop stripe_customers while it maps a user to a Stripe Customer'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format('DROP TABLE IF EXISTS %I.stripe_customers', migration_schema);
END
$$;
