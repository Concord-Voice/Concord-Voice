CREATE TABLE voice_enforcement_rollout (
    id           BOOLEAN NOT NULL DEFAULT TRUE,
    activated_at TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT voice_enforcement_rollout_pkey PRIMARY KEY (id),
    CONSTRAINT voice_enforcement_rollout_id_check CHECK (id)
);

INSERT INTO voice_enforcement_rollout (id, activated_at)
VALUES (TRUE, NULL);

COMMENT ON TABLE voice_enforcement_rollout IS
    'Singleton activation receipt proving all voice-enforcement nodes support durable session evidence.';
COMMENT ON COLUMN voice_enforcement_rollout.id IS
    'Singleton key; the only valid value is TRUE.';
COMMENT ON COLUMN voice_enforcement_rollout.activated_at IS
    'Time the rollout was activated after old media nodes stopped and new capability was ready; NULL means inactive.';
COMMENT ON COLUMN voice_enforcement_rollout.updated_at IS
    'Audit timestamp for activation receipt changes.';

-- Immutable evidence of the exact media sessions that a voice-enforcement
-- obligation must reach.  There are intentionally no foreign keys or expiry:
-- a hard-crashed node must not erase evidence needed for reconciliation.
CREATE TABLE voice_enforcement_sessions (
    session_generation UUID NOT NULL,
    node_boot_id       UUID NOT NULL,
    room_id            UUID NOT NULL,
    room_kind          TEXT NOT NULL,
    user_id            UUID NOT NULL,
    credential_epoch   TEXT NOT NULL,
    socket_id          TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT voice_enforcement_sessions_pkey
        PRIMARY KEY (session_generation),
    CONSTRAINT voice_enforcement_sessions_room_kind_check
        CHECK (room_kind IN ('dm', 'channel')),
    CONSTRAINT voice_enforcement_sessions_credential_epoch_check
        CHECK (credential_epoch = '' OR credential_epoch ~ '^[0-9a-f]{32}$'),
    CONSTRAINT voice_enforcement_sessions_socket_id_check
        CHECK (octet_length(socket_id) BETWEEN 1 AND 255),
    CONSTRAINT voice_enforcement_sessions_node_socket_key
        UNIQUE (node_boot_id, socket_id)
);

CREATE INDEX idx_voice_enforcement_sessions_room_user_generation
    ON voice_enforcement_sessions (room_kind, room_id, user_id, session_generation);

CREATE INDEX idx_voice_enforcement_sessions_user_epoch_generation
    ON voice_enforcement_sessions (user_id, credential_epoch, session_generation);

CREATE FUNCTION prevent_voice_enforcement_sessions_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'voice_enforcement_sessions rows are immutable'
        USING ERRCODE = 'restrict_violation';
    RETURN NULL;
END;
$$;

CREATE TRIGGER prevent_voice_enforcement_sessions_update
    BEFORE UPDATE ON voice_enforcement_sessions
    FOR EACH ROW
    EXECUTE FUNCTION prevent_voice_enforcement_sessions_update();

COMMENT ON TABLE voice_enforcement_sessions IS
    'Immutable exact media-session evidence for durable multi-node voice enforcement.';
COMMENT ON COLUMN voice_enforcement_sessions.session_generation IS
    'Unique generation identifying the exact admitted voice session.';
COMMENT ON COLUMN voice_enforcement_sessions.node_boot_id IS
    'Stable identifier for the media-node boot that owns the session.';
COMMENT ON COLUMN voice_enforcement_sessions.room_id IS
    'DM conversation or server channel containing the session.';
COMMENT ON COLUMN voice_enforcement_sessions.room_kind IS
    'Room namespace for room_id: dm or channel.';
COMMENT ON COLUMN voice_enforcement_sessions.user_id IS
    'Account identity attached to the exact media session.';
COMMENT ON COLUMN voice_enforcement_sessions.credential_epoch IS
    'Credential epoch observed at admission, or empty for a legacy session.';
COMMENT ON COLUMN voice_enforcement_sessions.socket_id IS
    'Socket.IO connection identifier scoped by node_boot_id.';
COMMENT ON COLUMN voice_enforcement_sessions.created_at IS
    'Time the exact session evidence was recorded.';
