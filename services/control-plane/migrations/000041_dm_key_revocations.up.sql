-- #122: DM key-epoch enforcement — revocation log for DM conversations
CREATE TABLE dm_key_revocations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    revoked_epoch INTEGER NOT NULL,
    successor_epoch INTEGER NOT NULL,
    reason TEXT NOT NULL,
    revoked_by UUID REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(conversation_id, revoked_epoch),
    CHECK (successor_epoch > revoked_epoch)
);
-- UNIQUE(conversation_id, revoked_epoch) already provides the composite index
-- needed by the epoch enforcement query.
