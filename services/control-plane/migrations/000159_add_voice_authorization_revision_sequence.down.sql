-- Never roll back a consumed watermark: dropping and recreating it could reuse
-- revision values and make stale authorization snapshots appear current.
DO $$
DECLARE
    sequence_called BOOLEAN;
BEGIN
    IF to_regclass('public.voice_authorization_revision_seq') IS NULL THEN
        RETURN;
    END IF;

    -- ALTER SEQUENCE takes the sequence lock supported by PostgreSQL 16 and
    -- serializes with concurrent nextval callers before inspecting is_called.
    ALTER SEQUENCE public.voice_authorization_revision_seq CACHE 1;
    SELECT is_called
      INTO sequence_called
      FROM public.voice_authorization_revision_seq;

    IF sequence_called THEN
        RAISE EXCEPTION
            'refusing to drop consumed voice_authorization_revision_seq; preserve the watermark';
    END IF;
END
$$;

DROP SEQUENCE IF EXISTS public.voice_authorization_revision_seq;
