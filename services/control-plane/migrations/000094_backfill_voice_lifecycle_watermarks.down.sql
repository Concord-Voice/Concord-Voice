-- The joined_at backfill cannot be distinguished from later lifecycle writes.
-- Refuse the phase rollback until both ephemeral participant tables drain.
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
            'cannot roll back migration 000094 from a partial lifecycle-watermark schema'
            USING ERRCODE = '55000';
    END IF;

    LOCK TABLE voice_participants IN ACCESS EXCLUSIVE MODE;
    LOCK TABLE dm_voice_participants IN ACCESS EXCLUSIVE MODE;

    IF EXISTS (SELECT 1 FROM voice_participants)
       OR EXISTS (SELECT 1 FROM dm_voice_participants) THEN
        RAISE EXCEPTION
            'cannot roll back migration 000094 while voice participants are active'
            USING ERRCODE = '55000';
    END IF;
END
$$;
