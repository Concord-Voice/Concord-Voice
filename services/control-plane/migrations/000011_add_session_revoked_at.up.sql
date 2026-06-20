-- Add revoked_at column for soft-delete of sessions (enables "Past Sessions" UI)
ALTER TABLE refresh_tokens ADD COLUMN revoked_at TIMESTAMPTZ DEFAULT NULL;

-- Index for efficient queries: active sessions (revoked_at IS NULL) and past sessions (revoked_at IS NOT NULL)
CREATE INDEX idx_refresh_tokens_revoked ON refresh_tokens (user_id, revoked_at);
