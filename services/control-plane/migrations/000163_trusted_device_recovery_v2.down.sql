-- WARNING: rollback drains expired/terminal ceremonies and restores only the
-- empty original schema. It cannot resurrect raw JTIs or approval ciphertext.
-- Keep legacy refusal in the rollback binary; never deploy vulnerable v1 code.
-- The whole file runs in one implicit transaction, including the refusal guard.
LOCK TABLE recovery_requests IN ACCESS EXCLUSIVE MODE;

DO $migration$
DECLARE
    rollback_clock TIMESTAMPTZ := clock_timestamp();
BEGIN
    -- The column check also permits a harmless replay after this down completed.
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
            AND table_name = 'recovery_requests'
            AND column_name = 'protocol_version'
    ) THEN
        IF EXISTS (
            SELECT 1 FROM recovery_requests
            WHERE protocol_version = 2
                AND status IN ('pending', 'offered', 'approved')
                AND expires_at > rollback_clock
        ) THEN
            RAISE EXCEPTION 'cannot roll back trusted recovery v2 while unexpired ceremonies remain; wait for expiry or finish/reject requests';
        END IF;
    END IF;
END;
$migration$;

DELETE FROM recovery_requests
WHERE status IN ('rejected', 'expired', 'complete')
    OR expires_at <= clock_timestamp();

-- Fail closed if an unexpected row survived rather than fabricating legacy data.
DO $migration$
BEGIN
    IF EXISTS (SELECT 1 FROM recovery_requests) THEN
        RAISE EXCEPTION 'cannot restore the legacy recovery schema while any ceremony remains';
    END IF;
END;
$migration$;

ALTER TABLE recovery_requests
    DROP CONSTRAINT IF EXISTS recovery_requests_protocol_version_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_status_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_server_origin_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_account_binding_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_requester_nonce_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_jti_hash_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_requester_key_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_responder_key_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_responder_nonce_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_transcript_hash_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_expiry_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_offer_fields_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_offer_status_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_payload_check,
    DROP CONSTRAINT IF EXISTS recovery_requests_completion_check,
    DROP COLUMN IF EXISTS protocol_version,
    DROP COLUMN IF EXISTS server_origin,
    DROP COLUMN IF EXISTS account_binding,
    DROP COLUMN IF EXISTS requester_nonce,
    DROP COLUMN IF EXISTS recovery_token_jti_hash,
    DROP COLUMN IF EXISTS responder_nonce,
    DROP COLUMN IF EXISTS transcript_hash,
    DROP COLUMN IF EXISTS offered_at,
    DROP COLUMN IF EXISTS completed_at,
    ADD COLUMN IF NOT EXISTS recovery_token_jti TEXT NOT NULL,
    ALTER COLUMN created_at SET DEFAULT NOW(),
    ADD CONSTRAINT recovery_requests_status_check
        CHECK (status IN ('pending', 'approved', 'rejected', 'complete'));
