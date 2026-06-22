-- 000076_redemption_code_issuance.up.sql
-- Platform-level audit trail for redemption-code GENERATION (#1303, epic #1294).
-- Every issue/batch (CLI or admin HTTP) writes exactly one row here IN THE SAME
-- TRANSACTION as the minted redemption_codes rows, so a code can never exist
-- without its generation being audited (design spec §7). This is distinct from
-- the server-scoped audit_log table (000035), which requires a server_id and is
-- for per-server RBAC actions — code issuance is a platform-wide event with no
-- server context.
--
-- PRIVACY: this table records WHO issued, WHAT kind, HOW MANY, WHICH batch, and
-- WHEN — never the code plaintext or its SHA-256 hash (those are write-only at
-- rest in redemption_codes; an audit row must not become a second copy of the
-- secret surface). Net-new table — DROP is safe in the paired down.
CREATE TABLE redemption_code_issuance (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The issuer's user id. NULL for the CLI/operator-on-the-box path (no user
    -- identity in a shell session); the issuer_context column records the
    -- channel ('cli' | 'admin-http') so a NULL issuer_id is not ambiguous.
    issuer_id        UUID REFERENCES users(id) ON DELETE SET NULL,
    issuer_context   VARCHAR(32) NOT NULL,        -- 'cli' | 'admin-http'
    grant_kind       VARCHAR(64) NOT NULL,        -- catalog key issued (no params/secret)
    code_count       INT NOT NULL CHECK (code_count >= 1),
    batch_id         VARCHAR(64),                 -- shared campaign label (NULL for ad-hoc)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Audit queries are time-ordered and batch-filtered.
CREATE INDEX idx_redemption_issuance_created ON redemption_code_issuance(created_at DESC);
CREATE INDEX idx_redemption_issuance_batch
    ON redemption_code_issuance(batch_id)
    WHERE batch_id IS NOT NULL;
