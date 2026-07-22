ALTER TABLE voice_participants
    ADD COLUMN lifecycle_event_at TIMESTAMPTZ;

ALTER TABLE voice_participants
    ALTER COLUMN lifecycle_event_at SET DEFAULT NOW();

COMMENT ON COLUMN voice_participants.lifecycle_event_at IS
    'Latest authoritative media-plane event time for this active server voice participant; prevents stale lifecycle transitions from replacing newer state.';

ALTER TABLE dm_voice_participants
    ADD COLUMN lifecycle_event_at TIMESTAMPTZ;

ALTER TABLE dm_voice_participants
    ALTER COLUMN lifecycle_event_at SET DEFAULT NOW();

COMMENT ON COLUMN dm_voice_participants.lifecycle_event_at IS
    'Latest authoritative media-plane event time for this active private-call participant; prevents stale lifecycle transitions from replacing newer state.';
