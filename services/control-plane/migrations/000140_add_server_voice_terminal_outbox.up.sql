-- Durable obligations for retrying stale Server Voice terminal events.
-- Channel, server, and participant rows are deliberately not foreign-key parents:
-- ordinary cleanup must leave this obligation deliverable. The user reference does
-- cascade so account erasure cannot retain or later broadcast that identifier.
CREATE TABLE server_voice_terminal_outbox (
    channel_id       UUID        NOT NULL,
    user_id          UUID        NOT NULL,
    server_id        UUID        NOT NULL,
    operation_id     UUID        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    reconcile_after  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    CONSTRAINT server_voice_terminal_outbox_pkey
        PRIMARY KEY (channel_id, user_id),
    CONSTRAINT server_voice_terminal_outbox_operation_id_key
        UNIQUE (operation_id),
    CONSTRAINT server_voice_terminal_outbox_reconcile_after_check
        CHECK (reconcile_after >= created_at),
    CONSTRAINT server_voice_terminal_outbox_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX idx_server_voice_terminal_outbox_due
    ON server_voice_terminal_outbox (reconcile_after, created_at, channel_id, user_id);

COMMENT ON TABLE server_voice_terminal_outbox IS
    'Durable Server Voice terminal obligations captured when stale participant rows are deleted; rows remain until successor-safe left delivery settles.';
COMMENT ON COLUMN server_voice_terminal_outbox.channel_id IS
    'Channel whose stale Server Voice membership was removed.';
COMMENT ON COLUMN server_voice_terminal_outbox.user_id IS
    'User whose stale Server Voice membership was removed; account erasure cascades the obligation.';
COMMENT ON COLUMN server_voice_terminal_outbox.server_id IS
    'Server scope required to construct the existing terminal voice-state frame without a parent-row lookup.';
COMMENT ON COLUMN server_voice_terminal_outbox.operation_id IS
    'Opaque operation token used to replace stale obligations and acknowledge only the captured generation.';
COMMENT ON COLUMN server_voice_terminal_outbox.created_at IS
    'Time at which the terminal obligation was recorded.';
COMMENT ON COLUMN server_voice_terminal_outbox.reconcile_after IS
    'Earliest time at which the obligation is eligible for another delivery attempt.';
