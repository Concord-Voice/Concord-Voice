DO $$
BEGIN
    IF to_regclass('public.dm_block_voice_ejections') IS NULL THEN
        RETURN;
    END IF;

    LOCK TABLE public.dm_block_voice_ejections IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM public.dm_block_voice_ejections) THEN
        RAISE EXCEPTION
            'refusing to roll back dm_block_voice_ejection generation while delivery evidence remains';
    END IF;

    EXECUTE 'ALTER TABLE public.dm_block_voice_ejections DROP COLUMN IF EXISTS generation';
END
$$;
