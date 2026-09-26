-- Guarded down for 000157 (000138 shape). Dropping the column while any server
-- enforces would silently turn enforcement off for that server, so the down
-- refuses instead of discarding the setting.
--
-- The lock keeps the check and the drop atomic against a concurrent flip; it is
-- the same strength DROP COLUMN takes anyway.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (156) as dirty before running this file, while the file's implicit
-- transaction has rolled back and the column still exists. Confirm the column
-- exists, then `force 157` and boot. NEVER `force 156`: a following `down`
-- would run 000156's down. See [internal]rules/migrations.md.
LOCK TABLE servers IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    enforcing_count bigint;
BEGIN
    -- The count runs through EXECUTE so it is parsed only when the column
    -- exists; a static reference would fail to prepare on a re-run after the
    -- column is gone.
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'servers'
          AND column_name = 'enforce_mfa_dangerous_actions'
    ) THEN
        EXECUTE 'SELECT count(*) FROM servers WHERE enforce_mfa_dangerous_actions'
            INTO enforcing_count;
        IF enforcing_count > 0 THEN
            RAISE EXCEPTION
                'cannot remove servers.enforce_mfa_dangerous_actions while % server(s) enforce it',
                enforcing_count;
        END IF;
    END IF;
END
$$;

ALTER TABLE servers
    DROP COLUMN IF EXISTS enforce_mfa_dangerous_actions;
