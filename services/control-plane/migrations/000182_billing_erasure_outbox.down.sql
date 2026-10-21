-- 000182_billing_erasure_outbox.down.sql
-- Guarded down for 000182 (000117 shape; schema-aware like 000170).
--
-- Every row is a pending obligation to erase a Customer whose user is already
-- gone, and nothing else records it. Refuses while any row exists. The ACCESS
-- EXCLUSIVE lock is taken BEFORE the check, so an obligation that commits
-- while this file waits for the lock is seen.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (181) as dirty before it runs this file, while the file's implicit
-- transaction rolled back and the schema is still at 182. Confirm the table
-- exists, then `force 182` and boot. NEVER `force 181`: the next down would
-- run 000181's plain drop and silently discard the webhook dedupe table. See
-- [internal]rules/migrations.md.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_pending BOOLEAN;
BEGIN
    IF to_regclass(format('%I.billing_erasure_outbox', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with inserts and the drop.
    BEGIN
        EXECUTE format(
            'LOCK TABLE %I.billing_erasure_outbox IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent rollback removed it while this one waited for the lock.
            RETURN;
    END;

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.billing_erasure_outbox)',
        migration_schema)
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop billing_erasure_outbox while Stripe Customer erasure obligations are pending'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format('DROP TABLE IF EXISTS %I.billing_erasure_outbox', migration_schema);
END
$$;
