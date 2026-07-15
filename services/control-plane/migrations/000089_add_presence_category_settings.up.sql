ALTER TABLE user_presence_settings
    ADD COLUMN master_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN server_voice_tier SMALLINT NOT NULL DEFAULT 1,
    ADD COLUMN server_voice_show_details BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN private_call_tier SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN private_call_show_details BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE user_presence_settings
    ADD CONSTRAINT user_presence_settings_server_voice_tier_check
        CHECK (server_voice_tier IN (0, 1, 2)) NOT VALID,
    ADD CONSTRAINT user_presence_settings_private_call_tier_check
        CHECK (private_call_tier IN (0, 1, 2)) NOT VALID;

COMMENT ON COLUMN user_presence_settings.master_enabled IS
    'Global authorization switch for Rich Presence disclosure.';
COMMENT ON COLUMN user_presence_settings.server_voice_tier IS
    'Server Voice audience: 0 off, 1 friends in server, 2 all in server.';
COMMENT ON COLUMN user_presence_settings.server_voice_show_details IS
    'Controls Server Voice detail disclosure to the authorized audience.';
COMMENT ON COLUMN user_presence_settings.private_call_tier IS
    'Private Call audience: 0 off, 1 friends, 2 shared-server users.';
COMMENT ON COLUMN user_presence_settings.private_call_show_details IS
    'Controls Private Call detail disclosure to the authorized audience.';
