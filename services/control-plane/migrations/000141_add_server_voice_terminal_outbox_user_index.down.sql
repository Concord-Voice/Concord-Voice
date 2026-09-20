DO $$
BEGIN
    IF to_regclass('public.server_voice_terminal_outbox') IS NULL THEN
        RETURN;
    END IF;
    BEGIN
        EXECUTE
            'LOCK TABLE public.server_voice_terminal_outbox IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            RETURN;
    END;
    IF EXISTS (SELECT 1 FROM public.server_voice_terminal_outbox) THEN
        RETURN;
    END IF;
    DROP INDEX IF EXISTS idx_server_voice_terminal_outbox_user;
END
$$;
