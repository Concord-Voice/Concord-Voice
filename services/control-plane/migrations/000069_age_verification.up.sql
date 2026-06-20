-- 000069_age_verification.up.sql
-- Identity-blind age-verification server foundation (#1623, child A of epic #272).
-- ADR-0025: the schema IS the privacy guarantee. This table stores ONLY booleans,
-- a jurisdiction-obligation integer (0..2), and signature metadata. There is NO
-- dob / age / birthdate / jurisdiction_name / ip / location column — by construction,
-- not by policy. A server that never receives a birthdate cannot leak one.

CREATE TABLE age_verification_records (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                 UUID NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    valid_age               BOOLEAN NOT NULL,
    nsfw_auth               BOOLEAN NOT NULL,
    jurisdiction_obligation INTEGER NOT NULL DEFAULT 0 CHECK (jurisdiction_obligation BETWEEN 0 AND 2),
    -- forward-compat columns: NOT accepted from the request in child A (left at defaults);
    -- populated + signed by children C/D/E/F. Downstream MUST treat as untrusted until then
    -- (the "stored-unsigned-then-trusted" trap is owned by child F, not opened here).
    obligation_sources      TEXT[]  NOT NULL DEFAULT '{}',
    confidence              TEXT    NOT NULL DEFAULT 'low',
    conflict_flag           BOOLEAN NOT NULL DEFAULT FALSE,
    assurance_signature     TEXT,
    assurance_provider      TEXT,
    assured_date            TIMESTAMPTZ,
    attestation_signature   TEXT,
    attestation_date        TIMESTAMPTZ,
    -- the client RSA-PSS signature the server verifies (audit / non-repudiation):
    client_signature        TEXT    NOT NULL,
    client_version          TEXT    NOT NULL,
    signature_key_version   INTEGER NOT NULL,
    canonical_version       INTEGER NOT NULL DEFAULT 1,
    last_change             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
    -- NO dob / age / birthdate / jurisdiction_name / ip / location column. Structural invariant.
);
-- UNIQUE(user_id) provides the single-row-per-user lookup index (one active row, upsert).

-- Soft-disable mechanism (D3): the valid_age=false enforcement path sets these. Distinct from
-- the legacy users.age_verified (000001, never enforced) and from the hard-erasure DeleteAccount path.
ALTER TABLE users ADD COLUMN disabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users ADD COLUMN disabled_reason TEXT;
ALTER TABLE users ADD COLUMN disabled_at TIMESTAMPTZ;

-- Partial index justified by the denylist REBUILD path (spec §4.6), which enumerates the
-- disabled set (SELECT id WHERE disabled=TRUE) on process start / Redis reconnect — NOT by
-- the gate queries (which filter disabled=FALSE). It indexes the small disabled set cheaply.
CREATE INDEX idx_users_disabled ON users(id) WHERE disabled = TRUE;
