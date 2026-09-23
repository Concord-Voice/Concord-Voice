DO $$
DECLARE
    has_pending BOOLEAN;
BEGIN
    IF to_regclass('public.credential_epoch_voice_ejections') IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with inserts and the drop.  An obligation
    -- must never be silently discarded during a rollback.
    LOCK TABLE public.credential_epoch_voice_ejections IN ACCESS EXCLUSIVE MODE;
    SELECT EXISTS (
        SELECT 1 FROM public.credential_epoch_voice_ejections
    ) INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop credential_epoch_voice_ejections while delivery evidence remains';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_credential_epoch_voice_ejections_due;
DROP TABLE IF EXISTS public.credential_epoch_voice_ejections;
