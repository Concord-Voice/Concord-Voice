-- Per-user notification mute preferences for servers, channels, and DMs.
-- Closes #84 (Mute & Notification Preferences).
--
-- target_type is a discriminator over heterogeneous targets (server, channel,
-- dm conversation), so a real FK on target_id is not possible without a
-- separate row per type. The application is responsible for cleaning up rows
-- when the underlying target is deleted; orphaned rows are harmless (they
-- simply never match anything).
--
-- The composite primary key (user_id, target_type, target_id) gives us at
-- most one preference row per user per target — set/clear is an UPSERT.
--
-- `muted = false` is a meaningful state, NOT just the absence of a row:
-- it expresses "user explicitly unmuted this channel even though its server
-- is muted." Resolution order in the client is channel-pref > server-pref >
-- default (unmuted).
--
-- `muted_until` semantics:
--   NULL     — mute is indefinite (until manually toggled off)
--   NOT NULL — mute expires at that timestamp; the client enforces expiry
--              (server stores the value but does not filter notify events).

CREATE TABLE notification_preferences (
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_type VARCHAR(10) NOT NULL,
    target_id   UUID        NOT NULL,
    muted       BOOLEAN     NOT NULL DEFAULT TRUE,
    muted_until TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, target_type, target_id),
    CONSTRAINT notification_preferences_target_type_check
        CHECK (target_type IN ('server', 'channel', 'dm'))
);

-- The PK already indexes (user_id, target_type, target_id), so a prefix
-- scan on (user_id) or (user_id, target_type) is free — no extra index
-- needed for the hydration endpoint (`GET /notifications/preferences`)
-- nor the batch endpoint (`GET /servers/:id/mute-states`).
