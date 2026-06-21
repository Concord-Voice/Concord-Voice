-- Server-level embed policy: controls whether embedded content is allowed
-- in this server. Default OFF (embeds suppressed). Only the server policy
-- can set embeds_suppressed = false on new messages.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS allow_embedded_content BOOLEAN NOT NULL DEFAULT FALSE;

-- Per-message embed suppression flag. Stamped by the server at send time
-- based on the server's allow_embedded_content policy. Moderators can
-- suppress (false → true) but never un-suppress (true → false).
-- Default TRUE (suppressed) — safe default if not explicitly stamped.
ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS embeds_suppressed BOOLEAN NOT NULL DEFAULT TRUE;
