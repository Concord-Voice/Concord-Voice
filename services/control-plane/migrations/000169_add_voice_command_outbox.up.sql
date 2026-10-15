-- Allocation updates one locked row in the authoritative transaction. A normal
-- PostgreSQL sequence would allow committed gaps and commit-order inversions.
CREATE TABLE voice_command_sequence (
    id                      BOOLEAN NOT NULL DEFAULT TRUE,
    last_allocated_sequence BIGINT NOT NULL DEFAULT 0,
    last_published_sequence BIGINT NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT voice_command_sequence_pkey PRIMARY KEY (id),
    CONSTRAINT voice_command_sequence_id_check CHECK (id),
    CONSTRAINT voice_command_sequence_allocated_check CHECK (last_allocated_sequence >= 0),
    CONSTRAINT voice_command_sequence_published_check CHECK (
        last_published_sequence >= 0
        AND last_published_sequence <= last_allocated_sequence
    )
);

INSERT INTO voice_command_sequence (id) VALUES (TRUE);

-- Typed command evidence survives account, room and registry cleanup. Only the
-- permanent transport generation is a foreign key; no domain deletion cascades.
CREATE TABLE voice_command_outbox (
    sequence               BIGINT NOT NULL,
    command_id             UUID NOT NULL,
    stream_generation      UUID NOT NULL,
    envelope_version       INTEGER NOT NULL DEFAULT 1,
    scope                  TEXT NOT NULL,
    kind                   TEXT NOT NULL,
    action                 TEXT NOT NULL,
    channel_id             UUID,
    server_id              UUID,
    user_id                UUID NOT NULL,
    session_generation     UUID,
    node_boot_id           UUID,
    socket_id              TEXT,
    credential_epoch       TEXT,
    reason                 TEXT,
    authorization_revision BIGINT,
    server_muted           BOOLEAN,
    server_deafened         BOOLEAN,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_token            UUID,
    lease_until            TIMESTAMPTZ,
    attempts               INTEGER NOT NULL DEFAULT 0,
    retry_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    failure_class          TEXT,
    published_at           TIMESTAMPTZ,
    broker_sequence        BIGINT,

    CONSTRAINT voice_command_outbox_pkey PRIMARY KEY (sequence),
    CONSTRAINT voice_command_outbox_command_id_key UNIQUE (command_id),
    CONSTRAINT voice_command_outbox_generation_fkey
        FOREIGN KEY (stream_generation)
        REFERENCES voice_command_transport (generation) ON DELETE RESTRICT,
    CONSTRAINT voice_command_outbox_sequence_check CHECK (sequence > 0),
    CONSTRAINT voice_command_outbox_version_check CHECK (envelope_version = 1),
    CONSTRAINT voice_command_outbox_scope_check CHECK (scope IN ('channel', 'dm')),
    CONSTRAINT voice_command_outbox_kind_check
        CHECK (kind IN ('server_moderation', 'user_moderation', 'disconnect')),
    CONSTRAINT voice_command_outbox_action_check
        CHECK (action IN ('mute', 'unmute', 'deafen', 'undeafen', 'disconnect')),
    CONSTRAINT voice_command_outbox_target_check CHECK (
        (
            session_generation IS NULL
            AND node_boot_id IS NULL
            AND socket_id IS NULL
            AND credential_epoch IS NULL
        ) OR (
            session_generation IS NOT NULL
            AND node_boot_id IS NOT NULL
            AND socket_id IS NOT NULL
            AND credential_epoch IS NOT NULL
            AND channel_id IS NOT NULL
            AND octet_length(socket_id) BETWEEN 1 AND 128
            -- PostgreSQL TEXT already excludes NUL; these are the other C0/C1 controls.
            AND socket_id COLLATE "C" !~ U&'[\0001-\001F\007F-\009F]'
            AND (
                credential_epoch = ''
                OR credential_epoch COLLATE "C" ~ '^[0-9a-f]{32}$'
            )
        )
    ),
    -- Presence guards are explicit: SQL CHECK accepts UNKNOWN, so nullable
    -- snapshot/reason fields must never satisfy a required branch through NULL.
    CONSTRAINT voice_command_outbox_envelope_check CHECK (
        (
            kind = 'server_moderation'
            AND action IN ('mute', 'unmute', 'deafen', 'undeafen')
            AND reason IS NULL
            AND authorization_revision IS NOT NULL
            AND authorization_revision > 0
            AND server_muted IS NOT NULL
            AND server_deafened IS NOT NULL
            AND (NOT server_deafened OR server_muted)
            AND (
                (scope = 'channel' AND server_id IS NOT NULL)
                OR (scope = 'dm' AND channel_id IS NOT NULL AND server_id IS NULL)
            )
        ) OR (
            kind = 'user_moderation'
            AND action IN ('mute', 'deafen')
            AND session_generation IS NOT NULL
            AND server_id IS NULL
            AND reason IS NULL
            AND authorization_revision IS NULL
            AND server_muted IS NULL
            AND server_deafened IS NULL
        ) OR (
            kind = 'disconnect'
            AND action = 'disconnect'
            AND server_id IS NULL
            AND reason IS NOT NULL
            AND reason IN ('membership_removed', 'moderator_request')
            AND authorization_revision IS NULL
            AND server_muted IS NULL
            AND server_deafened IS NULL
            AND (
                session_generation IS NOT NULL
                OR (scope = 'dm' AND channel_id IS NOT NULL AND reason = 'membership_removed')
            )
        )
    ),
    CONSTRAINT voice_command_outbox_lease_check CHECK (
        (lease_token IS NULL AND lease_until IS NULL)
        OR (lease_token IS NOT NULL AND lease_until IS NOT NULL)
    ),
    CONSTRAINT voice_command_outbox_attempts_check CHECK (attempts >= 0),
    CONSTRAINT voice_command_outbox_failure_class_check CHECK (
        failure_class IS NULL
        OR failure_class IN (
            'transport_unavailable', 'broker_unavailable', 'broker_continuity',
            'invalid_command', 'receipt_unavailable'
        )
    ),
    CONSTRAINT voice_command_outbox_receipt_check CHECK (
        (published_at IS NULL AND broker_sequence IS NULL)
        OR (
            published_at IS NOT NULL
            AND broker_sequence IS NOT NULL
            AND broker_sequence = sequence
        )
    )
);

