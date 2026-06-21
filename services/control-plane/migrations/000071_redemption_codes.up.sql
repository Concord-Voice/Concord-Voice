-- 000071_redemption_codes.up.sql
-- Universal redemption-code registry (random+registry model; epic #1294, child #1295).
-- The code carries NO meaning; this row holds all grant semantics. Codes are stored as
-- SHA-256 hash only (plaintext returned once at issue). The redemption ENGINE (/redeem,
-- hashing, grant catalog, issuer CLI) is #1303 — this migration only creates the table.
CREATE TABLE redemption_codes (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code_hash         TEXT NOT NULL UNIQUE,         -- SHA-256 of the opaque code
    code_prefix       VARCHAR(16),                  -- non-secret support handle ('KS','PROMO')
    grant_kind        VARCHAR(64) NOT NULL,         -- key into the grant-effect catalog (#1303)
    grant_params      JSONB NOT NULL DEFAULT '{}',
    single_use        BOOLEAN NOT NULL DEFAULT TRUE,
    max_redemptions   INT,                          -- NULL = unlimited (promo); 1 = one-off
    redemption_count  INT NOT NULL DEFAULT 0,
    valid_from        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at        TIMESTAMPTZ,
    batch_id          VARCHAR(64),
    metadata          JSONB NOT NULL DEFAULT '{}',
    created_by        UUID REFERENCES users(id) ON DELETE SET NULL,
    revoked_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Partial: batch_id is NULL for ad-hoc/promo codes; only batch-issued codes (audit/bulk
-- ops by batch_id) are ever looked up by this index.
CREATE INDEX idx_redemption_codes_batch
    ON redemption_codes(batch_id)
    WHERE batch_id IS NOT NULL;
