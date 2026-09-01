-- Durable key-only obligations for retrying Tier-1 erasure object deletes.
-- This table intentionally has no user foreign key so obligations survive
-- deletion of the user that created them.
CREATE TABLE tier1_erasure_delete_obligations (
    storage_key     VARCHAR(500) PRIMARY KEY,
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    reconcile_after TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX idx_tier1_erasure_delete_obligations_due
    ON tier1_erasure_delete_obligations (reconcile_after);

COMMENT ON TABLE tier1_erasure_delete_obligations IS
    'Durable key-only obligations for retrying Tier-1 erasure object deletion after the user row is removed.';
COMMENT ON COLUMN tier1_erasure_delete_obligations.storage_key IS
    'Exact deterministic Tier-1 object key requiring deletion.';
COMMENT ON COLUMN tier1_erasure_delete_obligations.attempts IS
    'Number of reconciliation attempts; never negative.';
COMMENT ON COLUMN tier1_erasure_delete_obligations.reconcile_after IS
    'Earliest time at which the obligation is eligible for reconciliation.';
COMMENT ON COLUMN tier1_erasure_delete_obligations.created_at IS
    'Time at which the durable deletion obligation was recorded.';