-- All indexes are on the brand-new empty table in this migration transaction.
-- This supports the restrictive generation FK without locking populated tables.
CREATE INDEX voice_command_outbox_generation_idx
    ON voice_command_outbox (stream_generation);

CREATE FUNCTION protect_voice_command_outbox_envelope()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF ROW(
        NEW.sequence, NEW.command_id, NEW.stream_generation, NEW.envelope_version,
        NEW.scope, NEW.kind, NEW.action, NEW.channel_id, NEW.server_id, NEW.user_id,
        NEW.session_generation, NEW.node_boot_id, NEW.socket_id, NEW.credential_epoch,
        NEW.reason, NEW.authorization_revision, NEW.server_muted, NEW.server_deafened,
        NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.sequence, OLD.command_id, OLD.stream_generation, OLD.envelope_version,
        OLD.scope, OLD.kind, OLD.action, OLD.channel_id, OLD.server_id, OLD.user_id,
        OLD.session_generation, OLD.node_boot_id, OLD.socket_id, OLD.credential_epoch,
        OLD.reason, OLD.authorization_revision, OLD.server_muted, OLD.server_deafened,
        OLD.created_at
    ) THEN
        RAISE EXCEPTION 'voice command outbox envelope is immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER protect_voice_command_outbox_envelope
    BEFORE UPDATE ON voice_command_outbox
    FOR EACH ROW
    EXECUTE FUNCTION protect_voice_command_outbox_envelope();

COMMENT ON TABLE voice_command_sequence IS
    'Permanent singleton cursors for transactional command allocation and ordered publication.';
COMMENT ON COLUMN voice_command_sequence.id IS
    'Singleton key; the only valid value is TRUE.';
COMMENT ON COLUMN voice_command_sequence.last_allocated_sequence IS
    'Highest committed command sequence; allocation updates this row within the authoritative transaction.';
