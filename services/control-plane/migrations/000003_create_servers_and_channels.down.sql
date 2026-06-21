-- Drop indexes first
DROP INDEX IF EXISTS idx_server_members_user_id;
DROP INDEX IF EXISTS idx_channels_server_id;
DROP INDEX IF EXISTS idx_servers_owner_id;

-- Drop tables in reverse order of creation (respecting foreign keys)
DROP TABLE IF EXISTS server_members;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS servers;
