-- Refuse a destructive rollback while committed Rich Presence cleanup evidence
-- remains unresolved. Operators must successfully retry those cleanups first.
DO $$
DECLARE
    has_pending BOOLEAN;
BEGIN
    IF to_regclass('public.activity_settings_pending_cleanups') IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with both INSERT and DROP. Without this
    -- lock, evidence can commit after SELECT EXISTS but before DROP TABLE.
    BEGIN
        EXECUTE
            'LOCK TABLE public.activity_settings_pending_cleanups IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent/idempotent rollback may have removed it while this
            -- rollback waited to acquire the lock.
            RETURN;
    END;

    EXECUTE
        'SELECT EXISTS (SELECT 1 FROM public.activity_settings_pending_cleanups)'
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop activity_settings_pending_cleanups while cleanup evidence remains';
    END IF;

    EXECUTE 'DROP TABLE IF EXISTS public.activity_settings_pending_cleanups';
END
$$;
