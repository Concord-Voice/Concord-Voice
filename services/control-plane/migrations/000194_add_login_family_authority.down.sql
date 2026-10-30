-- Unused-schema rollback only. NEVER discard authority, closure, boot or floor
-- evidence, including retired boots and completed-but-retained auth operations.
--
-- The refusal check counts ONLY the tables this migration creates. It does not
-- count voice_enforcement_sessions (000154) or credential_epoch_voice_ejections
-- (000151): both predate this migration and hold live-call rows on every
-- running server, and nothing here depends on them. The only link is
-- voice_auth_room_bindings -> voice_enforcement_sessions, and dropping the
-- referencing table needs no lock or drain on the referenced one. The binding
-- table is itself in the checked set, so no live binding can be orphaned.
--
-- SET LOCAL lock_timeout bounds each ACCESS EXCLUSIVE wait (an unbounded wait
-- would queue sign-in and voice traffic behind it, as in 000189's down);
-- statement_timeout bounds the whole DO block. Either failure rolls the file
-- back whole. Single DO statement makes lock/check/drop atomic even outside the
-- runner. Refusal leaves schema 194 intact; the runner may mark the previous
-- version dirty. Inspect the actual schema and recover the migration version
-- using established rules (force 194 once the tables are confirmed present).
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
DO $$
DECLARE
    relation_name TEXT;
    has_evidence BOOLEAN;
    migration_schema TEXT := current_schema();
BEGIN
    IF to_regclass(format('%I.login_family_rollout', migration_schema)) IS NOT NULL THEN
        -- Lock barrier first without a later lock-mode upgrade.
        EXECUTE format('LOCK TABLE %I.login_family_rollout IN ACCESS EXCLUSIVE MODE', migration_schema);
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.login_family_rollout WHERE activated_at IS NOT NULL OR receipt IS NOT NULL)', migration_schema)
            INTO has_evidence;
        IF has_evidence THEN
            RAISE EXCEPTION 'cannot remove activated login-family floor' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;

    -- Releases, retirement and both GC units do not take the rollout row lock.
    -- Relation locks exclude them throughout the subsequent checks and drops.
    FOREACH relation_name IN ARRAY ARRAY[
        'login_families', 'login_family_sids', 'login_family_revocations',
        'login_family_revocation_targets', 'voice_auth_boot_fences',
        'voice_auth_connections', 'voice_auth_room_bindings',
        'voice_auth_connection_closures', 'voice_auth_room_closures']
    LOOP
        IF to_regclass(format('%I.%I', migration_schema, relation_name)) IS NOT NULL THEN
            EXECUTE format('LOCK TABLE %I.%I IN ACCESS EXCLUSIVE MODE', migration_schema, relation_name);
        END IF;
    END LOOP;
    FOREACH relation_name IN ARRAY ARRAY[
        'login_families', 'login_family_sids', 'login_family_revocations',
        'login_family_revocation_targets', 'voice_auth_boot_fences',
        'voice_auth_connections', 'voice_auth_room_bindings',
        'voice_auth_connection_closures', 'voice_auth_room_closures']
    LOOP
        IF to_regclass(format('%I.%I', migration_schema, relation_name)) IS NOT NULL THEN
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I)', migration_schema, relation_name) INTO has_evidence;
            IF has_evidence THEN
                RAISE EXCEPTION 'cannot remove login-family schema while % evidence remains', relation_name
                    USING ERRCODE = 'restrict_violation';
            END IF;
        END IF;
    END LOOP;
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_auth_room_bindings', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_auth_room_closures', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_auth_connection_closures', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_auth_connections', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.voice_auth_boot_fences', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.login_family_revocation_targets', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.login_family_sids', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.login_families', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.login_family_revocations', migration_schema);
    EXECUTE format('DROP TABLE IF EXISTS %I.login_family_rollout', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_closure_gc()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_closure_check()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_release_guard()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_closure_insert()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_binding_insert()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_connection_insert()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_boot_guard()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_gc_guard()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_target_insert()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_frontier_check()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_state_guard()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_immutable()', migration_schema);
    EXECUTE format('DROP FUNCTION IF EXISTS %I.login_family_generation_lock(TEXT, UUID)', migration_schema);
END
$$;
