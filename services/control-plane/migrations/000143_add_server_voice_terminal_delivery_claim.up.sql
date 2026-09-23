ALTER TABLE server_voice_terminal_outbox
    ADD COLUMN delivery_claim_id UUID,
    ADD COLUMN delivery_claim_until TIMESTAMPTZ;

COMMENT ON COLUMN server_voice_terminal_outbox.delivery_claim_id IS
    'Worker instance currently claiming this terminal delivery attempt; NULL means unclaimed.';

COMMENT ON COLUMN server_voice_terminal_outbox.delivery_claim_until IS
    'Expiration of the delivery claim; an expired claim is eligible for recovery.';
