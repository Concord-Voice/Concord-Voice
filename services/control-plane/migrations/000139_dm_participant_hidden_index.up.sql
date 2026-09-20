CREATE INDEX CONCURRENTLY idx_dm_participants_hidden_conversation
    ON dm_participants (conversation_id)
    WHERE hidden_at IS NOT NULL;
