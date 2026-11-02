-- 000195_add_refresh_token_session_binding.down.sql
-- Guarded down for 000195 (the 000193 shape, schema-aware).
--
-- Dropping the columns would silently unbind every bound session, so a stolen
-- refresh token would refresh without the device key. The down therefore
-- refuses while any row is bound. This is the database half of the rollback
-- floor recorded in [internal]deploys.md. The ACCESS EXCLUSIVE lock is
-- taken BEFORE the check, so a row bound while this file waits is seen.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (194) as dirty, but the schema is still at 195. Confirm the pop_jkt column
-- exists, then `force 195` (never 194) and boot.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_row BOOLEAN;
BEGIN
    IF to_regclass(format('%I.refresh_tokens', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    EXECUTE format(
        'LOCK TABLE %I.refresh_tokens IN ACCESS EXCLUSIVE MODE',
        migration_schema);

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.refresh_tokens WHERE pop_jkt IS NOT NULL)',
        migration_schema)
        INTO has_row;
    IF has_row THEN
        RAISE EXCEPTION
            'cannot drop refresh token session binding while a session is bound'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format(
        'ALTER TABLE %I.refresh_tokens DROP CONSTRAINT refresh_tokens_pop_binding_check, '
        'DROP COLUMN pop_alg, DROP COLUMN pop_jkt',
        migration_schema);
END
$$;
