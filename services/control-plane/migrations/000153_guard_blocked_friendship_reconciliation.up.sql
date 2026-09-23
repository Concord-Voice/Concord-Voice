LOCK TABLE public.friendships IN SHARE ROW EXCLUSIVE MODE;

-- An old 000146 schema has no transaction stamp. Its blocked rows cannot be
-- repaired safely because the marker direction may be stale, so fail closed
-- before mutating that schema.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_attribute a
        JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
        JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relname = 'dm_block_reconciliations'
          AND a.attname = 'reconciliation_transaction_id'
          AND NOT a.attisdropped
    ) AND EXISTS (
        SELECT 1
        FROM public.friendships
        WHERE status = 'blocked'
    ) THEN
        RAISE EXCEPTION
            'cannot retrofit reconciliation stamps while blocked friendships exist; resolve or unblock ambiguous rows, recover the dirty migration to verified-good 152, and retry 153; re-establish required blocks through a current writer only after 153 succeeds'
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

ALTER TABLE public.dm_block_reconciliations
    ADD COLUMN IF NOT EXISTS reconciliation_transaction_id xid8;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_attribute a
        JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
        JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relname = 'dm_block_reconciliations'
          AND a.attname = 'reconciliation_transaction_id'
          AND NOT a.attisdropped
          AND a.atttypid = 'xid8'::regtype
    ) THEN
        RAISE EXCEPTION
            'dm_block_reconciliations.reconciliation_transaction_id must use xid8'
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

COMMENT ON COLUMN public.dm_block_reconciliations.reconciliation_transaction_id IS
    'Top-level transaction that most recently recorded directional block reconciliation evidence.';

CREATE OR REPLACE FUNCTION public.stamp_dm_block_reconciliation_transaction()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.reconciliation_transaction_id := pg_current_xact_id();
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS stamp_dm_block_reconciliation_transaction ON public.dm_block_reconciliations;
CREATE TRIGGER stamp_dm_block_reconciliation_transaction
    BEFORE INSERT OR UPDATE OF operation_id, remove_a, remove_b
    ON public.dm_block_reconciliations
    FOR EACH ROW
    EXECUTE FUNCTION public.stamp_dm_block_reconciliation_transaction();

CREATE OR REPLACE FUNCTION public.require_blocked_friendship_reconciliation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    pair_a UUID;
    pair_b UUID;
    final_requester UUID;
    final_addressee UUID;
    final_status VARCHAR(20);
BEGIN
    SELECT requester_id, addressee_id, status
    INTO final_requester, final_addressee, final_status
    FROM public.friendships
    WHERE id = NEW.id;

    IF NOT FOUND OR final_status IS DISTINCT FROM 'blocked' THEN
        RETURN NULL;
    END IF;

    pair_a := LEAST(final_requester, final_addressee);
    pair_b := GREATEST(final_requester, final_addressee);

    IF NOT EXISTS (
        SELECT 1
        FROM public.dm_block_reconciliations
        WHERE user_a_id = pair_a
          AND user_b_id = pair_b
          AND reconciliation_transaction_id = pg_current_xact_id()
    ) THEN
        RAISE EXCEPTION 'blocked friendship requires current reconciliation evidence'
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS require_blocked_friendship_reconciliation ON public.friendships;
CREATE CONSTRAINT TRIGGER require_blocked_friendship_reconciliation
    AFTER INSERT OR UPDATE OF id, status, requester_id, addressee_id
    ON public.friendships
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION public.require_blocked_friendship_reconciliation();
