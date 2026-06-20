DROP INDEX IF EXISTS idx_key_revocations_channel;
DROP TABLE IF EXISTS key_revocations;
ALTER TABLE dm_messages DROP COLUMN IF EXISTS key_version;
ALTER TABLE messages DROP COLUMN IF EXISTS key_version;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS machine_id;
