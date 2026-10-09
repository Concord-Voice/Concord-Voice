-- Apply the documented defaults to new presence rows without rewriting existing choices.
ALTER TABLE user_presence_settings
    ALTER COLUMN server_voice_show_details SET DEFAULT FALSE,
    ALTER COLUMN private_call_tier SET DEFAULT 1;

ALTER TABLE privacy_settings
    ALTER COLUMN dm_friends_of_friends SET DEFAULT TRUE;
