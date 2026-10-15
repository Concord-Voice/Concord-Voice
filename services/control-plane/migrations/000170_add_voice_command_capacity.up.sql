-- Acknowledged broker history may be removed only through a durable proof.
-- The new proof table starts empty, so an unused transport gains no generation
-- reference that would prevent its existing first-time provisioning behavior.
CREATE TABLE voice_command_retirement (
    intent_id                UUID NOT NULL,
    from_sequence            BIGINT NOT NULL,
    through_sequence         BIGINT NOT NULL,
    published_sequence       BIGINT NOT NULL,
    stream_generation        UUID NOT NULL,
    stream_created_at        TEXT NOT NULL,
    owner_store_created_at   TEXT NOT NULL,
    initializing_revision    BIGINT NOT NULL,
    consumer_stream          TEXT NOT NULL,
    consumer_name            TEXT NOT NULL,
    consumer_durable         TEXT NOT NULL,
    consumer_created_at      TEXT NOT NULL,
    consumer_profile_version INTEGER NOT NULL,
    acknowledged_sequence    BIGINT NOT NULL,
    prepared_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at             TIMESTAMPTZ,
    attempts                 INTEGER NOT NULL DEFAULT 0,
    retry_at                 TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    failure_class            TEXT,

    CONSTRAINT voice_command_retirement_pkey PRIMARY KEY (intent_id),
    CONSTRAINT voice_command_retirement_intent_check
        CHECK (intent_id <> '00000000-0000-0000-0000-000000000000'::UUID),
    CONSTRAINT voice_command_retirement_from_key UNIQUE (from_sequence),
    CONSTRAINT voice_command_retirement_through_key UNIQUE (through_sequence),
    CONSTRAINT voice_command_retirement_generation_fkey
        FOREIGN KEY (stream_generation)
        REFERENCES voice_command_transport (generation) ON DELETE RESTRICT,
    CONSTRAINT voice_command_retirement_interval_check CHECK (
        from_sequence >= 0
        AND through_sequence > from_sequence
        AND published_sequence > through_sequence
        AND acknowledged_sequence >= through_sequence
    ),
    CONSTRAINT voice_command_retirement_resource_birth_check CHECK (
        stream_created_at COLLATE "C" ~
            '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$'
        AND owner_store_created_at COLLATE "C" ~
            '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$'
        AND consumer_created_at COLLATE "C" ~
            '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$'
    ),
    CONSTRAINT voice_command_retirement_initializing_revision_check
        CHECK (initializing_revision > 0),
    CONSTRAINT voice_command_retirement_consumer_check CHECK (
        consumer_stream = 'VOICE_COMMANDS_V1'
        AND consumer_name = 'voice-enforcement-v1'
        AND consumer_durable = 'voice-enforcement-v1'
        AND consumer_profile_version = 1
    ),
    CONSTRAINT voice_command_retirement_completion_check
        CHECK (completed_at IS NULL OR completed_at >= prepared_at),
    CONSTRAINT voice_command_retirement_attempts_check CHECK (attempts >= 0),
    CONSTRAINT voice_command_retirement_failure_class_check CHECK (
        failure_class IS NULL
        OR failure_class IN (
            'transport_unavailable', 'broker_unavailable', 'broker_continuity',
            'invalid_command', 'receipt_unavailable'
        )
    )
);

-- This index is created while the new proof table is empty.
CREATE INDEX voice_command_retirement_generation_idx
    ON voice_command_retirement (stream_generation);

CREATE FUNCTION protect_voice_command_retirement_proof()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'voice command retirement proof cannot be deleted'
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF OLD.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'completed voice command retirement proof is immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF ROW(
        NEW.intent_id, NEW.from_sequence, NEW.through_sequence,
        NEW.published_sequence, NEW.stream_generation, NEW.stream_created_at,
        NEW.owner_store_created_at, NEW.initializing_revision,
        NEW.consumer_stream, NEW.consumer_name, NEW.consumer_durable,
        NEW.consumer_created_at, NEW.consumer_profile_version,
        NEW.acknowledged_sequence, NEW.prepared_at
    ) IS DISTINCT FROM ROW(
        OLD.intent_id, OLD.from_sequence, OLD.through_sequence,
        OLD.published_sequence, OLD.stream_generation, OLD.stream_created_at,
        OLD.owner_store_created_at, OLD.initializing_revision,
        OLD.consumer_stream, OLD.consumer_name, OLD.consumer_durable,
        OLD.consumer_created_at, OLD.consumer_profile_version,
        OLD.acknowledged_sequence, OLD.prepared_at
    ) THEN
        RAISE EXCEPTION 'prepared voice command retirement proof is immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;

    -- A pending proof may update only retry metadata and complete once.
    RETURN NEW;
END;
$$;

CREATE TRIGGER protect_voice_command_retirement_proof
    BEFORE UPDATE OR DELETE ON voice_command_retirement
    FOR EACH ROW
    EXECUTE FUNCTION protect_voice_command_retirement_proof();

-- This existing table contains only its enforced singleton. Adding and checking
-- these columns therefore requires no scan of the permanent command history.
ALTER TABLE voice_command_sequence
    ADD COLUMN retired_through_sequence BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN pending_retirement_id UUID,
    ADD CONSTRAINT voice_command_sequence_retired_check CHECK (
        retired_through_sequence >= 0
        AND (
            retired_through_sequence = 0
            OR retired_through_sequence < last_published_sequence
        )
    ),
    ADD CONSTRAINT voice_command_sequence_pending_retirement_fkey
        FOREIGN KEY (pending_retirement_id)
        REFERENCES voice_command_retirement (intent_id) ON DELETE RESTRICT;

