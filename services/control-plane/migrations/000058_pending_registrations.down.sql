DROP INDEX IF EXISTS idx_pending_registrations_username_lower;
DROP INDEX IF EXISTS idx_pending_registrations_email_lower;
DROP INDEX IF EXISTS idx_pending_registrations_expires_at;
DROP TABLE IF EXISTS pending_registrations;
