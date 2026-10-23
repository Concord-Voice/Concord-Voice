CREATE TABLE token_theft_pending_revocations (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    operation_id UUID,
    superseded_operation_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

ALTER TABLE users ADD COLUMN token_theft_generation BIGINT NOT NULL DEFAULT 0;

COMMENT ON TABLE token_theft_pending_revocations IS
    'Durable recovery records for refresh-token sweeps interrupted after token-theft detection.';
COMMENT ON COLUMN token_theft_pending_revocations.operation_id IS
    'Unique theft event identifier used to fence Redis cleanup against a newer theft event.';
COMMENT ON COLUMN token_theft_pending_revocations.superseded_operation_id IS
    'Redis fallback present when this sweep began; cleanup may retire that exact older event.';
COMMENT ON COLUMN token_theft_pending_revocations.completed_at IS
    'Refresh-token sweep committed; the row remains until Redis deny-state cleanup succeeds.';
COMMENT ON COLUMN users.token_theft_generation IS
    'Durable fence for WebSocket registration racing a completed token-theft sweep.';
