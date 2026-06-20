-- Enforce uniqueness on (message_id, position) so the position range constraint
-- truly caps attachments at 5 per message (prevents reusing positions).
CREATE UNIQUE INDEX idx_message_attachments_position ON message_attachments(message_id, position);
CREATE UNIQUE INDEX idx_dm_message_attachments_position ON dm_message_attachments(message_id, position);
