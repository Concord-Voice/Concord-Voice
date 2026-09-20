ALTER TABLE dm_participants
    ADD COLUMN hidden_at TIMESTAMPTZ;

ALTER TABLE dm_message_hidden_ranges
    ADD COLUMN includes_own BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN dm_participants.hidden_at IS
    'Per-participant DM list hide timestamp; later messages clear it.';

COMMENT ON COLUMN dm_message_hidden_ranges.includes_own IS
    'Whether this participant-private range also hides their own messages.';
