-- Reply/Quote Messages: nullable FK to the message being replied to.
-- ON DELETE SET NULL ensures replies survive when the original is deleted.
ALTER TABLE messages ADD COLUMN reply_to_id UUID REFERENCES messages(id) ON DELETE SET NULL;

-- Partial index: only rows that are replies. Used for batch-loading replied-to summaries.
-- For large production tables, run the index creation CONCURRENTLY outside a transaction.
CREATE INDEX idx_messages_reply_to_id ON messages(reply_to_id) WHERE reply_to_id IS NOT NULL;
