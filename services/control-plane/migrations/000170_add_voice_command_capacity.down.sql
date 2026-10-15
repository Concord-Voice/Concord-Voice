-- Removing acknowledged-prefix evidence would make a used compacted stream
-- indistinguishable from continuity loss. Keep this additive schema during an
-- application rollback, and use an image that understands recorded suffixes.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_capacity_evidence BOOLEAN;
BEGIN
    -- Take the publisher's sequence -> command order before the owned capacity
    -- tables. Hold the locks through every guard and drop so no preparation,
    -- allocation or receipt can race removal of its evidence/accounting.
    IF to_regclass(format('%I.voice_command_sequence', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'LOCK TABLE %I.voice_command_sequence IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    END IF;
    IF to_regclass(format('%I.voice_command_outbox', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'LOCK TABLE %I.voice_command_outbox IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    END IF;
    IF to_regclass(format('%I.voice_command_retirement', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'LOCK TABLE %I.voice_command_retirement IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    END IF;
    IF to_regclass(format('%I.voice_command_admission', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'LOCK TABLE %I.voice_command_admission IN ACCESS EXCLUSIVE MODE',
            migration_schema);
    END IF;

    IF to_regclass(format('%I.voice_command_retirement', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.voice_command_retirement)',
            migration_schema)
            INTO has_capacity_evidence;
        IF has_capacity_evidence THEN
            RAISE EXCEPTION 'cannot drop voice command capacity while retirement proof exists'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    IF to_regclass(format('%I.voice_command_admission', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.voice_command_admission)',
            migration_schema)
            INTO has_capacity_evidence;
        IF has_capacity_evidence THEN
            RAISE EXCEPTION 'cannot drop voice command capacity while admission accounting exists'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;

    -- Check columns separately so a guarded teardown also handles a partial
    -- installation without dereferencing an absent capacity column.
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = migration_schema
            AND table_name = 'voice_command_sequence'
            AND column_name = 'retired_through_sequence'
    ) THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.voice_command_sequence WHERE retired_through_sequence <> 0)',
            migration_schema)
            INTO has_capacity_evidence;
        IF has_capacity_evidence THEN
            RAISE EXCEPTION 'cannot drop voice command capacity after prefix retirement'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = migration_schema
            AND table_name = 'voice_command_sequence'
            AND column_name = 'pending_retirement_id'
    ) THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.voice_command_sequence WHERE pending_retirement_id IS NOT NULL)',
            migration_schema)
            INTO has_capacity_evidence;
        IF has_capacity_evidence THEN
            RAISE EXCEPTION 'cannot drop voice command capacity while retirement is pending'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;

    -- Release only this migration's reference and checkpoint columns. Dropping
    -- an owned table removes only its own indexes, constraints and trigger.
    IF to_regclass(format('%I.voice_command_sequence', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'ALTER TABLE %I.voice_command_sequence DROP CONSTRAINT IF EXISTS voice_command_sequence_pending_retirement_fkey',
            migration_schema);
        EXECUTE format(
            'ALTER TABLE %I.voice_command_sequence DROP CONSTRAINT IF EXISTS voice_command_sequence_retired_check',
            migration_schema);
        EXECUTE format(
            'ALTER TABLE %I.voice_command_sequence DROP COLUMN IF EXISTS pending_retirement_id',
            migration_schema);
        EXECUTE format(
            'ALTER TABLE %I.voice_command_sequence DROP COLUMN IF EXISTS retired_through_sequence',
            migration_schema);
    END IF;

    EXECUTE format('DROP TABLE IF EXISTS %I.voice_command_admission', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_command_retirement', migration_schema);
    EXECUTE format(
        'DROP FUNCTION IF EXISTS %I.protect_voice_command_retirement_proof()',
        migration_schema);
END;
$$;
