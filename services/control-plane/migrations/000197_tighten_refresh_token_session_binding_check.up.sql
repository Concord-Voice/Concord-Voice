-- 000197_tighten_refresh_token_session_binding_check.up.sql
-- Closes a hole in 000195's both-or-neither CHECK on refresh_tokens.pop_jkt and
-- pop_alg. A CHECK passes when it yields UNKNOWN, and 000195's bound branch
-- yields UNKNOWN for (NULL, 'ES256') and for (43-character jkt, NULL), so both
-- half-bound pairs were admitted. The bound branch now requires both columns to
-- be non-NULL. Every Go writer already supplies both or neither.
--
-- The replacement is added NOT VALID and swapped in for the old one in the
-- same statement, the 000155 shape. That holds every write from here on
-- without scanning the table under the ACCESS EXCLUSIVE lock. Rows written
-- before it never came from a Go writer as a half-bound pair, and session
-- binding has been dormant in production since 000195 (no row is bound).
-- The drop is replaced by a strictly stronger check in the same statement, so
-- no guarantee is ever absent.
--
-- A NOT VALID check still applies to every later UPDATE of an old row, so one
-- surviving half-bound row (only a manual write could make one) would make
-- every revocation that touches it fail, and a user-wide revoke with it. The
-- guard below refuses the migration while such a row exists. It scans under
-- ACCESS SHARE, before the ALTER takes ACCESS EXCLUSIVE, so auth traffic is
-- never queued behind the scan. Once it passes every row satisfies the new
-- check, which therefore stays NOT VALID permanently: add no validating
-- migration (the 000168 reasoning, recorded in [internal]rules/migrations.md).
--
-- Recovery after a refused up: golang-migrate records 197 as dirty with the
-- schema still at 196. Revoke or clear the reported rows, `force 196`, and
-- run the migration again.
SET LOCAL lock_timeout = '3s';

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM refresh_tokens
        WHERE (pop_jkt IS NULL) <> (pop_alg IS NULL)
           OR (pop_jkt IS NOT NULL
               AND (pop_alg <> 'ES256' OR char_length(pop_jkt) <> 43))
    ) THEN
        RAISE EXCEPTION
            'refresh_tokens holds a row the tightened session binding check refuses'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

ALTER TABLE refresh_tokens
    -- squawk-ignore ban-drop-constraint -- replaced by a strictly stronger check in this same statement
    DROP CONSTRAINT refresh_tokens_pop_binding_check,
    ADD CONSTRAINT refresh_tokens_pop_binding_check
    CHECK ((pop_jkt IS NULL AND pop_alg IS NULL)
        OR (pop_jkt IS NOT NULL AND pop_alg IS NOT NULL
            AND pop_alg = 'ES256' AND char_length(pop_jkt) = 43)) NOT VALID;
