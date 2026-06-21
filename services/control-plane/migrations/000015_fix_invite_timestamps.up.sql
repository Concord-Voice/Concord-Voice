-- Fix server_invites to use timezone-aware timestamps (matching all other tables).
-- TIMESTAMP WITHOUT TIME ZONE caused timezone roundtrip bugs where short-expiry
-- invites appeared expired immediately due to timezone offset being stripped.
ALTER TABLE server_invites
    ALTER COLUMN expires_at TYPE TIMESTAMPTZ USING expires_at AT TIME ZONE 'UTC',
    ALTER COLUMN created_at TYPE TIMESTAMPTZ USING created_at AT TIME ZONE 'UTC',
    ALTER COLUMN created_at SET DEFAULT NOW();
