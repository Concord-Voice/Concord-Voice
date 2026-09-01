-- Replace the transient deterministic-key clear rail from 000119.  Its retry
-- evidence becomes permanent before the table is removed: a late object-store
-- PUT may still materialize an old deterministic key after this migration.
INSERT INTO tier1_erasure_delete_obligations (
    storage_key, attempts, reconcile_after, created_at
)
SELECT storage_key, attempts, reconcile_after, created_at
FROM tier1_profile_clear_delete_obligations
ON CONFLICT (storage_key) DO UPDATE
SET attempts = GREATEST(
        tier1_erasure_delete_obligations.attempts,
        EXCLUDED.attempts
    ),
    reconcile_after = LEAST(
        tier1_erasure_delete_obligations.reconcile_after,
        EXCLUDED.reconcile_after
    ),
    created_at = LEAST(
        tier1_erasure_delete_obligations.created_at,
        EXCLUDED.created_at
    );

DROP TABLE tier1_profile_clear_delete_obligations;

-- A canonical profile URL resolves through this slot to an immutable physical
-- generation.  Backfill the legacy deterministic rows so existing profiles
-- remain visible immediately after deployment.
ALTER TABLE media_files ADD COLUMN profile_slot VARCHAR(6);

UPDATE media_files
SET profile_slot = CASE
    WHEN storage_key = 'avatars/' || uploader_id::text THEN 'avatar'
    WHEN storage_key = 'banners/' || uploader_id::text THEN 'banner'
END
WHERE media_tier = 1
  AND deleted_at IS NULL
  AND storage_key IN ('avatars/' || uploader_id::text, 'banners/' || uploader_id::text);

ALTER TABLE media_files ADD CONSTRAINT media_files_profile_slot_check CHECK (
    profile_slot IS NULL
    OR (media_tier = 1 AND profile_slot IN ('avatar', 'banner'))
);

CREATE UNIQUE INDEX idx_media_files_live_profile_slot
    ON media_files (uploader_id, profile_slot)
    WHERE deleted_at IS NULL AND profile_slot IS NOT NULL;

-- Commit this row before PutObject.  RESTRICT makes account erasure fail
-- closed until every potentially materialized immutable object is terminalized.
CREATE TABLE tier1_profile_upload_intents (
    storage_key  VARCHAR(500) PRIMARY KEY,
    user_id      UUID         NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    profile_slot VARCHAR(6)   NOT NULL CHECK (profile_slot IN ('avatar', 'banner')),
    expires_at   TIMESTAMPTZ  NOT NULL DEFAULT (clock_timestamp() + INTERVAL '1 minute'),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT clock_timestamp(),

    CONSTRAINT tier1_profile_upload_intents_key_shape_check CHECK (
        (profile_slot = 'avatar' AND storage_key LIKE 'avatars/' || user_id::text || '/%')
        OR
        (profile_slot = 'banner' AND storage_key LIKE 'banners/' || user_id::text || '/%')
    )
);

CREATE INDEX idx_tier1_profile_upload_intents_due
    ON tier1_profile_upload_intents (expires_at, user_id, profile_slot);
CREATE INDEX idx_tier1_profile_upload_intents_user_slot
    ON tier1_profile_upload_intents (user_id, profile_slot);

COMMENT ON TABLE tier1_profile_upload_intents IS
    'Durable pre-PutObject evidence for immutable avatar/banner generations; expiry or terminalization transfers the key to permanent deletion obligations.';
COMMENT ON COLUMN media_files.profile_slot IS
    'Canonical profile endpoint slot for an immutable avatar or banner object.';
