CREATE OR REPLACE FUNCTION renew_voice_participant_lifecycle_observed_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.lifecycle_observed_at := clock_timestamp();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
