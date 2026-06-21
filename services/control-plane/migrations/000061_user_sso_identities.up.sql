-- SSO identity foundation: one row per (Concord user, identity provider) link.
-- Closes #270 (Google SSO); foundation for #271 (Apple SSO).

CREATE TABLE user_sso_identities (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider          VARCHAR(32) NOT NULL,
    provider_user_id  VARCHAR(255) NOT NULL,
    provider_email    VARCHAR(255) NOT NULL,
    is_relay_email    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at      TIMESTAMPTZ,
    -- Hard 1:1 link between a Concord user and any single provider. Allowing
    -- multiple identities for the same (user, provider) would silently double
    -- up the user's auth surface — the app surfaces "linked providers" as a
    -- set, and the unlink path would only remove one of N rows. The provider
    -- itself only ever issues one provider_user_id per Concord user, so this
    -- is the natural cardinality.
    UNIQUE (user_id, provider),
    -- Defends against a different attack: an attacker who hijacks a single
    -- Google account cannot pivot it onto two different Concord users.
    UNIQUE (provider, provider_user_id)
);

CREATE INDEX idx_user_sso_identities_user_id ON user_sso_identities(user_id);

ALTER TABLE users
    ADD COLUMN password_login_disabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN trust_sso_security      BOOLEAN NOT NULL DEFAULT FALSE;
