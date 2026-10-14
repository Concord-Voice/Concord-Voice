-- This receipt survives account and room cleanup. Its generation binds the
-- durable command stream and permanent owner record across process restarts.
CREATE TABLE voice_command_transport (
    id                     BOOLEAN NOT NULL DEFAULT TRUE,
    generation             UUID NOT NULL DEFAULT gen_random_uuid(),
    provisioned_at         TIMESTAMPTZ,
    stream_created_at      TEXT,
    owner_store_created_at TEXT,
    initializing_revision  BIGINT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT voice_command_transport_pkey PRIMARY KEY (id),
    CONSTRAINT voice_command_transport_id_check CHECK (id),
    CONSTRAINT voice_command_transport_generation_key UNIQUE (generation),
    CONSTRAINT voice_command_transport_generation_check
        CHECK (generation <> '00000000-0000-0000-0000-000000000000'::UUID),
    CONSTRAINT voice_command_transport_provisioning_proof_check CHECK (
        (
            provisioned_at IS NULL
            AND stream_created_at IS NULL
            AND owner_store_created_at IS NULL
            AND initializing_revision IS NULL
        ) OR (
            provisioned_at IS NOT NULL
            AND stream_created_at IS NOT NULL
            AND owner_store_created_at IS NOT NULL
            AND initializing_revision IS NOT NULL
            AND initializing_revision > 0
        )
    )
);

CREATE FUNCTION protect_voice_command_transport_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.generation IS DISTINCT FROM OLD.generation
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'voice command transport generation and creation time are immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;

    -- Once committed, this proof must not revert to first-time provisioning.
    IF OLD.provisioned_at IS NOT NULL
        AND (
            NEW.provisioned_at IS DISTINCT FROM OLD.provisioned_at
            OR NEW.stream_created_at IS DISTINCT FROM OLD.stream_created_at
            OR NEW.owner_store_created_at IS DISTINCT FROM OLD.owner_store_created_at
            OR NEW.initializing_revision IS DISTINCT FROM OLD.initializing_revision
        ) THEN
        RAISE EXCEPTION 'voice command transport provisioning receipt is immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;

    NEW.updated_at := statement_timestamp();
    RETURN NEW;
END;
$$;

CREATE TRIGGER protect_voice_command_transport_receipt
    BEFORE UPDATE ON voice_command_transport
    FOR EACH ROW
    EXECUTE FUNCTION protect_voice_command_transport_receipt();

INSERT INTO voice_command_transport (id) VALUES (TRUE);

COMMENT ON TABLE voice_command_transport IS
    'Permanent singleton binding durable voice command transport to one immutable generation.';
COMMENT ON COLUMN voice_command_transport.id IS
    'Singleton key; the only valid value is TRUE.';
COMMENT ON COLUMN voice_command_transport.generation IS
    'Immutable generation shared by the command stream and permanent owner record.';
COMMENT ON COLUMN voice_command_transport.provisioned_at IS
    'Committed proof that generation-bound broker resources were verified; NULL permits initial provisioning.';
COMMENT ON COLUMN voice_command_transport.stream_created_at IS
    'Exact broker stream creation fingerprint as canonical UTC RFC3339Nano text; TEXT preserves nanoseconds that TIMESTAMPTZ would round.';
COMMENT ON COLUMN voice_command_transport.owner_store_created_at IS
    'Exact owner KV backing-stream creation fingerprint as canonical UTC RFC3339Nano text; TEXT preserves broker nanoseconds.';
COMMENT ON COLUMN voice_command_transport.initializing_revision IS
    'Positive KV revision of the initializing owner record verified before the provisioning receipt committed.';
COMMENT ON COLUMN voice_command_transport.created_at IS
    'Immutable time the transport generation was created.';
COMMENT ON COLUMN voice_command_transport.updated_at IS
    'Database-maintained time the transport receipt was last updated.';
