-- Add description, emoji, and position fields to channels table
ALTER TABLE channels
ADD COLUMN IF NOT EXISTS description TEXT,
ADD COLUMN IF NOT EXISTS emoji VARCHAR(10),
ADD COLUMN IF NOT EXISTS position INTEGER DEFAULT 0 NOT NULL;

-- Create index on position for ordering
CREATE INDEX IF NOT EXISTS idx_channels_position ON channels(server_id, position);
