-- Single-use, purpose-bound step-up tokens (#3455 / PR #3509).
--
-- Two kinds share the table, told apart by factor:
--   password  minted by POST /api/v1/auth/step-up/password, the only endpoint
--             an own-rule route's password reaches;
--   webauthn  minted by POST /api/v1/mfa/webauthn/verify-inline/finish, which
--             wrote them to Redis before this migration.
-- A consumer spends a token with one DELETE inside its own transaction, so a
-- rollback restores it. Only SHA-256(token) is stored; the token itself is
-- never written anywhere.
--
-- credential_epoch is users.credential_epoch at mint (NULL = never rotated). A
-- spend matches only while the user's current epoch is the same, so a
-- credential-epoch rotation (a password change, a key reset, an account
-- recovery) between mint and use strands the token. A token binds to the user
-- and the epoch, not to a session: revoking one session, or all of them,
-- without rotating the epoch leaves an outstanding token spendable for the
-- rest of its 60 s.
--
-- Growth is bounded by the writer, not the schema: every mint deletes the
-- user's expired rows and keeps at most 16 live rows per user, oldest dropped
-- first, and the hourly cleanup job deletes expired rows globally.
--
-- A brand-new, empty table: no backfill, and the indexes are built before any
-- writer exists, so a plain CREATE INDEX holds no lock anything waits on.
-- (golang-migrate sends each file as one implicit transaction, so
-- CONCURRENTLY was not available here anyway.)
-- The CHECKs pin what the writer already guarantees, so a bad row fails at the
-- write rather than as a token that can never match: token_hash is a SHA-256,
-- purpose names a route, and credential_epoch has credepoch.NewEpoch's format
-- (32 lowercase hex characters, migration 000151's check) or is NULL.
CREATE TABLE public.step_up_tokens (
    token_hash BYTEA PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    user_id UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    factor TEXT NOT NULL CHECK (factor IN ('password', 'webauthn')),
    purpose TEXT NOT NULL CHECK (purpose <> ''),
    credential_epoch TEXT CHECK (credential_epoch IS NULL OR credential_epoch ~ '^[0-9a-f]{32}$'),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_step_up_tokens_user_expires
    ON public.step_up_tokens (user_id, expires_at);

CREATE INDEX idx_step_up_tokens_expires
    ON public.step_up_tokens (expires_at);
