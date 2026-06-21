-- Encrypted user preferences: opaque client-encrypted blob per user.
-- The server cannot read the contents (AES-256-GCM encrypted, base64 encoded).
-- Used for cross-device sync of UI settings (theme, layout, folders, etc.).
CREATE TABLE user_preferences (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    encrypted_data TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
