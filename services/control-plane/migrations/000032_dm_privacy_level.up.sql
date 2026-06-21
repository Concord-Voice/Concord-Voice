-- Replace messages_friends_only + messages_server_members booleans with
-- a single dm_privacy_level integer and dm_friends_of_friends boolean.
--
-- Levels:
--   0 = off (no DMs)
--   1 = friends only
--   2 = friends + server members (default, matches previous behavior)
--   3 = allow all

ALTER TABLE privacy_settings ADD COLUMN IF NOT EXISTS dm_privacy_level INTEGER NOT NULL DEFAULT 2;
ALTER TABLE privacy_settings ADD COLUMN IF NOT EXISTS dm_friends_of_friends BOOLEAN NOT NULL DEFAULT FALSE;

-- Migrate existing data: map the two booleans to the new level
-- friends_only=false -> allow all (3)
-- friends_only=true, server_members=true -> friends + server (2)
-- friends_only=true, server_members=false -> friends only (1)
UPDATE privacy_settings SET dm_privacy_level = CASE
  WHEN messages_friends_only = FALSE THEN 3
  WHEN messages_friends_only = TRUE AND messages_server_members = TRUE THEN 2
  WHEN messages_friends_only = TRUE AND messages_server_members = FALSE THEN 1
  ELSE 2
END;
