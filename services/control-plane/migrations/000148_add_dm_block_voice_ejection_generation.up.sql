ALTER TABLE dm_block_voice_ejections
    ADD COLUMN generation UUID;

COMMENT ON COLUMN dm_block_voice_ejections.generation IS
    'Fresh UUID on every enqueue generation; stale delivery callbacks may only acknowledge or reschedule their exact generation.';
