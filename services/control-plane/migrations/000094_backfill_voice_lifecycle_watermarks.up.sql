UPDATE voice_participants
SET lifecycle_event_at = joined_at
WHERE lifecycle_event_at IS NULL;

UPDATE dm_voice_participants
SET lifecycle_event_at = joined_at
WHERE lifecycle_event_at IS NULL;
