-- 000191_accountguard_rewrap_xact_bind.up.sql
-- Binds the rewrap flag's value to the writing transaction (#3680 follow-up,
-- red-team-critical F2 on #4129).
--
-- 000189's accountguard_is_rewrap() accepted the bare value 'mfa-rekey'. A
-- writer that set it with set_config(..., false), at session level, exempted
-- every later autocommit swap on that pooled connection. The flag now has to
-- read 'mfa-rekey:' || pg_current_xact_id(): only the transaction that set it
-- matches, so a session-level setting does not carry into a later
-- transaction. pg_current_xact_id() returns the top-level id inside a
-- subtransaction, and every caller is a material trigger on a write, which
-- already holds an id.
--
-- It binds a mistaken writer, not a session that runs arbitrary SQL: such a
-- session can predict its next transaction id, and as the table owner it can
-- already DISABLE TRIGGER (000189's accepted search_path boundary).
--
-- Deploy order: internal/mfa/rekey.go writes the bound form from the same
-- change. An mfa-rekey binary built before it sets the bare value, which this
-- function no longer accepts, so its rewrap re-enrols every row it touches;
-- run mfa-rekey from the image that carries this migration.
--
-- Catalog-only: CREATE OR REPLACE keeps the function's OID, so the five
-- material triggers that call it pick up the new body without being touched.

CREATE OR REPLACE FUNCTION accountguard_is_rewrap() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT COALESCE(NULLIF(current_setting('concord.credential_rewrap', true), '')
                          = 'mfa-rekey:' || pg_current_xact_id()::text, false) $$;
