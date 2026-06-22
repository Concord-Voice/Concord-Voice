-- 000066_client_attestation_registry.up.sql
-- Client attestation registry (#677): per-release hashes for verifying
-- official Concord Voice clients connecting to concordvoice.chat.
-- Two-axis: binary versions (vMAJOR.MINOR.PATCH) and SPA versions (sha7 commit
-- hash) tracked independently per ADR-0010.

CREATE TABLE release_binaries (
    version        TEXT NOT NULL,
    platform       TEXT NOT NULL CHECK (platform IN ('macos', 'windows', 'linux')),
    cert_hash      TEXT NOT NULL,
    published_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_by   TEXT NOT NULL,
    revoked_at     TIMESTAMPTZ,
    revoked_reason TEXT,
    revoked_by     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (version, platform)
);

CREATE INDEX idx_release_binaries_published_at ON release_binaries(published_at);
CREATE INDEX idx_release_binaries_revoked ON release_binaries(revoked_at) WHERE revoked_at IS NOT NULL;

CREATE TABLE release_spas (
    spa_version       TEXT PRIMARY KEY,
    html_hash         TEXT NOT NULL,
    published_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_by      TEXT NOT NULL,
    revoked_at        TIMESTAMPTZ,
    revoked_reason    TEXT,
    revoked_by        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_release_spas_published_at ON release_spas(published_at);
CREATE INDEX idx_release_spas_revoked ON release_spas(revoked_at) WHERE revoked_at IS NOT NULL;
