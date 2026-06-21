-- Remove description, emoji, and position fields from channels table
DROP INDEX IF EXISTS idx_channels_position;

ALTER TABLE channels
DROP COLUMN IF EXISTS position,
DROP COLUMN IF EXISTS emoji,
DROP COLUMN IF EXISTS description;
