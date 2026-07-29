-- Validate the FK added NOT VALID in 000103 after its short schema-change
-- transaction. Existing rows are NULL, so no historical distributor is read.
ALTER TABLE key_revocations
    VALIDATE CONSTRAINT key_revocations_rotation_distributor_id_fkey;
