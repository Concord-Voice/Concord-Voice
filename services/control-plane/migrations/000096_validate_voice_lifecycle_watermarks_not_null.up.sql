ALTER TABLE voice_participants
    VALIDATE CONSTRAINT voice_participants_lifecycle_event_at_not_null;

ALTER TABLE dm_voice_participants
    VALIDATE CONSTRAINT dm_voice_participants_lifecycle_event_at_not_null;
