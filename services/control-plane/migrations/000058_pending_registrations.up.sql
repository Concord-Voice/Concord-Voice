-- Pending registrations: hold unconfirmed signup data for 15 minutes while
-- the user verifies their email. Promoted atomically to users + user_keys +
-- public_keys on successful verification; deleted on expiry or abandon.
-- Closes #621 (no permanent user row before email verification).

CREATE TABLE pending_registrations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(255) NOT NULL,
    username VARCHAR(50) NOT NULL,
    password_hash TEXT NOT NULL,
    wrapped_private_key BYTEA NOT NULL,
    key_derivation_salt BYTEA NOT NULL,
    key_derivation_alg TEXT NOT NULL DEFAULT 'argon2id',
    public_key BYTEA NOT NULL,
    e2ee_preference BOOLEAN NOT NULL DEFAULT TRUE,
    age_verified BOOLEAN NOT NULL DEFAULT TRUE,
    resend_count INTEGER NOT NULL DEFAULT 0,
    last_resend_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (email),
    UNIQUE (username)
);

CREATE INDEX idx_pending_registrations_expires_at
    ON pending_registrations(expires_at);
CREATE INDEX idx_pending_registrations_email_lower
    ON pending_registrations(LOWER(email));
CREATE INDEX idx_pending_registrations_username_lower
    ON pending_registrations(LOWER(username));
