ALTER TABLE voice_participants
    ADD CONSTRAINT voice_participants_lifecycle_event_at_not_null
    CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;

ALTER TABLE dm_voice_participants
    ADD CONSTRAINT dm_voice_participants_lifecycle_event_at_not_null
    CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;
