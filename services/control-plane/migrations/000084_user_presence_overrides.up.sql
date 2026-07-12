-- Rich Presence recipient exceptions (#1234).
-- The server stores the opaque category-local preference document separately
-- from the materialized user IDs required for enforcement.
CREATE TABLE presence_override_preferences (
    user_id        UUID        NOT NULL,
    category       TEXT        NOT NULL,
    encrypted_data TEXT        NOT NULL,
    version        INTEGER     NOT NULL DEFAULT 1,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT presence_override_preferences_pkey
        PRIMARY KEY (user_id, category),
    CONSTRAINT presence_override_preferences_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT presence_override_preferences_category_check
        CHECK (category = 'custom_text'),
    CONSTRAINT presence_override_preferences_version_positive
        CHECK (version > 0)
);

CREATE TABLE user_presence_overrides (
    sender_id     UUID        NOT NULL,
    category      TEXT        NOT NULL,
    target_user_id UUID       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT user_presence_overrides_pkey
        PRIMARY KEY (sender_id, category, target_user_id),
    CONSTRAINT user_presence_overrides_sender_id_fkey
        FOREIGN KEY (sender_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT user_presence_overrides_target_user_id_fkey
        FOREIGN KEY (target_user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT user_presence_overrides_preference_fkey
        FOREIGN KEY (sender_id, category)
        REFERENCES presence_override_preferences(user_id, category)
        ON DELETE CASCADE,
    CONSTRAINT user_presence_overrides_category_check
        CHECK (category = 'custom_text'),
    CONSTRAINT user_presence_overrides_not_self_check
        CHECK (sender_id <> target_user_id)
);

CREATE INDEX idx_user_presence_overrides_target
    ON user_presence_overrides (target_user_id);
