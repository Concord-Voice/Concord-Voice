-- Rich Presence Phase 1 — minimal framework slice for Custom Text Status (#1233).
-- Persistent per-user presence settings. This PR wires only the custom-text
-- columns end-to-end; other rich-presence categories are added by future
-- migrations (additive) when those features land. See
-- [internal]specs/2026-06-18-1233-custom-text-status-design.md §4.
CREATE TABLE user_presence_settings (
    user_id           UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    custom_text_tier  SMALLINT NOT NULL DEFAULT 0 CHECK (custom_text_tier IN (0, 1, 2)),
    custom_text       TEXT CHECK (custom_text IS NULL OR char_length(custom_text) <= 140),
    custom_text_emoji TEXT CHECK (custom_text_emoji IS NULL OR char_length(custom_text_emoji) <= 32),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
