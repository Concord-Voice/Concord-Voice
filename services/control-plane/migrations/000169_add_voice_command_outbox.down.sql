-- Command rows and nonzero cursors are durable continuity evidence. A binary
-- rollback leaves this additive schema in place; downgrade never clears it.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_commands BOOLEAN;
    has_cursor_evidence BOOLEAN;
BEGIN
    -- Preserve the publisher lock order, and hold both locks before testing
    -- evidence so no concurrent allocation or receipt can race the teardown.
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

    IF to_regclass(format('%I.voice_command_outbox', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.voice_command_outbox)',
            migration_schema)
            INTO has_commands;
        IF has_commands THEN
            RAISE EXCEPTION 'cannot drop voice command outbox while command evidence exists'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    IF to_regclass(format('%I.voice_command_sequence', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.voice_command_sequence WHERE last_allocated_sequence <> 0 OR last_published_sequence <> 0)',
            migration_schema)
            INTO has_cursor_evidence;
        IF has_cursor_evidence THEN
            RAISE EXCEPTION 'cannot drop voice command cursors after allocation or publication'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;

    -- Dropping the table removes only its own trigger, indexes and constraints.
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_command_outbox', migration_schema);
    EXECUTE format(
        'DROP FUNCTION IF EXISTS %I.protect_voice_command_outbox_envelope()',
        migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_command_sequence', migration_schema);
END;
$$;
