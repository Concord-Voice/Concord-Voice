ALTER TABLE server_members
  DROP COLUMN IF EXISTS server_muted,
  DROP COLUMN IF EXISTS server_deafened;

ALTER TABLE dm_participants
  DROP COLUMN IF EXISTS server_muted,
  DROP COLUMN IF EXISTS server_deafened;
