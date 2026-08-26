-- Dropping the column drops its CHECK constraint with it.
ALTER TABLE privacy_settings DROP COLUMN IF EXISTS allow_friend_requests_from;
