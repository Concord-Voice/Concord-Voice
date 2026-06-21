-- Add email_verification_sent_at for defense-in-depth rate limiting
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verification_sent_at TIMESTAMPTZ;

-- Auto-verify all existing Alpha users (trusted testers, no disruption)
UPDATE users SET email_verified = true WHERE email_verified = false;
