CREATE TABLE activity_settings_pending_cleanups (
    user_id      UUID        NOT NULL,
    operation_id UUID        NOT NULL,
    evidence     JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT activity_settings_pending_cleanups_pkey
        PRIMARY KEY (user_id),
    CONSTRAINT activity_settings_pending_cleanups_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT,
    CONSTRAINT activity_settings_pending_cleanups_operation_id_key
        UNIQUE (operation_id),
    CONSTRAINT activity_settings_pending_cleanups_evidence_object_check
        CHECK (jsonb_typeof(evidence) = 'object'),
    CONSTRAINT activity_settings_pending_cleanups_updated_at_check
        CHECK (updated_at >= created_at)
);

COMMENT ON TABLE activity_settings_pending_cleanups IS
    'Durable policy evidence and suppression receipts for retrying incomplete Rich Presence settings cleanup.';
COMMENT ON COLUMN activity_settings_pending_cleanups.user_id IS
    'Settings owner whose committed activity-policy cleanup remains incomplete.';
COMMENT ON COLUMN activity_settings_pending_cleanups.operation_id IS
    'Exact committed presence-settings operation that created this cleanup obligation.';
COMMENT ON COLUMN activity_settings_pending_cleanups.evidence IS
    'Versioned JSON object containing original before/after policy evidence until suppression, then a durable suppression receipt.';
COMMENT ON COLUMN activity_settings_pending_cleanups.created_at IS
    'Time the cleanup obligation was committed with the settings write.';
COMMENT ON COLUMN activity_settings_pending_cleanups.updated_at IS
    'Last time the cleanup obligation evidence or suppression receipt was updated.';
