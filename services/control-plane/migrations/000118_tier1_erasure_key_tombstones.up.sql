-- Make Tier-1 erasure obligations permanent key-reuse tombstones.
-- Existing 000117 rows are retained; last_delete_at is metadata only and is
-- intentionally not backfilled.
ALTER TABLE tier1_erasure_delete_obligations
    ADD COLUMN IF NOT EXISTS last_delete_at TIMESTAMPTZ;

COMMENT ON TABLE tier1_erasure_delete_obligations IS
    'Permanent Tier-1 erasure key tombstones for avatar and banner objects. Row existence permanently denies key reuse; a successful DeleteObject is not terminal acknowledgement.';
COMMENT ON COLUMN tier1_erasure_delete_obligations.storage_key IS
    'Exact deterministic Tier-1 avatar or banner object key. A surviving row permanently denies key reuse, even after a successful DeleteObject.';
COMMENT ON COLUMN tier1_erasure_delete_obligations.last_delete_at IS
    'Timestamp of the most recent successful DeleteObject attempt, when known. Metadata only: it does not remove the tombstone or permit key reuse.';
