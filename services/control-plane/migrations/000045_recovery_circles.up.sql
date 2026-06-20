-- Social Recovery: Shamir's Secret Sharing recovery circles (#200 Phase C)

CREATE TABLE IF NOT EXISTS recovery_circles (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    threshold_k     INTEGER NOT NULL,
    total_shares_n  INTEGER NOT NULL,
    share_version   INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(user_id),
    CONSTRAINT recovery_circles_threshold_positive CHECK (threshold_k >= 2),
    CONSTRAINT recovery_circles_total_positive CHECK (total_shares_n >= 2),
    CONSTRAINT recovery_circles_threshold_le_total CHECK (threshold_k <= total_shares_n),
    CONSTRAINT recovery_circles_max_shares CHECK (total_shares_n <= 7)
);

CREATE TABLE IF NOT EXISTS recovery_circle_shares (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    circle_id       UUID NOT NULL REFERENCES recovery_circles(id) ON DELETE CASCADE,
    contact_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    share_index     INTEGER NOT NULL CHECK (share_index >= 1),
    encrypted_share BYTEA NOT NULL,
    share_version   INTEGER NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(circle_id, contact_id, share_version),
    UNIQUE(circle_id, share_index, share_version)
);
CREATE INDEX IF NOT EXISTS idx_recovery_shares_contact ON recovery_circle_shares(contact_id);

CREATE TABLE IF NOT EXISTS recovery_circle_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    circle_id       UUID NOT NULL REFERENCES recovery_circles(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    recovery_token_jti TEXT NOT NULL,
    ephemeral_public_key BYTEA NOT NULL,
    status          VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'complete', 'expired')),
    shares_received INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS recovery_circle_responses (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id      UUID NOT NULL REFERENCES recovery_circle_requests(id) ON DELETE CASCADE,
    contact_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    encrypted_share BYTEA NOT NULL,
    responded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(request_id, contact_id)
);
