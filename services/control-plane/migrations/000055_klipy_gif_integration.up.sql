-- KLIPY GIF integration: store the per-GIF slug returned by KLIPY's API.
-- NULL = no GIF attached. Non-NULL = KLIPY slug (e.g. "happy-cat-dance").
--
-- For E2EE channels the gif_slug is encrypted inside the content blob;
-- this column stays NULL on the row.
--
-- We deliberately store ONLY the slug — never titles, URLs, dimensions, or any
-- other KLIPY-derived metadata — to comply with KLIPY ToS Section 1
-- ("you must not use content accessed through KLIPY to compile, build, or
-- expand any database, directory, or collection of GIFs").
ALTER TABLE messages ADD COLUMN gif_slug TEXT;
ALTER TABLE dm_messages ADD COLUMN gif_slug TEXT;

-- E2EE Saved GIFs — opaque encrypted blob per user (same pattern as user_preferences).
-- The encrypted blob holds ONLY {slug, saved_at} entries — no metadata.
-- The server stores only AES-256-GCM ciphertext; it cannot see which GIFs the user saved.
CREATE TABLE saved_gifs (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    encrypted_data TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
