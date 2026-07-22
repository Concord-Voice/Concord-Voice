-- Restore the nullable phase while retaining non-null guards for future writes.
-- The column is already NOT NULL here, so existing rows need no validation scan.
DO $$
DECLARE
    has_voice_watermark BOOLEAN;
    has_dm_watermark BOOLEAN;
BEGIN
    SELECT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'voice_participants'
          AND column_name = 'lifecycle_event_at'
    ) INTO has_voice_watermark;
    SELECT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'dm_voice_participants'
          AND column_name = 'lifecycle_event_at'
    ) INTO has_dm_watermark;

    IF NOT has_voice_watermark AND NOT has_dm_watermark THEN
        RETURN;
    END IF;
    IF has_voice_watermark <> has_dm_watermark THEN
        RAISE EXCEPTION
            'cannot roll back migration 000097 from a partial lifecycle-watermark schema'
            USING ERRCODE = '55000';
    END IF;

END
$$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'voice_participants'
          AND column_name = 'lifecycle_event_at'
    ) THEN
        ALTER TABLE voice_participants
            DROP CONSTRAINT IF EXISTS voice_participants_lifecycle_event_at_not_null;
        ALTER TABLE voice_participants
            ADD CONSTRAINT voice_participants_lifecycle_event_at_not_null
            CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;
        ALTER TABLE voice_participants
            ALTER COLUMN lifecycle_event_at DROP NOT NULL;

        ALTER TABLE dm_voice_participants
            DROP CONSTRAINT IF EXISTS dm_voice_participants_lifecycle_event_at_not_null;
        ALTER TABLE dm_voice_participants
            ADD CONSTRAINT dm_voice_participants_lifecycle_event_at_not_null
            CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;
        ALTER TABLE dm_voice_participants
            ALTER COLUMN lifecycle_event_at DROP NOT NULL;
    END IF;
END
$$;
