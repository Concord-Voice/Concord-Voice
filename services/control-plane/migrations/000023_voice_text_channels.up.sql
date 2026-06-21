-- Linked text channels for voice channels.
-- Each voice channel can have one auto-created text channel attached to it.
-- The linked text channel is hidden in the sidebar unless the user is in the voice channel.

ALTER TABLE channels ADD COLUMN linked_voice_channel_id UUID REFERENCES channels(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX idx_channels_linked_voice ON channels(linked_voice_channel_id) WHERE linked_voice_channel_id IS NOT NULL;
