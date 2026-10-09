-- New settings rows default to mutual servers. Existing choices, including
-- stored 'everyone' values, remain unchanged. Rows are created lazily, so the
-- application's no-row fallback changes in the same release.
ALTER TABLE privacy_settings
    ALTER COLUMN allow_friend_requests_from SET DEFAULT 'mutual_servers';
