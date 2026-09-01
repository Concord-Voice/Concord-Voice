-- Refuse to discard permanent Tier-1 erasure tombstones during rollback.
DO $$
DECLARE
    has_tombstones BOOLEAN;
BEGIN
    IF to_regclass('public.tier1_erasure_delete_obligations') IS NULL THEN
        RETURN;
    END IF;

    -- Serialize the emptiness check with inserts and the column teardown.
    BEGIN
        EXECUTE
            'LOCK TABLE public.tier1_erasure_delete_obligations IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            -- A concurrent/idempotent rollback may have removed it while this
            -- rollback waited to acquire the lock.
            RETURN;
    END;

    EXECUTE
        'SELECT EXISTS (SELECT 1 FROM public.tier1_erasure_delete_obligations)'
        INTO has_tombstones;
    IF has_tombstones THEN
        RAISE EXCEPTION
            'cannot remove Tier-1 erasure tombstone metadata while tombstone rows remain';
    END IF;

    ALTER TABLE public.tier1_erasure_delete_obligations
        DROP COLUMN IF EXISTS last_delete_at;

    COMMENT ON TABLE public.tier1_erasure_delete_obligations IS
        'Durable key-only obligations for retrying Tier-1 erasure object deletion after the user row is removed.';
    COMMENT ON COLUMN public.tier1_erasure_delete_obligations.storage_key IS
        'Exact deterministic Tier-1 object key requiring deletion.';
END
$$;
