-- 000077_admin_auth.up.sql
-- Platform-admin authentication for the Admin/Operations console (#1688).
-- A separate admin identity, fully isolated from end-user accounts: password
-- (Argon2id) + mandatory WebAuthn/FIDO2 hardware key, with an append-only audit
-- log whose append-only property is ENFORCED by a restricted Postgres role
-- (concord_admin_rt) adopted per-transaction via SET LOCAL ROLE.
-- Spec: [internal]specs/2026-06-20-1688-admin-auth-design.md

CREATE TABLE admin_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending_enrollment'
                  CHECK (status IN ('pending_enrollment', 'active', 'disabled')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at   TIMESTAMPTZ
);

CREATE TABLE admin_webauthn_credentials (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_id        UUID NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    credential_id   BYTEA NOT NULL UNIQUE,
    public_key      BYTEA NOT NULL,
    aaguid          BYTEA NOT NULL,
    sign_count      BIGINT NOT NULL DEFAULT 0,
    credential_name TEXT,
    transports      TEXT[],
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ
);
CREATE INDEX idx_admin_webauthn_admin_id ON admin_webauthn_credentials (admin_id);

CREATE TABLE admin_audit_log (
    id         BIGSERIAL PRIMARY KEY,
    ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
    admin_id   UUID REFERENCES admin_users(id) ON DELETE SET NULL,
    actor      TEXT,
    event_type TEXT NOT NULL,
    result     TEXT NOT NULL CHECK (result IN ('success', 'failure', 'denied')),
    source_ref TEXT,
    detail     JSONB
);
CREATE INDEX idx_admin_audit_ts ON admin_audit_log (ts);

-- Append-only enforcement: a NOLOGIN role that may INSERT/SELECT the audit log
-- but NOT UPDATE/DELETE it. Admin write paths adopt this role per-transaction
-- via SET LOCAL ROLE concord_admin_rt, so an audit rewrite is denied by Postgres
-- itself (enforced, not merely documented). The role is created idempotently so
-- re-running against a DB that already has it (shared infra) is safe.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'concord_admin_rt') THEN
        CREATE ROLE concord_admin_rt NOLOGIN;
    END IF;
END
$$;

GRANT SELECT, INSERT, UPDATE, DELETE ON admin_users TO concord_admin_rt;
GRANT SELECT, INSERT, UPDATE, DELETE ON admin_webauthn_credentials TO concord_admin_rt;
GRANT USAGE, SELECT ON SEQUENCE admin_audit_log_id_seq TO concord_admin_rt;
GRANT SELECT, INSERT ON admin_audit_log TO concord_admin_rt;  -- NO UPDATE / DELETE

-- Grant the restricted role to the app/migration user so admin transactions can
-- adopt it via SET LOCAL ROLE. current_user is the DB user running migrations
-- (the same user the control-plane connects as).
GRANT concord_admin_rt TO current_user;
