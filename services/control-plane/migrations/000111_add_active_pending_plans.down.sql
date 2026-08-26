-- Refuse a destructive rollback while unresolved active-category reconciliation
-- obligations remain. Each surviving row is a retraction that was owed and never
-- delivered; dropping the table discards the only durable record of it.
--
-- Operator procedure: deploy the previous binary (which stops capturing), let
-- the reconciler drain, then roll back.
DO $$
DECLARE
    has_pending BOOLEAN;
BEGIN
    IF to_regclass('public.presence_active_pending_plans') IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with both INSERT and DROP. Without this
    -- lock, a plan can commit after SELECT EXISTS but before DROP TABLE.
    BEGIN
        EXECUTE
            'LOCK TABLE public.presence_active_pending_plans IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent/idempotent rollback may have removed it while this
            -- rollback waited to acquire the lock.
            RETURN;
    END;

    EXECUTE
        'SELECT EXISTS (SELECT 1 FROM public.presence_active_pending_plans)'
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop presence_active_pending_plans while unresolved active-category reconciliation obligations remain';
    END IF;

    EXECUTE 'DROP TABLE IF EXISTS public.presence_active_pending_plans';
END
$$;
