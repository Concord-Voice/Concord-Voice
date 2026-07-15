-- Reverses 000090 in dependency order. Note: dropping blob_reaped_at discards the
-- reap markers; a subsequent re-up backfills every already-soft-deleted row as
-- reaped, which is the same assumption the legacy CleanupObject path made.
DROP INDEX IF EXISTS idx_media_files_unreaped;
ALTER TABLE media_files DROP COLUMN IF EXISTS blob_reaped_at;
DROP INDEX IF EXISTS idx_dm_hidden_ranges_user_conv;
DROP TABLE IF EXISTS dm_message_hidden_ranges;
ALTER TABLE privacy_settings DROP COLUMN IF EXISTS require_auth_before_purge;
DROP INDEX IF EXISTS idx_message_purges_created_at;
DROP INDEX IF EXISTS idx_message_purges_context;
DROP TABLE IF EXISTS message_purges;
