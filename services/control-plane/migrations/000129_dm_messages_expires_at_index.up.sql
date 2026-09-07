CREATE INDEX CONCURRENTLY idx_dm_messages_expires_at
    ON dm_messages (expires_at)
    WHERE expires_at IS NOT NULL;
