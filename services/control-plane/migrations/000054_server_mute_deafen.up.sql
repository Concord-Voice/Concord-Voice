-- Server-enforced mute/deafen: hard enforcement flags that persist across voice
-- sessions until explicitly removed by a moderator (servers) or group admin (DMs).
--
-- server_members: server-wide enforcement (RBAC-gated, hierarchy-enforced)
-- dm_participants: group DM enforcement (admin-only)

ALTER TABLE server_members
  ADD COLUMN server_muted BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN server_deafened BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE dm_participants
  ADD COLUMN server_muted BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN server_deafened BOOLEAN NOT NULL DEFAULT false;
