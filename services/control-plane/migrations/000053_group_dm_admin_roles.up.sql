-- Add role column to dm_participants for group DM admin/member distinction.
ALTER TABLE dm_participants ADD COLUMN role VARCHAR(10) NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member'));

-- Add icon_url column to dm_conversations for group icons.
ALTER TABLE dm_conversations ADD COLUMN icon_url VARCHAR(512);

-- Backfill: set the creator of each existing group as admin.
UPDATE dm_participants dp SET role = 'admin'
FROM dm_conversations dc
WHERE dp.conversation_id = dc.id
  AND dp.user_id = dc.created_by
  AND dc.is_group = TRUE;

-- Index for efficient role lookups within a conversation.
CREATE INDEX idx_dm_participants_role ON dm_participants(conversation_id, role);
