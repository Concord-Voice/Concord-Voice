-- Account Recovery: Recovery key storage for zero-knowledge key recovery (#200)
-- Stores a second wrapping of the user's private key (encrypted with a recovery-key-derived key)
-- so the user can recover their E2EE private key without knowing their password.

CREATE TABLE IF NOT EXISTS user_recovery_keys (
    user_id                      UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    recovery_wrapped_private_key BYTEA NOT NULL,   -- AES-256-GCM(Argon2id(recoveryKey, salt), privateKey)
    recovery_key_salt            BYTEA NOT NULL,   -- 16-byte random salt for Argon2id derivation
    recovery_wrapped_prefs_key   BYTEA,            -- AES-256-GCM(Argon2id(recoveryKey, prefsSalt), prefsKey)
    recovery_prefs_key_salt      BYTEA,            -- separate salt for preferences key wrapping
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
