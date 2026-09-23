-- Durable, exact-generation media eviction obligations created by a committed
-- credential-epoch rotation.  No foreign key is intentional: account deletion
-- must not discard an eviction that still needs to reach the media plane.
CREATE TABLE credential_epoch_voice_ejections (
    user_id                    UUID NOT NULL,
    credential_epoch           TEXT NOT NULL,
    superseded_credential_epoch TEXT NOT NULL,
    generation                 UUID NOT NULL DEFAULT gen_random_uuid(),
    attempts                   INTEGER NOT NULL DEFAULT 0,
    failure_class              TEXT,
    reconcile_after            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT credential_epoch_voice_ejections_pkey
        PRIMARY KEY (user_id, superseded_credential_epoch),
    CONSTRAINT credential_epoch_voice_ejections_generation_key
        UNIQUE (generation),
    CONSTRAINT credential_epoch_voice_ejections_credential_epoch_check
        CHECK (credential_epoch ~ '^[0-9a-f]{32}$'),
    CONSTRAINT credential_epoch_voice_ejections_superseded_epoch_check
        CHECK (superseded_credential_epoch = ''
            OR superseded_credential_epoch ~ '^[0-9a-f]{32}$'),
    CONSTRAINT credential_epoch_voice_ejections_distinct_epoch_check
        CHECK (credential_epoch <> superseded_credential_epoch),
    CONSTRAINT credential_epoch_voice_ejections_attempts_check
        CHECK (attempts >= 0),
    CONSTRAINT credential_epoch_voice_ejections_failure_class_check
        CHECK (failure_class IS NULL OR failure_class = 'delivery'),
    CONSTRAINT credential_epoch_voice_ejections_updated_at_check
        CHECK (updated_at >= created_at),
    CONSTRAINT credential_epoch_voice_ejections_reconcile_after_check
        CHECK (reconcile_after >= created_at)
);

-- The table is empty when created, so a transactional due index is safe here.
CREATE INDEX idx_credential_epoch_voice_ejections_due
    ON credential_epoch_voice_ejections (reconcile_after, user_id, superseded_credential_epoch);

COMMENT ON TABLE credential_epoch_voice_ejections IS
    'Durable exact targets for at-least-once media eviction after credential-epoch rotation.';
COMMENT ON COLUMN credential_epoch_voice_ejections.user_id IS
    'Account whose media sessions must be evicted; intentionally has no foreign key so account deletion cannot erase the obligation.';
COMMENT ON COLUMN credential_epoch_voice_ejections.credential_epoch IS
    'New 32-character lowercase hexadecimal credential epoch that superseded the target epoch.';
COMMENT ON COLUMN credential_epoch_voice_ejections.superseded_credential_epoch IS
    'Exact old credential epoch to evict, or the empty legacy value for sessions without an epoch claim.';
COMMENT ON COLUMN credential_epoch_voice_ejections.generation IS
    'Fresh delivery generation used to fence stale acknowledgements and retries.';
COMMENT ON COLUMN credential_epoch_voice_ejections.attempts IS
    'Number of bounded media-eviction delivery attempts.';
COMMENT ON COLUMN credential_epoch_voice_ejections.failure_class IS
    'Closed diagnostic class for the most recent failed delivery; only delivery is recorded.';
COMMENT ON COLUMN credential_epoch_voice_ejections.reconcile_after IS
    'Earliest time at which a worker may claim the eviction obligation again.';
COMMENT ON COLUMN credential_epoch_voice_ejections.created_at IS
    'Time the durable credential-epoch eviction obligation was recorded.';
COMMENT ON COLUMN credential_epoch_voice_ejections.updated_at IS
    'Time the obligation or its retry metadata was last changed.';
