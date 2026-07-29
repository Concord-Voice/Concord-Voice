-- Re-seal the columns before removing the guard. The NULL-only check rejects
-- new-format rows after this transaction commits and remains until 000103 down
-- removes both the check and columns under its own table lock.
LOCK TABLE key_revocations IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM key_revocations
        WHERE rotation_distributor_claimed IS NOT NULL
           OR rotation_distributor_id IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'refusing to remove rotation distributor writer guard while current-format rotations exist';
    END IF;
END
$$;

ALTER TABLE key_revocations
    ADD CONSTRAINT key_revocations_rotation_distributor_sealed
        CHECK (rotation_distributor_id IS NULL AND rotation_distributor_claimed IS NULL) NOT VALID;

DROP TRIGGER IF EXISTS enforce_rotation_distributor_writer_before_channel_key_insert ON channel_keys;
DROP FUNCTION IF EXISTS enforce_rotation_distributor_writer();
