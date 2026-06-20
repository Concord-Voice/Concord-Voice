DROP INDEX IF EXISTS idx_dm_participants_role;
ALTER TABLE dm_participants DROP COLUMN IF EXISTS role;
ALTER TABLE dm_conversations DROP COLUMN IF EXISTS icon_url;
