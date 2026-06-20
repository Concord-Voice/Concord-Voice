-- services/control-plane/migrations/000059_account_deletions_and_cascade_fix.up.sql

-- Part 1: Fix the FK cascade gap on key_revocations.revoked_by.
-- The original constraint (from 000028) used the default ON DELETE NO ACTION,
-- which blocks DELETE FROM users WHERE id = $1 for any user who has ever
-- recorded a channel-key rotation. Audit-style columns should hold NULL after
-- the actor is deleted.
ALTER TABLE key_revocations
    DROP CONSTRAINT key_revocations_revoked_by_fkey,
    ADD CONSTRAINT key_revocations_revoked_by_fkey
        FOREIGN KEY (revoked_by) REFERENCES users(id) ON DELETE SET NULL;

-- Part 2: account_deletions — durable, privacy-safe audit record.
-- user_id becomes NULL immediately after the user row is deleted (cascade
-- inside the same transaction), so the row preserves the deletion event and
-- timestamp without holding personal data.
CREATE TABLE account_deletions (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                  UUID REFERENCES users(id) ON DELETE SET NULL,
    deleted_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sentry_delete_attempted  BOOLEAN NOT NULL DEFAULT FALSE,
    metadata                 JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX idx_account_deletions_deleted_at
    ON account_deletions(deleted_at DESC);
