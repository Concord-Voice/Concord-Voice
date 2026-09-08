ALTER TABLE voice_participants
    ADD COLUMN lifecycle_observed_at TIMESTAMPTZ NOT NULL
    DEFAULT CURRENT_TIMESTAMP;

COMMENT ON COLUMN voice_participants.lifecycle_observed_at IS
    'PostgreSQL observation time of the latest participant lifecycle mutation; drives stale-row lease expiry independently of producer-supplied lifecycle_event_at.';

CREATE OR REPLACE FUNCTION renew_voice_participant_lifecycle_observed_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.lifecycle_observed_at := clock_timestamp();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER voice_participants_renew_lifecycle_observed_at
    BEFORE INSERT OR UPDATE OF lifecycle_event_at ON voice_participants
    FOR EACH ROW
    EXECUTE FUNCTION renew_voice_participant_lifecycle_observed_at();

CREATE INDEX idx_voice_participants_lifecycle_observed_at
    ON voice_participants (lifecycle_observed_at, channel_id, user_id);
