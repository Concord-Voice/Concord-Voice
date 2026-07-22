-- Restore the preceding phase's unvalidated guards without scanning either
-- participant table while ACCESS EXCLUSIVE is held.
ALTER TABLE voice_participants
    DROP CONSTRAINT IF EXISTS voice_participants_lifecycle_event_at_not_null;
ALTER TABLE voice_participants
    ADD CONSTRAINT voice_participants_lifecycle_event_at_not_null
    CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;

ALTER TABLE dm_voice_participants
    DROP CONSTRAINT IF EXISTS dm_voice_participants_lifecycle_event_at_not_null;
ALTER TABLE dm_voice_participants
    ADD CONSTRAINT dm_voice_participants_lifecycle_event_at_not_null
    CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;
