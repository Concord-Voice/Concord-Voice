-- #89: MFA (TOTP + WebAuthn) framework

-- TOTP secrets (one per user, encrypted at rest via AES-256-GCM)
CREATE TABLE user_mfa_totp (
    user_id           UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    totp_secret_enc   BYTEA NOT NULL,                        -- AES-256-GCM ciphertext of TOTP secret
    totp_secret_nonce BYTEA NOT NULL,                        -- 12-byte GCM nonce
    enabled           BOOLEAN NOT NULL DEFAULT FALSE,        -- true after code verified
    confirmed         BOOLEAN NOT NULL DEFAULT FALSE,        -- true after backup codes acknowledged
    backup_codes_hash TEXT[] NOT NULL DEFAULT '{}',           -- SHA-256 hashes of 8 one-time codes
    backup_codes_used BOOLEAN[] NOT NULL DEFAULT '{}',        -- parallel array: which codes are consumed
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    verified_at       TIMESTAMPTZ,                           -- when first code was verified
    confirmed_at      TIMESTAMPTZ,                           -- when backup codes were acknowledged
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- WebAuthn credentials (multiple per user — can register several keys/passkeys)
CREATE TABLE user_mfa_webauthn (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id     BYTEA NOT NULL UNIQUE,                 -- raw credential ID from authenticator
    public_key        BYTEA NOT NULL,                        -- COSE public key (stored as-is, it's public)
    aaguid            BYTEA,                                 -- authenticator attestation GUID
    sign_count        BIGINT NOT NULL DEFAULT 0,             -- monotonic counter for replay detection
    credential_name   VARCHAR(100) NOT NULL DEFAULT 'Security Key',
    credential_type   VARCHAR(20) NOT NULL DEFAULT 'hardware', -- 'hardware' or 'platform'
    transports        TEXT[] NOT NULL DEFAULT '{}',           -- e.g. {'usb', 'nfc', 'ble', 'internal'}
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at      TIMESTAMPTZ
);
CREATE INDEX idx_user_mfa_webauthn_user_id ON user_mfa_webauthn(user_id);
CREATE INDEX idx_user_mfa_webauthn_credential_id ON user_mfa_webauthn(credential_id);

-- Denormalized flags on users for fast login-path lookups (avoid JOINs on hot path)
ALTER TABLE users ADD COLUMN mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users ADD COLUMN mfa_methods TEXT[] NOT NULL DEFAULT '{}';
-- NOTE: recovery_only_methods and recovery_hardened are in migration 000030
