-- #89: Machine ID for token theft detection
ALTER TABLE refresh_tokens ADD COLUMN machine_id VARCHAR(255);

-- #96: Key version on messages for forward secrecy
ALTER TABLE messages ADD COLUMN key_version INTEGER DEFAULT 1;
ALTER TABLE dm_messages ADD COLUMN key_version INTEGER DEFAULT 1;

-- #96: Key Revocation Authority — epoch transition log
CREATE TABLE key_revocations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    revoked_epoch INTEGER NOT NULL,
    successor_epoch INTEGER NOT NULL,
    reason TEXT NOT NULL,
    revoked_by UUID REFERENCES users(id),
    revoked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(channel_id, revoked_epoch)
);
CREATE INDEX idx_key_revocations_channel ON key_revocations(channel_id);
