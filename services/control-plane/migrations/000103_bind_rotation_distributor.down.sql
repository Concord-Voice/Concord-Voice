-- Once a claim exists, no live downgrade is safe: re-applying this migration
-- would reopen a successor epoch to a different distributor. Restore a
-- pre-000103 backup instead; never clear claims manually.
LOCK TABLE key_revocations IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM key_revocations
        WHERE rotation_distributor_claimed IS TRUE
           OR rotation_distributor_id IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'refusing to drop rotation distributor claims; restore a pre-000103 backup to downgrade safely';
    END IF;
END
$$;

ALTER TABLE key_revocations
    DROP CONSTRAINT IF EXISTS key_revocations_rotation_distributor_sealed;

ALTER TABLE key_revocations
    DROP CONSTRAINT IF EXISTS key_revocations_rotation_distributor_id_fkey;

DROP INDEX IF EXISTS idx_key_revocations_rotation_distributor;

ALTER TABLE key_revocations
    DROP COLUMN IF EXISTS rotation_distributor_id,
    DROP COLUMN IF EXISTS rotation_distributor_claimed;
