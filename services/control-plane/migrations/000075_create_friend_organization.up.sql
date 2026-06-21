-- Friend categories (#324): per-user, zero-knowledge encrypted organization blob.
-- One row per user; the server stores only AES-256-GCM ciphertext (base64) + a
-- version counter and cannot read category names, colors, emoji, OR membership.
-- Same shape as user_preferences (000016) and saved_gifs (000055).
CREATE TABLE friend_organization (
    user_id        UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    encrypted_data TEXT        NOT NULL,
    version        INTEGER     NOT NULL DEFAULT 1,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
