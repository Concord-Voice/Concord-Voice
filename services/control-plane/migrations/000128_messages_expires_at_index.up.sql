CREATE INDEX CONCURRENTLY idx_messages_expires_at
    ON messages (expires_at)
    WHERE expires_at IS NOT NULL;
