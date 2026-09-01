-- A rollback to 000119 may discard only immutable generations that are no
-- longer live; their permanent deletion tombstones remain intact.  It must not
-- lose a pending upload or a live generation that 000119 cannot represent.
DO $$
DECLARE
    has_unsafe_state BOOLEAN;
BEGIN
    BEGIN
        LOCK TABLE tier1_profile_upload_intents IN ACCESS EXCLUSIVE MODE;
        LOCK TABLE media_files IN ACCESS EXCLUSIVE MODE;
    EXCEPTION
        WHEN undefined_table THEN
            RETURN;
    END;

    SELECT EXISTS (SELECT 1 FROM tier1_profile_upload_intents)
        OR EXISTS (
            SELECT 1
            FROM media_files
            WHERE deleted_at IS NULL
              AND profile_slot IS NOT NULL
              AND storage_key NOT IN (
                  'avatars/' || uploader_id::text,
                  'banners/' || uploader_id::text
              )
        )
    INTO has_unsafe_state;
    IF has_unsafe_state THEN
        RAISE EXCEPTION
            'cannot roll back 000120 while profile upload intents or live immutable profile generations remain';
    END IF;
END
$$;

-- Restore 000119's exact table before dropping 000120 state so its down
-- migration remains valid on the resulting schema.
CREATE TABLE tier1_profile_clear_delete_obligations (
    user_id         UUID         NOT NULL,
    storage_key     VARCHAR(500) NOT NULL,
    attempts        INTEGER      NOT NULL DEFAULT 0,
    reconcile_after TIMESTAMPTZ  NOT NULL DEFAULT clock_timestamp(),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT clock_timestamp(),

    CONSTRAINT tier1_profile_clear_delete_obligations_pkey
        PRIMARY KEY (user_id, storage_key),
    CONSTRAINT tier1_profile_clear_delete_obligations_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT tier1_profile_clear_delete_obligations_attempts_check
        CHECK (attempts >= 0),
    CONSTRAINT tier1_profile_clear_delete_obligations_storage_key_check
        CHECK (storage_key IN (
            'avatars/' || user_id::text,
            'banners/' || user_id::text
        ))
);

CREATE INDEX idx_tier1_profile_clear_delete_obligations_due
    ON tier1_profile_clear_delete_obligations (reconcile_after, user_id, storage_key);

DROP TABLE tier1_profile_upload_intents;
DROP INDEX idx_media_files_live_profile_slot;
ALTER TABLE media_files DROP CONSTRAINT media_files_profile_slot_check;
ALTER TABLE media_files DROP COLUMN profile_slot;
