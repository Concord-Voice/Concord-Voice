ALTER TABLE server_members
  ADD COLUMN timed_out_until TIMESTAMPTZ NULL;

COMMENT ON COLUMN server_members.timed_out_until IS 'UTC timestamp until which the server member is barred from sending messages and joining voice channels.';
