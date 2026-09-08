LOCK TABLE message_purges IN ACCESS EXCLUSIVE MODE;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM message_purges WHERE reason = 'expiry') THEN
    RAISE EXCEPTION 'refusing to drop expiry purge audit evidence; restore a pre-000130 backup to downgrade safely';
  END IF;
END $$;

ALTER TABLE message_purges DROP CONSTRAINT message_purges_reason_check;

ALTER TABLE message_purges ADD CONSTRAINT message_purges_reason_check
  CHECK (reason IN ('manual', 'ban', 'kick')) NOT VALID;
