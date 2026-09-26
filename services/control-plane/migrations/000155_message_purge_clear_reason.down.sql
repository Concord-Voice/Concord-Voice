LOCK TABLE message_purges IN ACCESS EXCLUSIVE MODE;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM message_purges WHERE reason = 'clear') THEN
    RAISE EXCEPTION 'refusing to drop clear purge audit evidence; restore a pre-000155 backup to downgrade safely';
  END IF;
END $$;

ALTER TABLE message_purges DROP CONSTRAINT message_purges_reason_check;

ALTER TABLE message_purges ADD CONSTRAINT message_purges_reason_check
  CHECK (reason IN ('manual', 'ban', 'kick', 'expiry')) NOT VALID;

-- Deliberate departure from 000130's down (#3462): before 000155 this
-- constraint was validated (by 000131), and the guard above has proven no row
-- can fail it, so validating here makes up -> down -> up restore pg_constraint
-- exactly, convalidated included.
ALTER TABLE message_purges VALIDATE CONSTRAINT message_purges_reason_check;
