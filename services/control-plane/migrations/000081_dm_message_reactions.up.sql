-- DM message reactions: per-user emoji reactions on direct messages.
CREATE TABLE dm_message_reactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id UUID NOT NULL REFERENCES dm_messages(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    emoji VARCHAR(32) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_dm_message_user_emoji UNIQUE (message_id, user_id, emoji)
);

COMMENT ON TABLE dm_message_reactions IS 'Per-user emoji reactions for direct-message rows.';
COMMENT ON COLUMN dm_message_reactions.message_id IS 'Direct-message row receiving the reaction.';
COMMENT ON COLUMN dm_message_reactions.user_id IS 'User who applied the reaction.';
COMMENT ON COLUMN dm_message_reactions.emoji IS 'Emoji sequence applied as the reaction.';
COMMENT ON COLUMN dm_message_reactions.created_at IS 'Time the reaction was first applied.';

CREATE INDEX idx_dm_message_reactions_message_emoji_created
    ON dm_message_reactions (message_id, emoji, created_at);
