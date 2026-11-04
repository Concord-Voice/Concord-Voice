-- 000196_accountguard_requests.down.sql
-- Guarded down for 000196 (000189 and 000138 shape). The guard DO block is
-- schema-aware: it qualifies every name with current_schema(), never a hardcoded
-- public. The DROPs after it resolve through search_path, as the up's CREATEs do.
--
-- Refuses (RAISE EXCEPTION, naming the condition) while
--   * any request is pending or disputed: it is an open obligation the owner or the
--     sweeper still has to settle;
--   * any proof or dispute_resolved cancel is younger than 30 days (N_cancel_cp):
--     its retained fingerprints are what let the same request be re-opened;
--   * any user_credential_history row is younger than 30 days: it is what a reset
--     or contest restores from;
--   * any account_address_reservations row exists: a displaced primary is still held.
-- It deletes nothing and drops nothing when it refuses. A completed request, a
-- cancel for any other reason, an older cancel or history row, and every
-- accountguard_hmac_keys row do NOT block: the key ids stamped on dropped rows die
-- with them, and a re-run up restarts the registry at 1 with nothing left to
-- misread.
--
-- SET LOCAL lock_timeout bounds every lock below. A timeout fails the file and rolls
-- the down back whole; recovery is the same as for a refused down (below).
--
-- The ACCESS EXCLUSIVE locks are taken BEFORE the checks, in the up's creation
-- order, so a row that commits while this file waits for a lock is seen. Each table
-- is looked up first, so the file also runs where a previous attempt left it half
-- undone.
--
-- Dropping the three tables with a users foreign key also takes ACCESS EXCLUSIVE on
-- users (removing the FK triggers), held to commit and bounded by the lock_timeout,
-- so every users read, sign-in included, queues behind it for up to 3s. Run the
-- down off-peak; a timeout here recovers as below.
--
-- Recovery after a refused or timed-out down: golang-migrate records the TARGET
-- version (the highest below 196) as dirty before it runs this file, while the
-- file's implicit transaction rolled back and the schema is still at 196. Confirm
-- that account_pending_requests exists, then `force 196` and boot. NEVER force the
-- prior version: the next down would run that migration's down against a schema
-- still at 196. See [internal]rules/migrations.md.
SET LOCAL lock_timeout = '3s';

DO $$
DECLARE
    migration_schema TEXT := current_schema();
    lock_table       TEXT;
    live_count       BIGINT;
BEGIN
    FOREACH lock_table IN ARRAY ARRAY[
        'account_pending_requests', 'account_pending_request_origins',
        'user_credential_history', 'account_address_reservations',
        'accountguard_hmac_keys'
    ] LOOP
        IF to_regclass(format('%I.%I', migration_schema, lock_table)) IS NOT NULL THEN
            EXECUTE format('LOCK TABLE %I.%I IN ACCESS EXCLUSIVE MODE', migration_schema, lock_table);
        END IF;
    END LOOP;

    IF to_regclass(format('%I.account_pending_requests', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'SELECT count(*) FROM %I.account_pending_requests WHERE status IN (''pending'', ''disputed'')',
            migration_schema)
            INTO live_count;
        IF live_count > 0 THEN
            RAISE EXCEPTION
                'cannot roll back 000196 while % account-guard request(s) are pending or disputed', live_count;
        END IF;

        EXECUTE format(
            'SELECT count(*) FROM %I.account_pending_requests '
            'WHERE cancel_reason IN (''proof'', ''dispute_resolved'') '
            'AND cancelled_at > now() - interval ''30 days''',
            migration_schema)
            INTO live_count;
        IF live_count > 0 THEN
            RAISE EXCEPTION
                'cannot roll back 000196 while % proof or dispute_resolved cancel(s) are younger than 30 days', live_count;
        END IF;
    END IF;

    IF to_regclass(format('%I.user_credential_history', migration_schema)) IS NOT NULL THEN
        EXECUTE format(
            'SELECT count(*) FROM %I.user_credential_history WHERE retired_at > now() - interval ''30 days''',
            migration_schema)
            INTO live_count;
        IF live_count > 0 THEN
            RAISE EXCEPTION
                'cannot roll back 000196 while % credential history row(s) are younger than 30 days', live_count;
        END IF;
    END IF;

    IF to_regclass(format('%I.account_address_reservations', migration_schema)) IS NOT NULL THEN
        EXECUTE format('SELECT count(*) FROM %I.account_address_reservations', migration_schema)
            INTO live_count;
        IF live_count > 0 THEN
            RAISE EXCEPTION
                'cannot roll back 000196 while % address reservation(s) exist', live_count;
        END IF;
    END IF;
END
$$;

-- Reverse of the up's creation order. The origins table references the requests
-- table, so it goes first. No CASCADE: nothing outside these five tables depends on
-- them.
DROP TABLE IF EXISTS accountguard_hmac_keys;
DROP TABLE IF EXISTS account_address_reservations;
DROP TABLE IF EXISTS user_credential_history;
DROP TABLE IF EXISTS account_pending_request_origins;
DROP TABLE IF EXISTS account_pending_requests;
