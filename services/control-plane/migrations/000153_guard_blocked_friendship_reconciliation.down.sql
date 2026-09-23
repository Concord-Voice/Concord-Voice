-- 000146 owns the durable evidence column and the complete strong fence. A
-- 000153 rollback must restore those definitions instead of silently leaving
-- the replacement trigger bodies installed.
LOCK TABLE public.users, public.friendships, public.dm_participants IN SHARE ROW EXCLUSIVE MODE;

DROP TRIGGER IF EXISTS require_blocked_friendship_reconciliation ON public.friendships;
DROP FUNCTION IF EXISTS public.require_blocked_friendship_reconciliation();
DROP TRIGGER IF EXISTS stamp_dm_block_reconciliation_transaction ON public.dm_block_reconciliations;
DROP FUNCTION IF EXISTS public.stamp_dm_block_reconciliation_transaction();

-- Older 000146 installations may not have the compatibility column. Keep
-- that rollback safe, while never dropping the column here: doing so would
-- discard durable reconciliation evidence owned by 000146. Migration 000146's
-- down migration remains the only operation permitted to remove that state,
-- and it refuses while evidence rows remain.
DO $$
BEGIN
    IF to_regclass('public.dm_block_reconciliations') IS NULL THEN
        RETURN;
    END IF;

    LOCK TABLE public.dm_block_reconciliations IN ACCESS EXCLUSIVE MODE;
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
        RETURN;
    END IF;

    EXECUTE $fn$
        CREATE FUNCTION public.stamp_dm_block_reconciliation_transaction()
        RETURNS trigger
        LANGUAGE plpgsql
        AS $body$
        BEGIN
            NEW.reconciliation_transaction_id := pg_current_xact_id();
            RETURN NEW;
        END;
        $body$
    $fn$;

    EXECUTE $trg$
        CREATE TRIGGER stamp_dm_block_reconciliation_transaction
            BEFORE INSERT OR UPDATE OF operation_id, remove_a, remove_b
            ON public.dm_block_reconciliations
            FOR EACH ROW
            EXECUTE FUNCTION public.stamp_dm_block_reconciliation_transaction()
    $trg$;

    EXECUTE $fn$
        CREATE FUNCTION public.require_blocked_friendship_reconciliation()
        RETURNS trigger
        LANGUAGE plpgsql
        AS $body$
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
        $body$
    $fn$;

    EXECUTE $trg$
        CREATE CONSTRAINT TRIGGER require_blocked_friendship_reconciliation
            AFTER INSERT OR UPDATE OF id, status, requester_id, addressee_id
            ON public.friendships
            DEFERRABLE INITIALLY DEFERRED
            FOR EACH ROW
            EXECUTE FUNCTION public.require_blocked_friendship_reconciliation()
    $trg$;
END
$$;
