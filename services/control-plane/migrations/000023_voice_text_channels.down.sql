DROP INDEX IF EXISTS idx_channels_linked_voice;
ALTER TABLE channels DROP COLUMN IF EXISTS linked_voice_channel_id;
