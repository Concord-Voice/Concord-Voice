DROP TRIGGER IF EXISTS voice_participants_renew_lifecycle_observed_at
    ON voice_participants;
DROP FUNCTION IF EXISTS renew_voice_participant_lifecycle_observed_at();
DROP INDEX IF EXISTS idx_voice_participants_lifecycle_observed_at;
ALTER TABLE voice_participants
    DROP COLUMN IF EXISTS lifecycle_observed_at;
