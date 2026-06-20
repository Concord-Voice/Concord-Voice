-- Trusted Device Recovery: Desktop-to-desktop key transfer for account recovery (#200 Phase B)

CREATE TABLE IF NOT EXISTS trusted_recovery_devices (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name     VARCHAR(255) NOT NULL,
    machine_id      VARCHAR(255) NOT NULL,
    designated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at    TIMESTAMPTZ,
    UNIQUE(user_id, machine_id)
);
CREATE INDEX IF NOT EXISTS idx_trusted_devices_user ON trusted_recovery_devices(user_id);

CREATE TABLE IF NOT EXISTS recovery_requests (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    recovery_token_jti   TEXT NOT NULL,
    ephemeral_public_key BYTEA NOT NULL,
    status               VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'complete')),
    encrypted_payload    BYTEA,
    responder_public_key BYTEA,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ NOT NULL,
    responded_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_recovery_requests_user ON recovery_requests(user_id, status);
