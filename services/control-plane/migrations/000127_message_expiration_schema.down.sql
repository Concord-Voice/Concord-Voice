ALTER TABLE dm_conversations
  DROP COLUMN IF EXISTS expiration_backfill_cutoff,
  DROP COLUMN IF EXISTS expiration_backfill_mode,
  DROP COLUMN IF EXISTS expiration_revision,
  DROP COLUMN IF EXISTS expiration_updated_at,
  DROP COLUMN IF EXISTS expiration_window_seconds;

ALTER TABLE channels
  DROP COLUMN IF EXISTS expiration_backfill_cutoff,
  DROP COLUMN IF EXISTS expiration_backfill_mode,
  DROP COLUMN IF EXISTS expiration_revision,
  DROP COLUMN IF EXISTS expiration_updated_at,
  DROP COLUMN IF EXISTS expiration_window_seconds;

ALTER TABLE dm_messages DROP COLUMN IF EXISTS expires_at;
ALTER TABLE messages DROP COLUMN IF EXISTS expires_at;
