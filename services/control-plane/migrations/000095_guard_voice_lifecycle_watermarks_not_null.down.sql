ALTER TABLE voice_participants
    DROP CONSTRAINT IF EXISTS voice_participants_lifecycle_event_at_not_null;

ALTER TABLE dm_voice_participants
    DROP CONSTRAINT IF EXISTS dm_voice_participants_lifecycle_event_at_not_null;
