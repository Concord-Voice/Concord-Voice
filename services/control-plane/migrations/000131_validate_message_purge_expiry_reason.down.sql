ALTER TABLE message_purges DROP CONSTRAINT message_purges_reason_check;

ALTER TABLE message_purges ADD CONSTRAINT message_purges_reason_check
  CHECK (reason IN ('manual', 'ban', 'kick', 'expiry')) NOT VALID;
