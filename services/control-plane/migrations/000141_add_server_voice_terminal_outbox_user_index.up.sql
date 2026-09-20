-- Migration 000140 and this migration run before code writes terminal obligations,
-- so the new outbox is empty while this transactional index is created.
CREATE INDEX IF NOT EXISTS idx_server_voice_terminal_outbox_user
    ON server_voice_terminal_outbox (user_id);
