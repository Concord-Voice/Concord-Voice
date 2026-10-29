-- 000191_accountguard_rewrap_xact_bind.down.sql
-- Restores 000189's accountguard_is_rewrap() body, after a guard.
--
-- Guard (red-team-critical F1 on #4129): 000189's down reads a missing
-- accountguard_state row as "not activated" and proceeds. The runner fails
-- closed on a missing row, so the rollback must too. Every rollback to before
-- 000189 passes through this down first, so it refuses while
-- accountguard_state exists and holds no row; the activation state is then
-- unknown. Recovery: restore the row (INSERT INTO accountguard_state DEFAULT
-- VALUES, then set activated_at if the guard was activated) and re-run.
-- golang-migrate records the target version (190 or 189) as dirty before it
-- runs this file, so after a refusal `force 191` and boot.
--
-- The SHARE lock holds off a concurrent DELETE until this down commits. It
-- does not cover a row deleted after this down and before 000189's down;
-- 000189's own guard reads that case as not activated.
SET LOCAL lock_timeout = '3s';

DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_row          BOOLEAN;
BEGIN
    IF to_regclass(format('%I.accountguard_state', migration_schema)) IS NOT NULL THEN
        EXECUTE format('LOCK TABLE %I.accountguard_state IN SHARE MODE', migration_schema);
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.accountguard_state)', migration_schema)
            INTO has_row;
        IF NOT has_row THEN
            RAISE EXCEPTION
                'cannot roll back 000191: accountguard_state holds no row, so the activation state is unknown'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION accountguard_is_rewrap() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT COALESCE(NULLIF(current_setting('concord.credential_rewrap', true), '') = 'mfa-rekey', false) $$;
