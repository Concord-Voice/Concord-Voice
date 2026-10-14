-- A provisioned receipt is continuity evidence, not disposable runtime state.
-- Downgrade must retain it so a later upgrade cannot create an empty transport
-- while a predecessor still owns media or undelivered commands remain.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_receipt BOOLEAN;
BEGIN
    IF to_regclass(format('%I.voice_command_transport', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'LOCK TABLE %I.voice_command_transport IN ACCESS EXCLUSIVE MODE',
            migration_schema);
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.voice_command_transport WHERE provisioned_at IS NOT NULL)',
            migration_schema)
            INTO has_receipt;
        IF has_receipt THEN
            RAISE EXCEPTION
                'cannot drop voice_command_transport after durable transport provisioning';
        END IF;
    END IF;

    EXECUTE format('DROP TABLE IF EXISTS %I.voice_command_transport', migration_schema);
    EXECUTE format(
        'DROP FUNCTION IF EXISTS %I.protect_voice_command_transport_receipt()',
        migration_schema);
END;
$$;
