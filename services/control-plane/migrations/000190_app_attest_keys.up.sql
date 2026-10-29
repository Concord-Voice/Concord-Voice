-- 000190_app_attest_keys.up.sql
-- iOS App Attest keys, ADR-0010 D17.
--
-- One row per attested Secure Enclave key. The attestation handshake writes
-- the row once; every later assertion advances sign_count, which must strictly
-- increase (Apple's replay guard). Holds no direct user identifier, matching
-- the PII-free attestation posture (ADR-0010 D7): first_user_hash is the same
-- SHA-256 digest TokenRecord.UserID carries.
CREATE TABLE app_attest_keys (
    key_id          BYTEA PRIMARY KEY
                        CONSTRAINT app_attest_keys_key_id_check CHECK (octet_length(key_id) = 32),
    public_key      BYTEA NOT NULL
                        CONSTRAINT app_attest_keys_public_key_check CHECK (octet_length(public_key) = 65),
    sign_count      BIGINT NOT NULL DEFAULT 0
                        CONSTRAINT app_attest_keys_sign_count_check CHECK (sign_count BETWEEN 0 AND 4294967295),
    environment     VARCHAR(16) NOT NULL
                        CONSTRAINT app_attest_keys_environment_check CHECK (environment IN ('production', 'development')),
    first_user_hash VARCHAR(64) NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
