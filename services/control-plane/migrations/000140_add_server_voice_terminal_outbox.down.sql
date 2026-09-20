-- Refuse to discard an undelivered Server Voice terminal obligation during rollback.
DO $$
DECLARE
    has_pending BOOLEAN;
BEGIN
    IF to_regclass('public.server_voice_terminal_outbox') IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with both INSERT and DROP.
    BEGIN
        EXECUTE
            'LOCK TABLE public.server_voice_terminal_outbox IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            -- An idempotent/concurrent rollback may have removed it while this
            -- rollback waited to acquire the lock.
            RETURN;
    END;

    EXECUTE
        'SELECT EXISTS (SELECT 1 FROM public.server_voice_terminal_outbox)'
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop server_voice_terminal_outbox while undelivered Server Voice terminal obligations remain';
    END IF;

    EXECUTE 'DROP TABLE IF EXISTS public.server_voice_terminal_outbox';
END
$$;
