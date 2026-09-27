-- Durable, database-assigned revision watermark for voice authorization snapshots.
-- The application connects as the migration/runtime database user, so grant the
-- privileges explicitly instead of relying on sequence-owner defaults.
CREATE SEQUENCE public.voice_authorization_revision_seq AS BIGINT;

GRANT USAGE, SELECT
    ON SEQUENCE public.voice_authorization_revision_seq
    TO current_user;

COMMENT ON SEQUENCE public.voice_authorization_revision_seq IS
    'Monotonic authorization revision allocated for voice admission snapshots.';
