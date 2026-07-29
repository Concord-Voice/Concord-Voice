-- Keep every batch of an issued channel-key rotation bound to its first
-- distributor. Without this durable claim, concurrent clients can wrap
-- different CSKs for disjoint 500-recipient batches at the same epoch.
ALTER TABLE key_revocations
    ADD COLUMN rotation_distributor_id UUID,
    ADD COLUMN rotation_distributor_claimed BOOLEAN;

COMMENT ON COLUMN key_revocations.rotation_distributor_id IS
    'User that claimed distribution of this successor epoch; cleared on erasure while the claim remains sealed.';
COMMENT ON COLUMN key_revocations.rotation_distributor_claimed IS
    'NULL for legacy or old-replica rotations (sealed); false only when a current writer explicitly creates an unclaimed rotation; true after a claim.';

-- Seal the new fields until 000105 installs the old-writer guard. Existing
-- replicas omit both columns and therefore remain compatible during rollout.
ALTER TABLE key_revocations
    ADD CONSTRAINT key_revocations_rotation_distributor_sealed
        CHECK (rotation_distributor_id IS NULL AND rotation_distributor_claimed IS NULL) NOT VALID;

-- golang-migrate runs this file transactionally, so CONCURRENTLY is not
-- available. The partial index only contains active distributor references.
CREATE INDEX idx_key_revocations_rotation_distributor
    ON key_revocations (rotation_distributor_id)
    WHERE rotation_distributor_id IS NOT NULL;

ALTER TABLE key_revocations
    ADD CONSTRAINT key_revocations_rotation_distributor_id_fkey
        FOREIGN KEY (rotation_distributor_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
