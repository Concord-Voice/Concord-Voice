-- Exact media-disconnect payloads are external-effect obligations. They remain
-- durable after the block marker, DM conversation, or user row is removed.
CREATE TABLE dm_block_voice_ejections (
    conversation_id UUID NOT NULL,
    user_id          UUID NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    failure_class    TEXT,
    reconcile_after  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT dm_block_voice_ejections_pkey
        PRIMARY KEY (conversation_id, user_id),
    CONSTRAINT dm_block_voice_ejections_attempts_check
        CHECK (attempts >= 0),
    CONSTRAINT dm_block_voice_ejections_failure_class_check
        CHECK (failure_class IS NULL OR failure_class = 'delivery'),
    CONSTRAINT dm_block_voice_ejections_updated_at_check
        CHECK (updated_at >= created_at)
);

-- The table is empty when created, so a transactional index is safe here.
CREATE INDEX idx_dm_block_voice_ejections_due
    ON dm_block_voice_ejections (reconcile_after, conversation_id, user_id);

COMMENT ON TABLE dm_block_voice_ejections IS
    'Durable exact targets for at-least-once DM media disconnect publication after membership removal.';
COMMENT ON COLUMN dm_block_voice_ejections.conversation_id IS
    'DM conversation identifier sent to the media-plane disconnect terminal; intentionally has no foreign key so conversation deletion cannot erase the obligation.';
COMMENT ON COLUMN dm_block_voice_ejections.user_id IS
    'Removed participant identifier sent to the media-plane disconnect terminal; intentionally has no foreign key so account erasure cannot erase the obligation.';
COMMENT ON COLUMN dm_block_voice_ejections.attempts IS
    'Number of bounded disconnect publication attempts.';
COMMENT ON COLUMN dm_block_voice_ejections.failure_class IS
    'Closed diagnostic class for the most recent failed disconnect publication.';
COMMENT ON COLUMN dm_block_voice_ejections.reconcile_after IS
    'Earliest time at which a worker may claim the disconnect target again.';
COMMENT ON COLUMN dm_block_voice_ejections.created_at IS
    'Time the membership-removal transaction first recorded the disconnect target.';
COMMENT ON COLUMN dm_block_voice_ejections.updated_at IS
    'Time the target claim or failure metadata last changed.';
