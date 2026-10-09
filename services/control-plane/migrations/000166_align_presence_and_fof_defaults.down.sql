ALTER TABLE user_presence_settings
    ALTER COLUMN server_voice_show_details SET DEFAULT TRUE,
    ALTER COLUMN private_call_tier SET DEFAULT 0;

ALTER TABLE privacy_settings
    ALTER COLUMN dm_friends_of_friends SET DEFAULT FALSE;
