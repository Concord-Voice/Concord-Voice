ALTER TABLE server_voice_terminal_outbox
    DROP COLUMN IF EXISTS delivery_claim_until,
    DROP COLUMN IF EXISTS delivery_claim_id;
