-- Bridge table linking channel messages to media_files (supports up to 5 attachments per message)
CREATE TABLE message_attachments (
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    file_id    UUID NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
    position   SMALLINT NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, file_id),
    CONSTRAINT max_position CHECK (position >= 0 AND position < 5)
);

CREATE INDEX idx_message_attachments_file ON message_attachments(file_id);
CREATE INDEX idx_message_attachments_message ON message_attachments(message_id);

-- Bridge table linking DM messages to media_files
CREATE TABLE dm_message_attachments (
    message_id UUID NOT NULL REFERENCES dm_messages(id) ON DELETE CASCADE,
    file_id    UUID NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
    position   SMALLINT NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, file_id),
    CONSTRAINT dm_max_position CHECK (position >= 0 AND position < 5)
);

CREATE INDEX idx_dm_message_attachments_file ON dm_message_attachments(file_id);
CREATE INDEX idx_dm_message_attachments_message ON dm_message_attachments(message_id);
