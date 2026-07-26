-- Durable cross-replica denial state for Rich Presence after a user disconnects.
-- Redis remains the low-latency status source, while this fence prevents a stale
-- visible Redis value from re-authorizing activity after a failed disconnect write.
CREATE TABLE presence_offline_fences (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    offline_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE presence_offline_fences IS
    'Durable Rich Presence emission-denial fences for disconnected users.';
COMMENT ON COLUMN presence_offline_fences.user_id IS
    'Disconnected user whose Rich Presence must remain suppressed until a new visible presence write succeeds.';
COMMENT ON COLUMN presence_offline_fences.offline_at IS
    'Time the durable offline denial fence was recorded.';
COMMENT ON COLUMN presence_offline_fences.created_at IS
    'Time the offline denial fence was first created.';
COMMENT ON COLUMN presence_offline_fences.updated_at IS
    'Schema audit timestamp set when the offline denial fence is created.';
