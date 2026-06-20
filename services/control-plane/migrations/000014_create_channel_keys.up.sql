-- Channel symmetric keys: each row is a CSK wrapped with a specific user's RSA public key.
-- The server never sees the unwrapped key (zero-knowledge).
CREATE TABLE channel_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    wrapped_key TEXT NOT NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(channel_id, user_id, key_version)
);

CREATE INDEX idx_channel_keys_channel_user ON channel_keys(channel_id, user_id);

-- Pending key requests: tracks new members who need CSKs distributed to them.
CREATE TABLE pending_key_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(channel_id, user_id)
);

CREATE INDEX idx_pending_key_requests_user ON pending_key_requests(user_id);
