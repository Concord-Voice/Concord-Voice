-- Message reactions: per-user emoji reactions on messages.
CREATE TABLE message_reactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    emoji VARCHAR(32) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_message_user_emoji UNIQUE (message_id, user_id, emoji)
);

-- Composite index matches batch-load ORDER BY (message_id, emoji, created_at) pattern.
-- The UNIQUE constraint already covers message_id prefix lookups.
CREATE INDEX idx_message_reactions_message_emoji_created
    ON message_reactions (message_id, emoji, created_at);