-- Attribution is temporary operational accounting. It has no account FK and is
-- removed atomically with each confirmed SQL publication receipt, while the
-- immutable outbox command remains. New indexes are built before the backfill.
CREATE TABLE voice_command_admission (
    sequence BIGINT NOT NULL,
    actor_id UUID NOT NULL,

    CONSTRAINT voice_command_admission_pkey PRIMARY KEY (sequence),
    CONSTRAINT voice_command_admission_command_fkey
        FOREIGN KEY (sequence)
        REFERENCES voice_command_outbox (sequence) ON DELETE RESTRICT,
    CONSTRAINT voice_command_admission_actor_check
        CHECK (actor_id <> '00000000-0000-0000-0000-000000000000'::UUID)
);

CREATE INDEX voice_command_admission_actor_idx
    ON voice_command_admission (actor_id);

-- Historical initiators are unknown; charge the retained target as the library
-- fallback. Preserve an existing queue even if it exceeds the new admission
-- limits. New work must wait for that grandfathered queue to drain.
INSERT INTO voice_command_admission (sequence, actor_id)
SELECT sequence, user_id
FROM voice_command_outbox
WHERE published_at IS NULL;

COMMENT ON TABLE voice_command_retirement IS
    'Permanent acknowledgement proof authorizing one exact broker prefix removal without deleting SQL command evidence.';
COMMENT ON COLUMN voice_command_retirement.intent_id IS
    'Immutable identity used to resume and complete the same prepared retirement after uncertain outcomes.';
COMMENT ON COLUMN voice_command_retirement.from_sequence IS
    'Completed retirement floor immediately before this proof was prepared.';
COMMENT ON COLUMN voice_command_retirement.through_sequence IS
    'Highest acknowledged broker sequence this exact proof permits removing.';
COMMENT ON COLUMN voice_command_retirement.published_sequence IS
    'Verified SQL publication cursor at preparation; its command is retained as the next append predecessor.';
COMMENT ON COLUMN voice_command_retirement.stream_generation IS
    'Permanent transport generation verified before this retirement was prepared.';
COMMENT ON COLUMN voice_command_retirement.stream_created_at IS
    'Exact command-stream birth as UTC RFC3339Nano text, preserving broker nanoseconds.';
COMMENT ON COLUMN voice_command_retirement.owner_store_created_at IS
    'Exact owner-store backing-stream birth as UTC RFC3339Nano text, preserving broker nanoseconds.';
COMMENT ON COLUMN voice_command_retirement.initializing_revision IS
    'Positive initializing owner-record revision verified for the committed transport generation.';
COMMENT ON COLUMN voice_command_retirement.consumer_stream IS
    'Exact command stream whose consumer supplied the acknowledgement proof.';
COMMENT ON COLUMN voice_command_retirement.consumer_name IS
    'Exact enforcement consumer name whose frozen profile supplied the acknowledgement proof.';
COMMENT ON COLUMN voice_command_retirement.consumer_durable IS
    'Exact durable enforcement consumer binding verified with the acknowledgement proof.';
COMMENT ON COLUMN voice_command_retirement.consumer_created_at IS
    'Exact consumer birth as UTC RFC3339Nano text; historic proof survives later consumer recreation.';
COMMENT ON COLUMN voice_command_retirement.consumer_profile_version IS
    'Checked frozen consumer profile version; version one denotes verification of the complete existing profile.';
COMMENT ON COLUMN voice_command_retirement.acknowledged_sequence IS
    'Verified stream acknowledgement floor, rather than the consumer delivery counter.';
COMMENT ON COLUMN voice_command_retirement.prepared_at IS
    'Immutable database time the acknowledged retirement interval was durably prepared.';
COMMENT ON COLUMN voice_command_retirement.completed_at IS
    'Committed database time the exact recorded prefix removal was verified and its checkpoint advanced.';
COMMENT ON COLUMN voice_command_retirement.attempts IS
    'Nonnegative retry count while this exact retirement proof remains pending.';
COMMENT ON COLUMN voice_command_retirement.retry_at IS
    'Earliest database time the pending prefix removal may be retried.';
COMMENT ON COLUMN voice_command_retirement.failure_class IS
    'Closed retry-failure category without raw error, payload or actor data.';
COMMENT ON COLUMN voice_command_sequence.retired_through_sequence IS
    'Highest broker sequence removed through completed acknowledgement proof; SQL commands and receipts remain permanent.';
COMMENT ON COLUMN voice_command_sequence.pending_retirement_id IS
    'Exact prepared retirement that blocks new publication claims until its checkpoint commits.';
COMMENT ON TABLE voice_command_admission IS
    'Temporary unpublished-command quota accounting, released only with a committed SQL publication receipt.';
COMMENT ON COLUMN voice_command_admission.sequence IS
    'Immutable command obligation charged until its verified SQL publication receipt commits.';
COMMENT ON COLUMN voice_command_admission.actor_id IS
    'Authorized caller charged for unpublished work; historic and library-only fallback uses the retained target identity.';
