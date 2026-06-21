-- Server ownership transfer tracking.
-- Supports the full lifecycle: initiate → pending → completed/cancelled,
-- with post-completion reversal via email token.
CREATE TABLE ownership_transfers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    from_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    to_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    reversal_token VARCHAR(64),
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,        -- requested_at + 24 hours
    completed_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    reversed_at TIMESTAMPTZ,
    CONSTRAINT chk_transfer_status CHECK (status IN ('pending', 'completed', 'cancelled', 'reversed'))
);

-- Only one pending transfer per server at a time
CREATE UNIQUE INDEX idx_ownership_transfers_active
    ON ownership_transfers(server_id)
    WHERE status = 'pending';

-- Reversal token lookup (only for completed transfers with a token)
-- UNIQUE enforces one-to-one token-to-transfer mapping
CREATE UNIQUE INDEX idx_ownership_transfers_reversal_token
    ON ownership_transfers(reversal_token)
    WHERE reversal_token IS NOT NULL AND status = 'completed';

-- Expiration sweep for auto-completing pending transfers
CREATE INDEX idx_ownership_transfers_pending_expires
    ON ownership_transfers(expires_at)
    WHERE status = 'pending';
