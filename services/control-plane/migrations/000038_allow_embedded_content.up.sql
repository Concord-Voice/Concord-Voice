-- Add user-level embedded content preference to privacy_settings.
-- Default OFF — users must opt-in to render link previews, image embeds, etc.
-- This protects against off-app beacons (IP leaks from fetching external URLs).
-- Even when enabled, server moderators can still suppress embeds per-message
-- via the PermManageAllMessages permission.
ALTER TABLE privacy_settings
    ADD COLUMN IF NOT EXISTS allow_embedded_content BOOLEAN NOT NULL DEFAULT FALSE;
