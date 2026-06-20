-- services/control-plane/migrations/000059_account_deletions_and_cascade_fix.down.sql

DROP INDEX IF EXISTS idx_account_deletions_deleted_at;
DROP TABLE IF EXISTS account_deletions;

-- Revert key_revocations.revoked_by to the original NO ACTION behavior.
-- NULL rows created during the up-migration's lifetime remain NULL and do
-- not violate the reverted constraint (NULL always satisfies a FK).
ALTER TABLE key_revocations
    DROP CONSTRAINT key_revocations_revoked_by_fkey,
    ADD CONSTRAINT key_revocations_revoked_by_fkey
        FOREIGN KEY (revoked_by) REFERENCES users(id);
