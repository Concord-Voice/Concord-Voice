-- 000186_billing_credit_outbox.down.sql
-- Guarded down for 000186 (000182 shape).
--
-- Every row is credit a user redeemed a code for and has not yet received;
-- nothing else records it. Refuses while any row exists. The ACCESS EXCLUSIVE
-- lock is taken BEFORE the check, so a credit that commits while this file
-- waits for the lock is seen.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (185) as dirty before it runs this file, while the file's implicit
-- transaction rolled back and the schema is still at 186. Confirm the table
-- exists, then `force 186` and boot. See [internal]rules/migrations.md.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_pending BOOLEAN;
BEGIN
    IF to_regclass(format('%I.billing_credit_outbox', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with inserts and the drop.
    BEGIN
        EXECUTE format(
            'LOCK TABLE %I.billing_credit_outbox IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent rollback removed it while this one waited for the lock.
            RETURN;
    END;

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.billing_credit_outbox)',
        migration_schema)
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop billing_credit_outbox while code credits are pending'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format('DROP TABLE IF EXISTS %I.billing_credit_outbox', migration_schema);
END
$$;
