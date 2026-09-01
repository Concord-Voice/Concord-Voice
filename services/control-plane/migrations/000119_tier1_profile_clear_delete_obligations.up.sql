-- Durable retry evidence for ordinary profile avatar/banner clears.
-- Unlike account-erasure tombstones, these rows are tied to the surviving
-- user and cover only a transient object-store deletion failure.
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

COMMENT ON TABLE tier1_profile_clear_delete_obligations IS
    'Transient retry obligations for ordinary profile avatar/banner clears when object-store deletion fails; distinct from permanent account-erasure key tombstones.';
COMMENT ON COLUMN tier1_profile_clear_delete_obligations.user_id IS
    'The surviving profile owner whose cleared object must be deleted.';
COMMENT ON COLUMN tier1_profile_clear_delete_obligations.storage_key IS
    'Exact avatar or banner object key owed for deletion; constrained to this user profile.';
COMMENT ON COLUMN tier1_profile_clear_delete_obligations.attempts IS
    'Number of reconciliation attempts; never negative.';
COMMENT ON COLUMN tier1_profile_clear_delete_obligations.reconcile_after IS
    'Earliest time at which the transient clear obligation is eligible for retry.';
COMMENT ON COLUMN tier1_profile_clear_delete_obligations.created_at IS
    'Time at which the ordinary profile-clear retry obligation was recorded.';
