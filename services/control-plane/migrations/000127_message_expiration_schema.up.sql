ALTER TABLE messages
  ADD COLUMN expires_at TIMESTAMPTZ NULL;

ALTER TABLE dm_messages
  ADD COLUMN expires_at TIMESTAMPTZ NULL;

ALTER TABLE channels
  ADD COLUMN expiration_window_seconds INTEGER NULL
    CHECK (expiration_window_seconds IN (3600, 86400, 604800, 2592000)),
  ADD COLUMN expiration_updated_at TIMESTAMPTZ NULL,
  ADD COLUMN expiration_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN expiration_backfill_mode TEXT NULL
    CHECK (expiration_backfill_mode IN ('apply', 'clear')),
  ADD COLUMN expiration_backfill_cutoff TIMESTAMPTZ NULL;

ALTER TABLE dm_conversations
  ADD COLUMN expiration_window_seconds INTEGER NULL
    CHECK (expiration_window_seconds IN (3600, 86400, 604800, 2592000)),
  ADD COLUMN expiration_updated_at TIMESTAMPTZ NULL,
  ADD COLUMN expiration_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN expiration_backfill_mode TEXT NULL
    CHECK (expiration_backfill_mode IN ('apply', 'clear')),
  ADD COLUMN expiration_backfill_cutoff TIMESTAMPTZ NULL;

COMMENT ON COLUMN messages.expires_at IS 'Optional server-side expiry timestamp for this message.';
COMMENT ON COLUMN dm_messages.expires_at IS 'Optional server-side expiry timestamp for this DM message.';

COMMENT ON COLUMN channels.expiration_window_seconds IS 'Shared message expiry window in seconds; NULL disables expiry stamps.';
COMMENT ON COLUMN channels.expiration_updated_at IS 'Time the current channel expiry policy was accepted.';
COMMENT ON COLUMN channels.expiration_revision IS 'Monotonic generation for channel expiry policy operations.';
COMMENT ON COLUMN channels.expiration_backfill_mode IS 'Active channel expiry backfill operation, if any.';
COMMENT ON COLUMN channels.expiration_backfill_cutoff IS 'Immutable cutoff bounding the active channel expiry backfill.';

COMMENT ON COLUMN dm_conversations.expiration_window_seconds IS 'Shared message expiry window in seconds; NULL disables expiry stamps.';
COMMENT ON COLUMN dm_conversations.expiration_updated_at IS 'Time the current DM expiry policy was accepted.';
COMMENT ON COLUMN dm_conversations.expiration_revision IS 'Monotonic generation for DM expiry policy operations.';
COMMENT ON COLUMN dm_conversations.expiration_backfill_mode IS 'Active DM expiry backfill operation, if any.';
COMMENT ON COLUMN dm_conversations.expiration_backfill_cutoff IS 'Immutable cutoff bounding the active DM expiry backfill.';
