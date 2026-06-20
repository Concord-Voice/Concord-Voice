-- Friend codes (modeled after server_invites from 000009)
CREATE TABLE friend_codes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code VARCHAR(8) NOT NULL UNIQUE,
    max_uses INTEGER DEFAULT 1,
    use_count INTEGER NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ,
    is_revoked BOOLEAN NOT NULL DEFAULT FALSE,
    auto_accept BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_friend_codes_code ON friend_codes(code);
CREATE INDEX idx_friend_codes_user ON friend_codes(user_id);

-- Privacy settings (server-queryable — separate from encrypted user_preferences blob)
CREATE TABLE privacy_settings (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    messages_friends_only BOOLEAN NOT NULL DEFAULT TRUE,
    messages_server_members BOOLEAN NOT NULL DEFAULT TRUE,
    auto_accept_friend_codes BOOLEAN NOT NULL DEFAULT FALSE,
    searchable_by_username BOOLEAN NOT NULL DEFAULT FALSE,
    searchable_by_email BOOLEAN NOT NULL DEFAULT FALSE,
    searchable_by_phone BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Personal Thread marker on dm_conversations
ALTER TABLE dm_conversations ADD COLUMN is_personal BOOLEAN NOT NULL DEFAULT FALSE;