COMMENT ON COLUMN voice_command_sequence.last_published_sequence IS
    'Highest contiguous command sequence with a committed verified broker receipt.';
COMMENT ON COLUMN voice_command_sequence.created_at IS
    'Time the singleton command cursors were created.';
COMMENT ON COLUMN voice_command_sequence.updated_at IS
    'Time an allocator or publisher last updated the command cursors.';

COMMENT ON TABLE voice_command_outbox IS
    'Retained immutable voice-enforcement commands and their mutable delivery evidence.';
COMMENT ON COLUMN voice_command_outbox.sequence IS
    'Positive immutable command position allocated transactionally in commit order.';
COMMENT ON COLUMN voice_command_outbox.command_id IS
    'Immutable command identity used for broker deduplication and exact receipt recovery.';
COMMENT ON COLUMN voice_command_outbox.stream_generation IS
    'Permanent transport generation that must match the command stream receipt.';
COMMENT ON COLUMN voice_command_outbox.envelope_version IS
    'Frozen version of the closed typed voice-enforcement envelope.';
COMMENT ON COLUMN voice_command_outbox.scope IS
    'Frozen channel or private-call command scope.';
COMMENT ON COLUMN voice_command_outbox.kind IS
    'Frozen persistent snapshot, transient moderation, or disconnect command kind.';
COMMENT ON COLUMN voice_command_outbox.action IS
    'Frozen moderation action or disconnect intent.';
COMMENT ON COLUMN voice_command_outbox.channel_id IS
    'Frozen room identity; NULL is allowed for an offline channel-scope snapshot.';
COMMENT ON COLUMN voice_command_outbox.server_id IS
    'Frozen server identity required only for channel-scope persistent snapshots.';
COMMENT ON COLUMN voice_command_outbox.user_id IS
    'Frozen subject identity retained independently of the account lifetime.';
COMMENT ON COLUMN voice_command_outbox.session_generation IS
    'Captured exact session generation; NULL denotes a permitted offline marker.';
COMMENT ON COLUMN voice_command_outbox.node_boot_id IS
    'Captured process birth for the exact target, present only with every target field.';
COMMENT ON COLUMN voice_command_outbox.socket_id IS
    'Captured bounded exact socket identity, retained without control characters.';
COMMENT ON COLUMN voice_command_outbox.credential_epoch IS
    'Captured credential epoch; empty preserves legacy registry evidence, NULL denotes no target.';
COMMENT ON COLUMN voice_command_outbox.reason IS
    'Closed disconnect reason, absent from other command kinds.';
COMMENT ON COLUMN voice_command_outbox.authorization_revision IS
    'Positive independently allocated authorization revision for a complete persistent snapshot.';
COMMENT ON COLUMN voice_command_outbox.server_muted IS
    'Complete persistent mute state; NULL for transient commands.';
COMMENT ON COLUMN voice_command_outbox.server_deafened IS
    'Complete persistent deafen state; a deafened snapshot also requires mute.';
COMMENT ON COLUMN voice_command_outbox.created_at IS
    'Immutable database time the command obligation was captured.';
COMMENT ON COLUMN voice_command_outbox.lease_token IS
    'Claim identity for conditional delivery updates; present together with lease expiry.';
COMMENT ON COLUMN voice_command_outbox.lease_until IS
    'Database-clock expiry for the current short-lived publisher claim.';
COMMENT ON COLUMN voice_command_outbox.attempts IS
    'Bounded nonnegative delivery-attempt count; retries never discard the obligation.';
COMMENT ON COLUMN voice_command_outbox.retry_at IS
    'Earliest database time the publication head may be retried.';
COMMENT ON COLUMN voice_command_outbox.failure_class IS
    'Closed delivery-failure category without raw error or payload data.';
COMMENT ON COLUMN voice_command_outbox.published_at IS
    'Committed time of the verified broker receipt, present together with broker sequence.';
COMMENT ON COLUMN voice_command_outbox.broker_sequence IS
    'Verified broker stream position, required to equal the immutable command sequence.';
