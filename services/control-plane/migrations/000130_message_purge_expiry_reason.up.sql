ALTER TABLE message_purges ADD CONSTRAINT message_purges_reason_expiry_check
  CHECK (reason IN ('manual', 'ban', 'kick', 'expiry')) NOT VALID;

ALTER TABLE message_purges DROP CONSTRAINT message_purges_reason_check;

ALTER TABLE message_purges RENAME CONSTRAINT message_purges_reason_expiry_check
  TO message_purges_reason_check;
