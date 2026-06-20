-- DM System: friendships, conversations, messages, read states, E2EE keys, voice participants

-- Friendships (bidirectional relationships)
CREATE TABLE friendships (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    addressee_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',  -- pending, accepted, blocked
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (requester_id, addressee_id),
    CHECK (requester_id != addressee_id)
);

CREATE INDEX idx_friendships_addressee ON friendships(addressee_id);
CREATE INDEX idx_friendships_status ON friendships(status);

-- DM Conversations
CREATE TABLE dm_conversations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    is_group BOOLEAN NOT NULL DEFAULT FALSE,
    name VARCHAR(100),            -- NULL for 1:1, set for group DMs
    is_encrypted BOOLEAN NOT NULL DEFAULT TRUE,
    created_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- DM Participants
CREATE TABLE dm_participants (
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (conversation_id, user_id)
);

CREATE INDEX idx_dm_participants_user ON dm_participants(user_id);

-- DM Messages (mirrors messages table structure)
CREATE TABLE dm_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    is_encrypted BOOLEAN NOT NULL DEFAULT FALSE,
    type VARCHAR(20) NOT NULL DEFAULT 'user',  -- 'user', 'system' (call events, etc.)
    edited_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_dm_messages_conversation ON dm_messages(conversation_id, created_at DESC);
CREATE INDEX idx_dm_messages_user ON dm_messages(user_id);

-- DM Read States
CREATE TABLE dm_read_states (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    last_read_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, conversation_id)
);

-- DM E2EE Keys (mirrors channel_keys)
CREATE TABLE dm_channel_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    wrapped_key TEXT NOT NULL,
    key_version INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (conversation_id, user_id, key_version)
);

CREATE INDEX idx_dm_channel_keys_conv_user ON dm_channel_keys(conversation_id, user_id);

-- DM Pending Key Requests (mirrors pending_key_requests)
CREATE TABLE dm_pending_key_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (conversation_id, user_id)
);

CREATE INDEX idx_dm_pending_key_requests_user ON dm_pending_key_requests(user_id);

-- DM Voice Participants (mirrors voice_participants)
CREATE TABLE dm_voice_participants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_muted BOOLEAN NOT NULL DEFAULT FALSE,
    is_deafened BOOLEAN NOT NULL DEFAULT FALSE,
    is_video_on BOOLEAN NOT NULL DEFAULT FALSE,
    is_screen_sharing BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (conversation_id, user_id)
);

CREATE INDEX idx_dm_voice_participants_conversation ON dm_voice_participants(conversation_id);
CREATE INDEX idx_dm_voice_participants_user ON dm_voice_participants(user_id);
