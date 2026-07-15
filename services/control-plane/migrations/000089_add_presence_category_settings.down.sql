ALTER TABLE user_presence_settings
    DROP CONSTRAINT IF EXISTS user_presence_settings_private_call_tier_check,
    DROP CONSTRAINT IF EXISTS user_presence_settings_server_voice_tier_check;

ALTER TABLE user_presence_settings
    DROP COLUMN IF EXISTS private_call_show_details,
    DROP COLUMN IF EXISTS private_call_tier,
    DROP COLUMN IF EXISTS server_voice_show_details,
    DROP COLUMN IF EXISTS server_voice_tier,
    DROP COLUMN IF EXISTS master_enabled;
