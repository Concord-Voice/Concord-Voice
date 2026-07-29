-- 000106 owns the recovery default for fresh installations. Retain it while
-- rolling back 000107 so 000106 remains safe for older replicas; 000106.down
-- removes it atomically with restoring the prior distributor guard.
ALTER TABLE key_revocations
    ALTER COLUMN rotation_distributor_claimed SET DEFAULT FALSE;
