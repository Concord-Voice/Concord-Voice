-- Refuse a destructive rollback while theft-triggered revocations remain pending.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_pending BOOLEAN;
BEGIN
    IF to_regclass(format('%I.token_theft_pending_revocations', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    -- PostgreSQL cannot inspect the Redis-only fallback. The rollback operator
    -- must stop all recovery writers, verify both fallback namespaces are
    -- empty, then supply this connection-local clearance to the migrate tool.
    IF current_setting('concord.token_theft_redis_clearance', TRUE) IS DISTINCT FROM 'verified' THEN
        RAISE EXCEPTION
            'cannot drop token_theft_pending_revocations without certified Redis fallback clearance'
            USING ERRCODE = 'restrict_violation';
    END IF;

    BEGIN
        EXECUTE format(
            'LOCK TABLE %I.token_theft_pending_revocations IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    EXCEPTION
        WHEN undefined_table THEN
            RETURN;
    END;

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.token_theft_pending_revocations)',
        migration_schema)
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop token_theft_pending_revocations while theft revocations remain pending'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format('DROP TABLE IF EXISTS %I.token_theft_pending_revocations', migration_schema);
    EXECUTE format('ALTER TABLE %I.users DROP COLUMN IF EXISTS token_theft_generation', migration_schema);
END
$$;
