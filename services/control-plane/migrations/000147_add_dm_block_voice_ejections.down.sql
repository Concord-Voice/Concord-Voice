DO $$
BEGIN
    IF to_regclass('public.dm_block_voice_ejections') IS NULL THEN
        RETURN;
    END IF;

    LOCK TABLE public.dm_block_voice_ejections IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM public.dm_block_voice_ejections) THEN
        RAISE EXCEPTION
            'refusing to roll back dm_block_voice_ejections while delivery evidence remains';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_dm_block_voice_ejections_due;
DROP TABLE IF EXISTS public.dm_block_voice_ejections;
