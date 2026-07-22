-- These validations are no-ops on the normal forward path and restore the
-- proofs recreated NOT VALID by migration 000097 down. Keep both before the
-- first ACCESS EXCLUSIVE operation so no validation scan holds that lock.
ALTER TABLE voice_participants
    VALIDATE CONSTRAINT voice_participants_lifecycle_event_at_not_null;

ALTER TABLE dm_voice_participants
    VALIDATE CONSTRAINT dm_voice_participants_lifecycle_event_at_not_null;

-- The validated checks let PostgreSQL make SET NOT NULL metadata-only. Keep
-- each proof constraint until its column conversion completes.
ALTER TABLE voice_participants
    ALTER COLUMN lifecycle_event_at SET NOT NULL;
ALTER TABLE voice_participants
    DROP CONSTRAINT voice_participants_lifecycle_event_at_not_null;

ALTER TABLE dm_voice_participants
    ALTER COLUMN lifecycle_event_at SET NOT NULL;
ALTER TABLE dm_voice_participants
    DROP CONSTRAINT dm_voice_participants_lifecycle_event_at_not_null;
