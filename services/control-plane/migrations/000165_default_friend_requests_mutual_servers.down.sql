-- Restore the previous default without rewriting existing user choices.
ALTER TABLE privacy_settings
    ALTER COLUMN allow_friend_requests_from SET DEFAULT 'everyone';
