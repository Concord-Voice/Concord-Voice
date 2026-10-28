-- 000188_subscription_seats.down.sql
-- Guarded down for 000188 (000182 shape).
--
-- A pending or active seat is a friend's access or an open invite that nothing
-- else records. Refuses while any exists; terminal rows are history and go
-- with the table. The ACCESS EXCLUSIVE lock is taken BEFORE the check, so a
-- seat that commits while this file waits for the lock is seen.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (187) as dirty before it runs this file, while the file's implicit
-- transaction rolled back and the schema is still at 188. Confirm the table
-- exists, then `force 188` and boot. See [internal]rules/migrations.md.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_open BOOLEAN;
BEGIN
    IF to_regclass(format('%I.subscription_seats', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the check with seat writes and the drop.
    BEGIN
        EXECUTE format(
            'LOCK TABLE %I.subscription_seats IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent rollback removed it while this one waited for the lock.
            RETURN;
    END;

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.subscription_seats WHERE state IN (''pending'', ''active''))',
        migration_schema)
        INTO has_open;
    IF has_open THEN
        RAISE EXCEPTION
            'cannot drop subscription_seats while a Wing seat is pending or active'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format('DROP TABLE IF EXISTS %I.subscription_seats', migration_schema);
END
$$;
