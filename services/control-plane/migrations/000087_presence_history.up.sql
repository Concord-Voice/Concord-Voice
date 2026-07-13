-- Activity History (#1235).
-- Storage is opt-in and self-only at the service layer. The database keeps the
-- category-neutral interval ledger bounded, internally consistent, and ready
-- for durable settings-write reconciliation.
ALTER TABLE user_presence_settings
    ADD COLUMN presence_settings_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN presence_settings_operation_id UUID,
    ADD COLUMN activity_history_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN activity_history_retention_days SMALLINT NOT NULL DEFAULT 30,
    ADD COLUMN activity_history_consent_version SMALLINT,
    ADD COLUMN activity_history_consent_copy_hash CHAR(64),
    ADD COLUMN activity_history_consented_at TIMESTAMPTZ,
    ADD COLUMN activity_history_reconsent_required BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE user_presence_settings
    ADD CONSTRAINT user_presence_settings_version_nonnegative
        CHECK (presence_settings_version >= 0),
    ADD CONSTRAINT user_presence_settings_history_retention_check
        CHECK (activity_history_retention_days IN (7, 30, 90, 365)),
    ADD CONSTRAINT user_presence_settings_history_hash_check
        CHECK (
            activity_history_consent_copy_hash IS NULL
            OR activity_history_consent_copy_hash ~ '^[0-9a-f]{64}$'
        ),
    ADD CONSTRAINT user_presence_settings_history_consent_check
        CHECK (
            (
                activity_history_enabled
                AND activity_history_consent_version IS NOT NULL
                AND activity_history_consent_version > 0
                AND activity_history_consent_copy_hash IS NOT NULL
                AND activity_history_consented_at IS NOT NULL
                AND NOT activity_history_reconsent_required
            )
            OR
            (
                NOT activity_history_enabled
                AND activity_history_consent_version IS NULL
                AND activity_history_consent_copy_hash IS NULL
                AND activity_history_consented_at IS NULL
            )
        );

CREATE TABLE presence_history (
    id              UUID        NOT NULL,
    sender_id       UUID        NOT NULL,
    category        TEXT        NOT NULL,
    payload_version SMALLINT    NOT NULL,
    payload         JSONB       NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ,
    recorded_at     TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    CONSTRAINT presence_history_pkey
        PRIMARY KEY (id),
    CONSTRAINT presence_history_sender_id_fkey
        FOREIGN KEY (sender_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT presence_history_category_check
        CHECK (category IN (
            'server_voice',
            'private_call',
            'games',
            'music',
            'streaming',
            'browser',
            'productivity',
            'creator',
            'custom_text'
        )),
    CONSTRAINT presence_history_payload_version_positive
        CHECK (payload_version > 0),
    CONSTRAINT presence_history_payload_object_check
        CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT presence_history_payload_size_check
        CHECK (octet_length(payload::TEXT) <= 4096),
    CONSTRAINT presence_history_ended_at_check
        CHECK (ended_at IS NULL OR ended_at >= started_at),
    CONSTRAINT presence_history_expires_at_check
        CHECK (expires_at > recorded_at)
);

CREATE UNIQUE INDEX idx_presence_history_one_open
    ON presence_history (sender_id, category)
    WHERE ended_at IS NULL;

CREATE INDEX idx_presence_history_sender_page
    ON presence_history (sender_id, recorded_at DESC, id DESC);

CREATE INDEX idx_presence_history_expiry
    ON presence_history (expires_at, id);

CREATE TABLE presence_settings_pending_operations (
    user_id                UUID        NOT NULL,
    operation_id           UUID        NOT NULL,
    prior_settings_version BIGINT      NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    reconcile_after        TIMESTAMPTZ NOT NULL DEFAULT (clock_timestamp() + INTERVAL '30 seconds'),
    CONSTRAINT presence_settings_pending_operations_pkey
        PRIMARY KEY (user_id),
    CONSTRAINT presence_settings_pending_operations_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT presence_settings_pending_operations_operation_id_key
        UNIQUE (operation_id),
    CONSTRAINT presence_settings_pending_operations_prior_version_check
        CHECK (prior_settings_version >= 0),
    CONSTRAINT presence_settings_pending_operations_reconcile_check
        CHECK (reconcile_after > created_at)
);
