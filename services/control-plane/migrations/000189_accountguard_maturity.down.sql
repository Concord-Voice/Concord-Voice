-- 000189_accountguard_maturity.down.sql
-- Guarded down for 000189 (000178 shape). The guard DO block is schema-aware: it
-- qualifies every name with current_schema(), never a hardcoded public. The DDL
-- after it resolves through search_path, as the up's does.
--
-- SET LOCAL lock_timeout bounds every lock below. The file takes ACCESS
-- EXCLUSIVE on users and the four factor tables, and an unbounded wait would
-- queue all sign-in traffic behind it. A timeout fails the file and rolls the
-- down back whole; recovery is the same as for a refused down (below).
--
-- Refuses while the account guard is activated (accountguard_state.activated_at
-- is set) or while any credential is contested (users.email_contested_at or a
-- factor table's maturity_contested_at). A contested stamp is live state that
-- dropping the column would silently discard. It does NOT refuse on an immature
-- credential: before activation nothing reads maturity, so a stamp carries no
-- state (spec 6.1, D-2; amendment A-14 narrows parent 7.1 accordingly). It
-- deletes nothing else.
--
-- It drops accountguard_material_version_seq without the consumed-watermark
-- refusal 000159's down has, and a down->up restarting the sequence at 2 is
-- safe: every table that can hold a version reference (000190 onward) is
-- dropped by an earlier down, and guard tokens and jobs exist only behind
-- activation, which this down refuses. 000159 refuses for the opposite reason:
-- its references live outside the database.
--
-- The ACCESS EXCLUSIVE locks are taken BEFORE the checks, in the up's order
-- (users first, then the four factor tables, then accountguard_state), so a row
-- that commits while this file waits for a lock is seen, and the order matches
-- every other writer. Each table and column is looked up first, so the file also
-- runs on a schema where a previous attempt left it half undone.
--
-- Recovery after a refused or timed-out down: golang-migrate records the TARGET
-- version (188) as dirty before it runs this file, while the file's implicit
-- transaction rolled back and the schema is still at 189. Confirm that
-- accountguard_state exists, then `force 189` and boot. NEVER `force 188` past
-- this guard while any server has activated the account guard. See
-- [internal]rules/migrations.md.
SET LOCAL lock_timeout = '3s';

DO $$
DECLARE
    migration_schema TEXT := current_schema();
    lock_table       TEXT;
    contested        RECORD;
    is_activated     BOOLEAN;
    is_contested     BOOLEAN;
BEGIN
    FOREACH lock_table IN ARRAY ARRAY[
        'users', 'user_mfa_totp', 'user_mfa_webauthn', 'user_sso_identities', 'user_recovery_keys',
        'accountguard_state'
    ] LOOP
        IF to_regclass(format('%I.%I', migration_schema, lock_table)) IS NOT NULL THEN
            EXECUTE format('LOCK TABLE %I.%I IN ACCESS EXCLUSIVE MODE', migration_schema, lock_table);
        END IF;
    END LOOP;

    IF to_regclass(format('%I.accountguard_state', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I.accountguard_state WHERE activated_at IS NOT NULL)',
            migration_schema)
            INTO is_activated;
        IF is_activated THEN
            RAISE EXCEPTION
                'cannot roll back 000189 while the account guard is activated'
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;

    FOR contested IN
        SELECT * FROM (VALUES
            ('users',               'email_contested_at'),
            ('user_mfa_totp',       'maturity_contested_at'),
            ('user_mfa_webauthn',   'maturity_contested_at'),
            ('user_sso_identities', 'maturity_contested_at'),
            ('user_recovery_keys',  'maturity_contested_at')
        ) AS v(table_name, column_name)
    LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.columns c
             WHERE c.table_schema = migration_schema
               AND c.table_name = contested.table_name
               AND c.column_name = contested.column_name
        ) THEN
            EXECUTE format(
                'SELECT EXISTS (SELECT 1 FROM %I.%I WHERE %I IS NOT NULL)',
                migration_schema, contested.table_name, contested.column_name)
                INTO is_contested;
            IF is_contested THEN
                RAISE EXCEPTION
                    'cannot roll back 000189 while a % credential is contested', contested.table_name
                    USING ERRCODE = 'restrict_violation';
            END IF;
        END IF;
    END LOOP;
END
$$;

-- Triggers before the functions they call, the functions before the stamp that
-- the column defaults depend on, and the columns (which carry their CHECKs with
-- them) before the stamp function. The version sequence goes last, after every
-- function that draws from it.
DROP TRIGGER IF EXISTS accountguard_material ON users;
DROP TRIGGER IF EXISTS accountguard_material ON user_mfa_totp;
DROP TRIGGER IF EXISTS accountguard_material ON user_mfa_webauthn;
DROP TRIGGER IF EXISTS accountguard_material ON user_sso_identities;
DROP TRIGGER IF EXISTS accountguard_material ON user_recovery_keys;

DROP TABLE IF EXISTS accountguard_state;

ALTER TABLE users
    DROP COLUMN IF EXISTS password_matures_at,
    DROP COLUMN IF EXISTS email_matures_at,
    DROP COLUMN IF EXISTS email_contested_at,
    DROP COLUMN IF EXISTS password_version,
    DROP COLUMN IF EXISTS email_version,
    DROP COLUMN IF EXISTS password_enrolled_at,
    DROP COLUMN IF EXISTS email_enrolled_at,
    DROP COLUMN IF EXISTS password_enrol_class,
    DROP COLUMN IF EXISTS email_enrol_class;

ALTER TABLE user_mfa_totp
    DROP COLUMN IF EXISTS matures_at,
    DROP COLUMN IF EXISTS maturity_contested_at,
    DROP COLUMN IF EXISTS material_version,
    DROP COLUMN IF EXISTS enrolled_at,
    DROP COLUMN IF EXISTS enrol_class;

ALTER TABLE user_mfa_webauthn
    DROP COLUMN IF EXISTS matures_at,
    DROP COLUMN IF EXISTS maturity_contested_at,
    DROP COLUMN IF EXISTS material_version,
    DROP COLUMN IF EXISTS enrolled_at,
    DROP COLUMN IF EXISTS enrol_class;

ALTER TABLE user_sso_identities
    DROP COLUMN IF EXISTS matures_at,
    DROP COLUMN IF EXISTS maturity_contested_at,
    DROP COLUMN IF EXISTS material_version,
    DROP COLUMN IF EXISTS enrolled_at,
    DROP COLUMN IF EXISTS enrol_class;

ALTER TABLE user_recovery_keys
    DROP COLUMN IF EXISTS matures_at,
    DROP COLUMN IF EXISTS maturity_contested_at,
    DROP COLUMN IF EXISTS material_version,
    DROP COLUMN IF EXISTS enrolled_at,
    DROP COLUMN IF EXISTS enrol_class;

DROP FUNCTION IF EXISTS accountguard_material_users();
DROP FUNCTION IF EXISTS accountguard_material_user_mfa_totp();
DROP FUNCTION IF EXISTS accountguard_material_user_mfa_webauthn();
DROP FUNCTION IF EXISTS accountguard_material_user_sso_identities();
DROP FUNCTION IF EXISTS accountguard_material_user_recovery_keys();
DROP FUNCTION IF EXISTS accountguard_is_rewrap();
DROP FUNCTION IF EXISTS accountguard_maturity_stamp();
DROP SEQUENCE IF EXISTS accountguard_material_version_seq;
