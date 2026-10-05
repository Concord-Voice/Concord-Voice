-- Trusted recovery v2 replaces short-lived ceremonies, never account/key records.
-- WARNING: all existing requests must restart; discarded v1 material is not retained.
-- The migration runner sends this whole file as one implicit transaction.
LOCK TABLE recovery_requests IN ACCESS EXCLUSIVE MODE;

DELETE FROM recovery_requests;

ALTER TABLE recovery_requests
    DROP CONSTRAINT recovery_requests_status_check,
    DROP COLUMN recovery_token_jti,
    ALTER COLUMN created_at SET DEFAULT clock_timestamp(),
    ADD COLUMN protocol_version SMALLINT NOT NULL,
    ADD COLUMN server_origin TEXT NOT NULL,
    ADD COLUMN account_binding BYTEA NOT NULL,
    ADD COLUMN requester_nonce BYTEA NOT NULL,
    ADD COLUMN recovery_token_jti_hash BYTEA NOT NULL,
    ADD COLUMN responder_nonce BYTEA,
    ADD COLUMN transcript_hash BYTEA,
    ADD COLUMN offered_at TIMESTAMPTZ,
    ADD COLUMN completed_at TIMESTAMPTZ,
    ADD CONSTRAINT recovery_requests_protocol_version_check
        CHECK (protocol_version = 2),
    ADD CONSTRAINT recovery_requests_status_check
        CHECK (status IN ('pending', 'offered', 'approved', 'rejected', 'expired', 'complete')),
    ADD CONSTRAINT recovery_requests_server_origin_check
        CHECK (octet_length(server_origin) BETWEEN 1 AND 512),
    ADD CONSTRAINT recovery_requests_account_binding_check
        CHECK (octet_length(account_binding) = 32),
    ADD CONSTRAINT recovery_requests_requester_nonce_check
        CHECK (octet_length(requester_nonce) = 32),
    ADD CONSTRAINT recovery_requests_jti_hash_check
        CHECK (octet_length(recovery_token_jti_hash) = 32),
    ADD CONSTRAINT recovery_requests_requester_key_check
        CHECK (octet_length(ephemeral_public_key) = 97
            AND substring(ephemeral_public_key FROM 1 FOR 1) = decode('04', 'hex')),
    ADD CONSTRAINT recovery_requests_responder_key_check
        CHECK (responder_public_key IS NULL
            OR (octet_length(responder_public_key) = 97
                AND substring(responder_public_key FROM 1 FOR 1) = decode('04', 'hex'))),
    ADD CONSTRAINT recovery_requests_responder_nonce_check
        CHECK (responder_nonce IS NULL OR octet_length(responder_nonce) = 32),
    ADD CONSTRAINT recovery_requests_transcript_hash_check
        CHECK (transcript_hash IS NULL OR octet_length(transcript_hash) = 32),
    ADD CONSTRAINT recovery_requests_expiry_check
        CHECK (isfinite(created_at) AND isfinite(expires_at)
            AND expires_at > created_at
            AND expires_at <= created_at + INTERVAL '15 minutes'),
    ADD CONSTRAINT recovery_requests_offer_fields_check
        CHECK (
            (responder_public_key IS NULL AND responder_nonce IS NULL
                AND transcript_hash IS NULL AND offered_at IS NULL)
            OR
            (responder_public_key IS NOT NULL AND responder_nonce IS NOT NULL
                AND transcript_hash IS NOT NULL AND offered_at IS NOT NULL)
        ),
    ADD CONSTRAINT recovery_requests_offer_status_check
        CHECK (
            (status = 'pending' AND offered_at IS NULL)
            OR (status IN ('offered', 'approved', 'complete') AND offered_at IS NOT NULL)
            OR status IN ('rejected', 'expired')
        ),
    ADD CONSTRAINT recovery_requests_payload_check
        CHECK (
            (status = 'approved' AND encrypted_payload IS NOT NULL
                AND octet_length(encrypted_payload) BETWEEN 30 AND 8192
                AND substring(encrypted_payload FROM 1 FOR 1) = decode('02', 'hex'))
            OR (status <> 'approved' AND encrypted_payload IS NULL)
        ),
    ADD CONSTRAINT recovery_requests_completion_check
        CHECK (
            (status = 'complete' AND completed_at IS NOT NULL)
            OR (status <> 'complete' AND completed_at IS NULL)
        );

-- The existing owner FK (ON DELETE CASCADE) and user/status index are retained.
-- Exact canonical origin, curve membership, JWT expiry and immutable transitions
-- are verified by the application; CHECK constraints describe one row only.
COMMENT ON COLUMN recovery_requests.protocol_version IS
    'Trusted-device recovery protocol; only v2 ceremonies are accepted.';
COMMENT ON COLUMN recovery_requests.server_origin IS
    'Canonical selected HTTP(S) API origin bound into the v2 transcript.';
COMMENT ON COLUMN recovery_requests.account_binding IS
    '32-byte SHA-256 binding to the canonical owner UUID, never an email address.';
COMMENT ON COLUMN recovery_requests.requester_nonce IS
    'Fresh 32-byte requester nonce bound into the v2 transcript.';
COMMENT ON COLUMN recovery_requests.recovery_token_jti_hash IS
    '32-byte SHA-256 of the verified recovery JWT JTI; raw JTI is never stored.';
COMMENT ON COLUMN recovery_requests.responder_nonce IS
    'Fresh 32-byte responder nonce, fixed together with the first accepted offer.';
COMMENT ON COLUMN recovery_requests.transcript_hash IS
    '32-byte server-computed canonical v2 transcript digest fixed at offer.';
COMMENT ON COLUMN recovery_requests.offered_at IS
    'Database wall-clock time of the first accepted immutable responder offer.';
COMMENT ON COLUMN recovery_requests.completed_at IS
    'Database wall-clock time of requester acknowledgement after confirmed key import.';
