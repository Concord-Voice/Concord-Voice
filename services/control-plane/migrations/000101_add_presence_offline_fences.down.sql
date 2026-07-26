-- Refuse a destructive rollback while active offline-denial fences remain.
-- Operators must first restore visible presence or otherwise resolve each fence.
DO $$
DECLARE
    has_fences BOOLEAN;
BEGIN
    IF to_regclass('public.presence_offline_fences') IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with fence writes and DROP TABLE.
    BEGIN
        EXECUTE
            'LOCK TABLE public.presence_offline_fences IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            RETURN;
    END;

    EXECUTE
        'SELECT EXISTS (SELECT 1 FROM public.presence_offline_fences)'
        INTO has_fences;
    IF has_fences THEN
        RAISE EXCEPTION
            'cannot drop presence_offline_fences while offline denial fences remain';
    END IF;

    EXECUTE 'DROP TABLE IF EXISTS public.presence_offline_fences';
END
$$;
