-- Session evidence is security-critical and must not be discarded by a
-- rollback while any exact targets remain unresolved.
DO $$
DECLARE
    has_rows BOOLEAN;
BEGIN
    IF to_regclass('public.voice_enforcement_sessions') IS NOT NULL THEN
        -- Serialize the emptiness check with inserts and DROP TABLE.
        EXECUTE
            'LOCK TABLE public.voice_enforcement_sessions IN ACCESS EXCLUSIVE MODE';

        EXECUTE
            'SELECT EXISTS (SELECT 1 FROM public.voice_enforcement_sessions)'
            INTO has_rows;
        IF has_rows THEN
            RAISE EXCEPTION
                'cannot drop voice_enforcement_sessions while exact session evidence remains';
        END IF;
    END IF;

    IF to_regclass('public.voice_enforcement_rollout') IS NOT NULL THEN
        EXECUTE
            'LOCK TABLE public.voice_enforcement_rollout IN ACCESS EXCLUSIVE MODE';
        IF EXISTS (
            SELECT 1
            FROM public.voice_enforcement_rollout
            WHERE activated_at IS NOT NULL
        ) THEN
            RAISE EXCEPTION
                'cannot drop voice_enforcement_rollout after voice enforcement rollout activation';
        END IF;
    END IF;

    EXECUTE 'DROP TABLE IF EXISTS public.voice_enforcement_sessions';
    DROP FUNCTION IF EXISTS public.prevent_voice_enforcement_sessions_update();
    DROP TABLE IF EXISTS public.voice_enforcement_rollout;
END
$$;
