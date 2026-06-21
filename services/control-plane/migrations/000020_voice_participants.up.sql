-- Voice participants table: tracks who is currently in a voice channel.
-- Rows are inserted by the NATS subscriber when the media plane publishes
-- voice.joined events, and deleted on voice.left / voice.room_empty.
CREATE TABLE voice_participants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_muted BOOLEAN NOT NULL DEFAULT false,
    is_deafened BOOLEAN NOT NULL DEFAULT false,
    is_video_on BOOLEAN NOT NULL DEFAULT false,
    is_screen_sharing BOOLEAN NOT NULL DEFAULT false,
    UNIQUE(channel_id, user_id)
);

CREATE INDEX idx_voice_participants_channel ON voice_participants(channel_id);
CREATE INDEX idx_voice_participants_user ON voice_participants(user_id);
