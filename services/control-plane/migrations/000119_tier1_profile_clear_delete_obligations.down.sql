-- Refuse to discard unresolved ordinary profile-clear obligations during rollback.
-- Account-erasure tombstones are a separate permanent privacy rail.
DO $$
DECLARE
    has_pending BOOLEAN;
BEGIN
    IF to_regclass('public.tier1_profile_clear_delete_obligations') IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with inserts and DROP.
    BEGIN
        EXECUTE
            'LOCK TABLE public.tier1_profile_clear_delete_obligations IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent/idempotent rollback may have removed it while this
            -- rollback waited to acquire the lock.
            RETURN;
    END;

    EXECUTE
        'SELECT EXISTS (SELECT 1 FROM public.tier1_profile_clear_delete_obligations)'
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop tier1_profile_clear_delete_obligations while unresolved profile-clear obligations remain';
    END IF;

    EXECUTE 'DROP TABLE IF EXISTS public.tier1_profile_clear_delete_obligations';
END
$$;
