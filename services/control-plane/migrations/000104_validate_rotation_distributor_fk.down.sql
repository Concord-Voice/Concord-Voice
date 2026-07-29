-- Keep the 000103 schema on rollback while returning the FK to its original
-- unvalidated state. 000103 down removes it after its claim-safety guard.
ALTER TABLE key_revocations
    DROP CONSTRAINT IF EXISTS key_revocations_rotation_distributor_id_fkey;

ALTER TABLE key_revocations
    ADD CONSTRAINT key_revocations_rotation_distributor_id_fkey
        FOREIGN KEY (rotation_distributor_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
