CREATE OR REPLACE FUNCTION renew_voice_participant_lifecycle_observed_at()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT'
       OR NEW.lifecycle_event_at IS DISTINCT FROM OLD.lifecycle_event_at THEN
        NEW.lifecycle_observed_at := clock_timestamp();
    ELSE
        NEW.lifecycle_observed_at := OLD.lifecycle_observed_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
